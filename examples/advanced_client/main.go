package main

import (
	"fmt"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xsfunc/acudp"
)

// CarState tracks the state of each car
type CarState struct {
	CarID      acudp.CarID
	DriverName string
	DriverGUID acudp.DriverGUID
	CarModel   string
	CarSkin    string
	DriverTeam string

	// Real-time telemetry
	Position            acudp.Vec
	Velocity            acudp.Vec
	Speed               float32 // km/h
	MaxSpeed            float32
	Gear                uint8
	EngineRPM           uint16
	NormalisedSplinePos float32

	// Statistics
	LapCount       int
	TotalLapTime   uint32
	LastLapTime    uint32
	CollisionCount int
	LastUpdateTime time.Time
}

// CarTracker manages all car states
type CarTracker struct {
	mu   sync.RWMutex
	cars map[acudp.CarID]*CarState
}

func NewCarTracker() *CarTracker {
	return &CarTracker{
		cars: make(map[acudp.CarID]*CarState),
	}
}

func (ct *CarTracker) GetOrCreate(carID acudp.CarID) *CarState {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if car, exists := ct.cars[carID]; exists {
		return car
	}

	car := &CarState{
		CarID:          carID,
		LastUpdateTime: time.Now(),
	}
	ct.cars[carID] = car
	return car
}

func (ct *CarTracker) Get(carID acudp.CarID) (*CarState, bool) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	car, exists := ct.cars[carID]
	return car, exists
}

func (ct *CarTracker) GetAll() []*CarState {
	ct.mu.RLock()
	defer ct.mu.RUnlock()

	cars := make([]*CarState, 0, len(ct.cars))
	for _, car := range ct.cars {
		cars = append(cars, car)
	}
	return cars
}

// calculateSpeed calculates speed in km/h from velocity vector
func calculateSpeed(velocity acudp.Vec) float32 {
	// Speed in m/s
	speedMS := float32(math.Sqrt(float64(velocity.X*velocity.X + velocity.Y*velocity.Y + velocity.Z*velocity.Z)))
	// Convert to km/h
	return speedMS * 3.6
}

