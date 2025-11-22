package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/xsfunc/acudp"
)

func main() {
	// Configure the logger
	logrus.SetLevel(logrus.DebugLevel)

	// Define the server configuration
	config := acudp.ServerConfig{
		Addr:        "127.0.0.1", // Address of the AC server (usually localhost if running locally)
		ReceivePort: 9996,        // Port to receive data from AC server
		SendPort:    9996,        // Port to send data to AC server
		// Optional: Forwarding configuration
		// Forward:           true,
		// ForwardAddrStr:    "127.0.0.1:9997",
		// ForwardListenPort: 9998,
	}

	// Define the callback function to handle incoming messages
	callback := func(msg acudp.Message) {
		switch m := msg.(type) {
		case acudp.CarUpdate:
			fmt.Printf("Car Update: ID=%d, RPM=%d, Velocity=%v\n", m.CarID, m.EngineRPM, m.Velocity)
		case acudp.SessionInfo:
			fmt.Printf("Session Info: Name=%s, Type=%d\n", m.Name, m.Type)
		case acudp.CarInfo:
			fmt.Printf("Car Info: ID=%d, Model=%s, Driver=%s\n", m.CarID, m.CarModel, m.DriverName)
		case acudp.Chat:
			fmt.Printf("Chat: %d: %s\n", m.CarID, m.Message)
		case acudp.ClientLoaded:
			fmt.Printf("Client Loaded: ID=%d\n", m)
		case acudp.SessionCarInfo:
			switch m.EventType {
			case acudp.EventNewConnection:
				fmt.Printf("New Connection: ID=%d, Name=%s\n", m.CarID, m.DriverName)
			case acudp.EventConnectionClosed:
				fmt.Printf("Connection Closed: ID=%d, Name=%s\n", m.CarID, m.DriverName)
			default:
				fmt.Printf("Session Car Info: ID=%d, Name=%s\n", m.CarID, m.DriverName)
			}
		case acudp.ServerError:
			fmt.Printf("Server Error: %v\n", m)
		default:
			// fmt.Printf("Received message: %T\n", m)
		}
	}

	// Create a new server client
	client, err := acudp.NewServerClient(config, callback)
	if err != nil {
		logrus.Fatalf("Failed to create server client: %v", err)
	}

	// Ensure the client is closed when the program exits
	defer client.Close()

	fmt.Println("AC UDP Client started. Press Ctrl+C to exit.")

	// Wait for interrupt signal to gracefully shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	fmt.Println("Shutting down...")
}
