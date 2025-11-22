package acudp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/text/encoding/unicode/utf32"
)

const (
	// DefaultRealtimePosIntervalMs is the default interval to request real time positional information.
	DefaultRealtimePosIntervalMs = -1

	readBufferSize = 1024
	chanBufferSize = 1000
)

// ServerConfig holds the configuration for the AssettoServerUDP.
type ServerConfig struct {
	Addr              string
	ReceivePort       int
	SendPort          int
	Forward           bool
	ForwardAddrStr    string
	ForwardListenPort int

	// RealtimePosIntervalMs is the interval to request real time positional information.
	// Set this to greater than 0 to enable.
	RealtimePosIntervalMs      int
	PosIntervalModifierEnabled bool
}

// CallbackFunc is the function signature for message callbacks.
type CallbackFunc func(response Message)

// AssettoServerUDP handles the UDP connection to the Assetto Corsa server.
type AssettoServerUDP struct {
	listener  *net.UDPConn
	forwarder *net.UDPConn

	config ServerConfig

	cfn      func()
	ctx      context.Context
	callback CallbackFunc

	closed bool

	// State for realtime pos interval adjustment
	currentRealtimePosIntervalMs int
}

// NewServerClient creates a new AssettoServerUDP client.
func NewServerClient(config ServerConfig, callback CallbackFunc) (*AssettoServerUDP, error) {
	listener, err := net.DialUDP("udp",
		&net.UDPAddr{IP: net.ParseIP(config.Addr), Port: config.ReceivePort},
		&net.UDPAddr{IP: net.ParseIP(config.Addr), Port: config.SendPort},
	)

	if err != nil {
		return nil, err
	}

	if runtime.GOOS != "darwin" {
		if err := listener.SetReadBuffer(1e8); err != nil {
			logrus.WithError(err).Error("unable to set read buffer")
		}
	}

	ctx, cfn := context.WithCancel(context.Background())

	u := &AssettoServerUDP{
		ctx:                          ctx,
		cfn:                          cfn,
		callback:                     callback,
		config:                       config,
		listener:                     listener,
		currentRealtimePosIntervalMs: config.RealtimePosIntervalMs,
	}

	if config.Forward && config.ForwardAddrStr != "" && config.ForwardListenPort != 0 {
		forwardAddr, err := net.ResolveUDPAddr("udp", config.ForwardAddrStr)

		if err != nil {
			return nil, err
		}

		u.forwarder, err = net.DialUDP("udp",
			&net.UDPAddr{IP: net.ParseIP(config.Addr), Port: config.ForwardListenPort},
			forwardAddr,
		)

		if err != nil {
			return nil, err
		}
	}

	go u.serve()
	go u.forwardServe()
	logrus.Debugf("Started new UDP server connection")

	return u, nil
}

func (asu *AssettoServerUDP) Close() error {
	if asu.closed {
		return nil
	}

	defer func() {
		asu.closed = true
	}()

	asu.cfn()
	err := asu.listener.Close()
	if err != nil {
		return err
	}

	if asu.forwarder != nil {
		err = asu.forwarder.Close()

		if err != nil {
			return err
		}
	}

	logrus.Debugf("Closed UDP server connection")
	return nil
}

func (asu *AssettoServerUDP) forwardServe() {
	if !asu.config.Forward || asu.forwarder == nil {
		return
	}

	for {
		select {
		case <-asu.ctx.Done():
			asu.forwarder.Close()
			return
		default:
			buf := make([]byte, readBufferSize)
			n, _, err := asu.forwarder.ReadFromUDP(buf)
			if err != nil {
				continue
			}

			_, err = asu.listener.Write(buf[:n])
			if err != nil {
				continue
			}
		}
	}
}

func (asu *AssettoServerUDP) serve() {
	messageChan := make(chan []byte, chanBufferSize)
	defer close(messageChan)

	asu.currentRealtimePosIntervalMs = asu.config.RealtimePosIntervalMs
	lastQueueSize := 0

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case buf := <-messageChan:
				msg, err := asu.handleMessage(bytes.NewReader(buf))

				if err != nil {
					logrus.WithError(err).Error("could not handle UDP message")
					return
				}

				asu.callback(msg)

				if asu.config.Forward && asu.forwarder != nil {
					// write the message to the forwarding address
					_, _ = asu.forwarder.Write(buf)
				}
			case <-ticker.C:
				asu.adjustRealtimePosInterval(&lastQueueSize, len(messageChan))
			case <-asu.ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-asu.ctx.Done():
			asu.listener.Close()
			return
		default:
			buf := make([]byte, readBufferSize)

			// read message from assetto
			n, _, err := asu.listener.ReadFromUDP(buf)

			if err != nil {
				// Check if it's a temporary error or closed connection
				if errors.Is(err, net.ErrClosed) {
					return
				}
				logrus.WithError(err).Debug("could not read from UDP")
				continue
			}

			select {
			case messageChan <- buf[:n]:
			default:
				logrus.Warn("Message channel full, dropping message")
			}
		}
	}
}

