// Package main is the entry point for the downloader service.
package main

import (
	"log"

	"github.com/CodeClarityCE/utility-types/ecosystem"
	amqp "github.com/rabbitmq/amqp091-go"
)

// DownloaderService wraps the ServiceBase with downloader-specific functionality
type DownloaderService struct {
	*ecosystem.ServiceBase
}

// NewDownloaderService creates a new DownloaderService
func NewDownloaderService() (*DownloaderService, error) {
	base, err := ecosystem.NewServiceBase()
	if err != nil {
		return nil, err
	}

	service := &DownloaderService{
		ServiceBase: base,
	}

	// Setup queue handler
	service.AddQueue("dispatcher_downloader", true, service.handleDispatcherMessage)

	return service, nil
}

// handleDispatcherMessage handles messages from dispatcher
func (s *DownloaderService) handleDispatcherMessage(d amqp.Delivery) {
	dispatch("dispatcher_downloader", d)
}

func main() {
	Downloader()
}

// Downloader is a function that starts the downloader service using ServiceBase.
func Downloader() {
	service, err := NewDownloaderService()
	if err != nil {
		log.Fatalf("Failed to create downloader service: %v", err)
	}
	defer service.Close()

	log.Printf("Starting Downloader Service...")
	if err := service.StartListening(); err != nil {
		log.Fatalf("Failed to start listening: %v", err)
	}

	log.Printf("Downloader Service started")
	service.WaitForever()
}