func main() {
	// Configure the logger - set to DebugLevel to see all events
	logrus.SetLevel(logrus.DebugLevel)
	// Create car tracker
	tracker := NewCarTracker()
	// Define the server configuration
	config := acudp.ServerConfig{
		Addr:        "127.0.0.1",
		ReceivePort: 12000,
		SendPort:    11000,
		// Request updates every 100ms (10Hz) - good balance between detail and performance
		RealtimePosIntervalMs: 100,
	}

	// Define the callback function to handle incoming messages
	callback := func(msg acudp.Message) {
		// Debug: log all incoming message types
		logrus.Debugf("Received message type: %T", msg)

		switch m := msg.(type) {
		case acudp.CarUpdate:
			car := tracker.GetOrCreate(m.CarID)
			car.Position = m.Pos
			car.Velocity = m.Velocity
			car.Gear = m.Gear
			car.EngineRPM = m.EngineRPM
			car.NormalisedSplinePos = m.NormalisedSplinePos
			car.Speed = calculateSpeed(m.Velocity)
			car.LastUpdateTime = time.Now()

			if car.Speed > car.MaxSpeed {
				car.MaxSpeed = car.Speed
			}

			// Print detailed telemetry
			fmt.Printf("\n┌─ Car #%d ─────────────────────────────────────────────────\n", m.CarID)
			if car.DriverName != "" {
				fmt.Printf("│ Driver: %s\n", car.DriverName)
			}
			fmt.Printf("│ Position:  X=%.2f  Y=%.2f  Z=%.2f\n", m.Pos.X, m.Pos.Y, m.Pos.Z)
			fmt.Printf("│ Velocity:  X=%.2f  Y=%.2f  Z=%.2f\n", m.Velocity.X, m.Velocity.Y, m.Velocity.Z)
			fmt.Printf("│ Speed:     %.1f km/h  (Max: %.1f km/h)\n", car.Speed, car.MaxSpeed)
			fmt.Printf("│ Gear:      %d\n", m.Gear)
			fmt.Printf("│ RPM:       %d\n", m.EngineRPM)
			fmt.Printf("│ Track Pos: %.2f%% (NSP)\n", m.NormalisedSplinePos*100)
			if car.LapCount > 0 {
				fmt.Printf("│ Laps:      %d\n", car.LapCount)
				if car.LastLapTime > 0 {
					fmt.Printf("│ Last Lap:  %s\n", formatLapTime(car.LastLapTime))
				}
			}
			fmt.Printf("└────────────────────────────────────────────────────────────\n")

		case acudp.CarInfo:
			car := tracker.GetOrCreate(m.CarID)
			car.DriverName = m.DriverName
			car.DriverGUID = m.DriverGUID
			car.CarModel = m.CarModel
			car.CarSkin = m.CarSkin
			car.DriverTeam = m.DriverTeam

			fmt.Printf("\n╔═══════════════════════════════════════════════════════════╗\n")
			fmt.Printf("║ 🏎️  CAR INFO - Car #%d\n", m.CarID)
			fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
			fmt.Printf("║ Driver:    %s\n", m.DriverName)
			if m.DriverTeam != "" {
				fmt.Printf("║ Team:      %s\n", m.DriverTeam)
			}
			fmt.Printf("║ GUID:      %s\n", m.DriverGUID)
			fmt.Printf("║ Car Model: %s\n", m.CarModel)
			fmt.Printf("║ Car Skin:  %s\n", m.CarSkin)
			fmt.Printf("║ Connected: %v\n", m.IsConnected)
			fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")

		case acudp.SessionCarInfo:
			car := tracker.GetOrCreate(m.CarID)
			car.DriverName = m.DriverName
			car.DriverGUID = m.DriverGUID
			car.CarModel = m.CarModel
			car.CarSkin = m.CarSkin

			eventName := "CONNECTION"
			if m.EventType == acudp.EventConnectionClosed {
				eventName = "DISCONNECTION"
			}

			fmt.Printf("\n[%s] Car #%d - %s (%s) - %s\n",
				eventName, m.CarID, m.DriverName, m.CarModel, m.DriverGUID)

		case acudp.Chat:
			fmt.Printf("\n💬 [CHAT] %s: %s\n", m.DriverName, m.Message)

		case acudp.ClientLoaded:
			fmt.Printf("\n✅ [CLIENT LOADED] Car #%d\n", m)

		case acudp.SessionInfo:
			if m.EventType == acudp.EventNewSession {
				fmt.Printf("\n╔═══════════════════════════════════════════════════════════╗\n")
				fmt.Printf("║ 🏁 NEW SESSION STARTED\n")
				fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
				fmt.Printf("║ Server:    %s\n", m.ServerName)
				fmt.Printf("║ Session:   %s\n", m.Name)
				fmt.Printf("║ Type:      %s\n", m.Type)
				fmt.Printf("║ Track:     %s", m.Track)
				if m.TrackConfig != "" {
					fmt.Printf(" (%s)", m.TrackConfig)
				}
				fmt.Printf("\n")
				if m.Laps > 0 {
					fmt.Printf("║ Laps:      %d\n", m.Laps)
				}
				if m.Time > 0 {
					fmt.Printf("║ Time:      %d minutes\n", m.Time)
				}
				fmt.Printf("║ Weather:   %s\n", m.WeatherGraphics)
				fmt.Printf("║ Ambient:   %d°C\n", m.AmbientTemp)
				fmt.Printf("║ Road:      %d°C\n", m.RoadTemp)
				fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")
			}

		case acudp.EndSession:
			fmt.Printf("\n🏁 [SESSION ENDED] Results file: %s\n", string(m))

		case acudp.LapCompleted:
			car := tracker.GetOrCreate(m.CarID)
			car.LapCount++
			car.LastLapTime = m.LapTime
			car.TotalLapTime += m.LapTime

			fmt.Printf("\n╔═══════════════════════════════════════════════════════════╗\n")
			fmt.Printf("║ 🏁 LAP COMPLETED - Car #%d\n", m.CarID)
			fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
			if car.DriverName != "" {
				fmt.Printf("║ Driver:    %s\n", car.DriverName)
			}
			fmt.Printf("║ Lap Time:  %s\n", formatLapTime(m.LapTime))
			fmt.Printf("║ Cuts:      %d\n", m.Cuts)
			fmt.Printf("║ Lap #:     %d\n", car.LapCount)
			if car.LapCount > 1 {
				avgLap := car.TotalLapTime / uint32(car.LapCount)
				fmt.Printf("║ Avg Lap:   %s\n", formatLapTime(avgLap))
			}
			fmt.Printf("║ \n")
			fmt.Printf("║ 📊 LEADERBOARD (%d cars):\n", m.CarsCount)
			for i, lbCar := range m.Cars {
				completed := ""
				if lbCar.Completed == 1 {
					completed = " ✓"
				}
				fmt.Printf("║   %2d. Car #%d - Laps: %d - Time: %s%s\n",
					i+1, lbCar.CarID, lbCar.Laps, formatLapTime(lbCar.LapTime), completed)
			}
			fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")

		case acudp.CollisionWithCar:
			car1, _ := tracker.Get(m.CarID)
			car2, _ := tracker.Get(m.OtherCarID)

			if car1 != nil {
				car1.CollisionCount++
			}

			fmt.Printf("\n💥 [COLLISION] Car #%d", m.CarID)
			if car1 != nil && car1.DriverName != "" {
				fmt.Printf(" (%s)", car1.DriverName)
			}
			fmt.Printf(" hit Car #%d", m.OtherCarID)
			if car2 != nil && car2.DriverName != "" {
				fmt.Printf(" (%s)", car2.DriverName)
			}
			fmt.Printf("\n   Impact Speed: %.1f km/h\n", m.ImpactSpeed*3.6)
			fmt.Printf("   Position: X=%.2f Y=%.2f Z=%.2f\n", m.WorldPos.X, m.WorldPos.Y, m.WorldPos.Z)

		case acudp.CollisionWithEnvironment:
			car, _ := tracker.Get(m.CarID)
			if car != nil {
				car.CollisionCount++
			}

			fmt.Printf("\n💥 [COLLISION] Car #%d", m.CarID)
			if car != nil && car.DriverName != "" {
				fmt.Printf(" (%s)", car.DriverName)
			}
			fmt.Printf(" hit Environment\n")
			fmt.Printf("   Impact Speed: %.1f km/h\n", m.ImpactSpeed*3.6)
			fmt.Printf("   Position: X=%.2f Y=%.2f Z=%.2f\n", m.WorldPos.X, m.WorldPos.Y, m.WorldPos.Z)

		case acudp.ServerError:
			logrus.Errorf("❌ Server Error: %v", m)

		default:
			fmt.Printf("⚠️  Unknown message type: %T\n", m)
		}
	}

	// Create a new server client
	client, err := acudp.NewServerClient(config, callback)
	if err != nil {
		logrus.Fatalf("Failed to create server client: %v", err)
	}

	defer client.Close()

	// CRITICAL: Request session info to trigger realtime updates
	fmt.Println("\n🔄 Requesting session info and enabling realtime updates...")
	// Request session info
	if err := client.SendMessage(acudp.GetSessionInfo{}); err != nil {
		logrus.Errorf("Failed to request session info: %v", err)
	} else {
		logrus.Info("Session info requested")
	}

	// Explicitly enable realtime position updates
	time.Sleep(500 * time.Millisecond)
	if err := client.SendMessage(acudp.NewEnableRealtimePosInterval(100)); err != nil {
		logrus.Errorf("Failed to enable realtime updates: %v", err)
	} else {
		logrus.Info("Realtime updates enabled (100ms interval)")
	}

	// Example: Send a welcome message after connection
	go func() {
		time.Sleep(2 * time.Second)
		msg, err := acudp.NewBroadcastChat("🏎️ Position Tracker Connected!")
		if err != nil {
			logrus.Error("Failed to create chat message:", err)
			return
		}
		if err := client.SendMessage(msg); err != nil {
			logrus.Error("Failed to send chat message:", err)
		}
	}()

	// Periodic status report
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			cars := tracker.GetAll()
			if len(cars) == 0 {
				continue
			}

			fmt.Printf("\n╔═══════════════════════════════════════════════════════════╗\n")
			fmt.Printf("║ 📊 STATUS REPORT - %d Active Cars\n", len(cars))
			fmt.Printf("╠═══════════════════════════════════════════════════════════╣\n")
			for _, car := range cars {
				fmt.Printf("║ Car #%d", car.CarID)
				if car.DriverName != "" {
					fmt.Printf(" - %s", car.DriverName)
				}
				fmt.Printf("\n")
				fmt.Printf("║   Speed: %.1f km/h | Max: %.1f km/h | Laps: %d\n",
					car.Speed, car.MaxSpeed, car.LapCount)
			}
			fmt.Printf("╚═══════════════════════════════════════════════════════════╝\n")
		}
	}()

	// Wait for interrupt signal to gracefully shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("\n👋 Shutting down...")
}

// formatLapTime formats milliseconds to MM:SS.mmm
func formatLapTime(ms uint32) string {
	totalSeconds := ms / 1000
	milliseconds := ms % 1000
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60

	return fmt.Sprintf("%02d:%02d.%03d", minutes, seconds, milliseconds)
}