func (asu *AssettoServerUDP) adjustRealtimePosInterval(lastQueueSize *int, currentQueueSize int) {
	if asu.config.RealtimePosIntervalMs < 0 || !asu.config.PosIntervalModifierEnabled {
		return
	}

	if currentQueueSize > *lastQueueSize {
		logrus.Warnf("Can't keep up! queue size: %d vs %d: changed by %d", currentQueueSize, *lastQueueSize, currentQueueSize-*lastQueueSize)

		// update as infrequently as we can, within sensible limits
		if currentQueueSize > 5 { // at this point we are half a second behind
			asu.currentRealtimePosIntervalMs += (currentQueueSize * 2) + 1

			logrus.Debugf("Adjusting real time pos interval: %d", asu.currentRealtimePosIntervalMs)
			err := asu.SendMessage(NewEnableRealtimePosInterval(asu.currentRealtimePosIntervalMs))

			if err != nil {
				logrus.WithError(err).Error("Could not send realtime pos interval adjustment")
			}
		}
	} else if currentQueueSize <= *lastQueueSize && currentQueueSize < 5 && asu.currentRealtimePosIntervalMs > asu.config.RealtimePosIntervalMs {
		logrus.Debugf("Catching up, queue size: %d vs %d: changed by %d", currentQueueSize, *lastQueueSize, currentQueueSize-*lastQueueSize)

		if asu.currentRealtimePosIntervalMs-1 >= asu.config.RealtimePosIntervalMs {
			asu.currentRealtimePosIntervalMs--

			logrus.Debugf("Adjusting real time pos interval, is now: %d", asu.currentRealtimePosIntervalMs)
			err := asu.SendMessage(NewEnableRealtimePosInterval(asu.currentRealtimePosIntervalMs))

			if err != nil {
				logrus.WithError(err).Error("Could not send realtime pos interval adjustment")
			}
		}
	}

	*lastQueueSize = currentQueueSize
}

func readStringW(r io.Reader) string {
	return readString(r, 4)
}

func readString(r io.Reader, sizeMultiplier int) string {
	var size uint8
	err := binary.Read(r, binary.LittleEndian, &size)

	if err != nil {
		return ""
	}

	b := make([]byte, int(size)*sizeMultiplier)

	err = binary.Read(r, binary.LittleEndian, &b)

	if err != nil {
		return ""
	}

	if sizeMultiplier == 4 {
		bs, err := utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM).NewDecoder().Bytes(b)

		if err != nil {
			return ""
		}

		return string(bs)
	}

	return string(b)
}

func (asu *AssettoServerUDP) SendMessage(message Message) error {
	switch a := message.(type) {
	case EnableRealtimePosInterval:
		if asu.config.PosIntervalModifierEnabled {
			return binary.Write(asu.listener, binary.LittleEndian, a)
		}
		return nil

	case GetSessionInfo, *RestartSession, *NextSession:
		return binary.Write(asu.listener, binary.LittleEndian, a.Event())

	case *SendChat:
		return asu.writeMessage(a.EventType, a.CarID, a.Len, a.UTF32Encoded)

	case *BroadcastChat:
		return asu.writeMessage(a.EventType, a.Len, a.UTF32Encoded)

	case *AdminCommand:
		return asu.writeMessage(a.EventType, a.Len, a.UTF32Encoded)

	case *KickUser:
		return asu.writeMessage(a.EventType, a.CarID)
	}

	return errors.New("udp: invalid message type")
}

