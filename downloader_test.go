package main

import (
	"testing"

	"github.com/CodeClarityCE/utility-boilerplates/testdeps"
)

func TestCreateDownloaderService(t *testing.T) {
	testdeps.ServiceDatabases(t)
	testdeps.RabbitMQ(t)
	t.Setenv("DOWNLOAD_PATH", t.TempDir())

	service, err := CreateDownloaderService()
	if err != nil {
		t.Fatalf("create downloader service: %v", err)
	}
	defer service.Close()

	if service.ServiceBase == nil {
		t.Error("Expected ServiceBase to be embedded, got nil")
	}
}