func (asu *AssettoServerUDP) writeMessage(data ...interface{}) error {
	buf := new(bytes.Buffer)
	for _, v := range data {
		if err := binary.Write(buf, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	_, err := io.Copy(asu.listener, buf)
	return err
}

func (asu *AssettoServerUDP) handleMessage(r io.Reader) (Message, error) {
	var messageType uint8

	err := binary.Read(r, binary.LittleEndian, &messageType)

	if err != nil {
		return nil, err
	}

	eventType := Event(messageType)

	switch eventType {
	case EventNewConnection, EventConnectionClosed:
		return asu.handleConnectionEvent(r, eventType)

	case EventCarUpdate:
		return asu.handleCarUpdate(r)

	case EventCarInfo:
		return asu.handleCarInfo(r)

	case EventEndSession:
		return EndSession(readStringW(r)), nil

	case EventVersion:
		var version uint8
		if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
			return nil, err
		}
		return Version(version), nil

	case EventChat:
		return asu.handleChat(r)

	case EventClientLoaded:
		var carID CarID
		if err := binary.Read(r, binary.LittleEndian, &carID); err != nil {
			return nil, err
		}
		return ClientLoaded(carID), nil

	case EventNewSession, EventSessionInfo:
		return asu.handleSessionInfo(r, eventType)

	case EventError:
		return ServerError{errors.New(readStringW(r))}, nil

	case EventLapCompleted:
		return asu.handleLapCompleted(r)

	case EventClientEvent:
		return asu.handleClientEvent(r)

	default:
		return nil, fmt.Errorf("unknown response type: %d", eventType)
	}
}

func (asu *AssettoServerUDP) handleConnectionEvent(r io.Reader, eventType Event) (Message, error) {
	driverName := readStringW(r)
	driverGUID := readStringW(r)

	var carID CarID
	if err := binary.Read(r, binary.LittleEndian, &carID); err != nil {
		return nil, err
	}

	carMode := readString(r, 1)
	carSkin := readString(r, 1)

	return SessionCarInfo{
		CarID:      carID,
		DriverName: driverName,
		DriverGUID: DriverGUID(driverGUID),
		CarModel:   carMode,
		CarSkin:    carSkin,
		EventType:  eventType,
	}, nil
}

func (asu *AssettoServerUDP) handleCarUpdate(r io.Reader) (Message, error) {
	carUpdate := CarUpdate{}
	if err := binary.Read(r, binary.LittleEndian, &carUpdate); err != nil {
		return nil, err
	}
	return carUpdate, nil
}

func (asu *AssettoServerUDP) handleCarInfo(r io.Reader) (Message, error) {
	var carID CarID
	if err := binary.Read(r, binary.LittleEndian, &carID); err != nil {
		return nil, err
	}

	var isConnected uint8
	if err := binary.Read(r, binary.LittleEndian, &isConnected); err != nil {
		return nil, err
	}

	return CarInfo{
		CarID:       carID,
		IsConnected: isConnected != 0,
		CarModel:    readStringW(r),
		CarSkin:     readStringW(r),
		DriverName:  readStringW(r),
		DriverTeam:  readStringW(r),
		DriverGUID:  DriverGUID(readStringW(r)),
	}, nil
}

func (asu *AssettoServerUDP) handleChat(r io.Reader) (Message, error) {
	var carID CarID
	if err := binary.Read(r, binary.LittleEndian, &carID); err != nil {
		return nil, err
	}
	return Chat{
		CarID:   carID,
		Message: readStringW(r),
	}, nil
}

func (asu *AssettoServerUDP) handleSessionInfo(r io.Reader, eventType Event) (Message, error) {
	sessionInfo := SessionInfo{}

	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.Version); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.SessionIndex); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.CurrentSessionIndex); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.SessionCount); err != nil {
		return nil, err
	}

	sessionInfo.ServerName = readStringW(r)
	sessionInfo.Track = readString(r, 1)
	sessionInfo.TrackConfig = readString(r, 1)
	sessionInfo.Name = readString(r, 1)

	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.Type); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.Time); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.Laps); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.WaitTime); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.AmbientTemp); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.RoadTemp); err != nil {
		return nil, err
	}

	sessionInfo.WeatherGraphics = readString(r, 1)

	if err := binary.Read(r, binary.LittleEndian, &sessionInfo.ElapsedMilliseconds); err != nil {
		return nil, err
	}

	sessionInfo.EventType = eventType

	if asu.config.RealtimePosIntervalMs > 0 && eventType == EventNewSession {
		if err := asu.SendMessage(NewEnableRealtimePosInterval(asu.config.RealtimePosIntervalMs)); err != nil {
			return nil, err
		}
	}

	return sessionInfo, nil
}

func (asu *AssettoServerUDP) handleLapCompleted(r io.Reader) (Message, error) {
	lapCompleted := lapCompletedInternal{}
	if err := binary.Read(r, binary.LittleEndian, &lapCompleted); err != nil {
		return nil, err
	}

	lc := LapCompleted{
		CarID:     lapCompleted.CarID,
		LapTime:   lapCompleted.LapTime,
		Cuts:      lapCompleted.Cuts,
		CarsCount: lapCompleted.CarsCount,
	}

	for i := uint8(0); i < lapCompleted.CarsCount; i++ {
		var car LapCompletedCar
		if err := binary.Read(r, binary.LittleEndian, &car); err != nil {
			return nil, err
		}
		lc.Cars = append(lc.Cars, &car)
	}

	return lc, nil
}

func (asu *AssettoServerUDP) handleClientEvent(r io.Reader) (Message, error) {
	var collisionType uint8
	if err := binary.Read(r, binary.LittleEndian, &collisionType); err != nil {
		return nil, err
	}

	switch Event(collisionType) {
	case EventCollisionWithCar:
		collision := CollisionWithCar{}
		if err := binary.Read(r, binary.LittleEndian, &collision); err != nil {
			return nil, err
		}
		return collision, nil

	case EventCollisionWithEnv:
		collision := CollisionWithEnvironment{}
		if err := binary.Read(r, binary.LittleEndian, &collision); err != nil {
			return nil, err
		}
		return collision, nil
	}

	return nil, nil
}

type lapCompletedInternal struct {
	CarID     CarID
	LapTime   uint32
	Cuts      uint8
	CarsCount uint8
}
