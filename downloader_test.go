package main

import (
	"os"
	"testing"

	"github.com/CodeClarityCE/service-project-downloader/test"
)

// func TestReceiveSymfony(t *testing.T) {
// 	var tests = []struct {
// 		value string
// 		want  error
// 	}{
// 		{"symfony_dispatcher", nil},
// 		{"sbom_dispatcher", nil},
// 	}
// 	for _, tt := range tests {
// 		testname := fmt.Sprintf(tt.value)
// 		t.Run(testname, func(t *testing.T) {
// 			// START TEST
// 			_, msg, err := openConnection(tt.value)
// 			if err != nil {
// 				t.Errorf(msg)
// 			}
// 			// END TEST
// 		})
// 	}
// }

func TestMain(t *testing.T) {
	os.Setenv("AMQP_PROTOCOL", "amqp")
	os.Setenv("AMQP_HOST", "localhost")
	os.Setenv("AMQP_PORT", "5672")
	os.Setenv("AMQP_USER", "guest")
	os.Setenv("AMQP_PASSWORD", "guest")
	os.Setenv("DOWNLOAD_PATH", "/tmp")

	// Test now uses ServiceBase instead of legacy receiveMessage
	service, err := CreateDownloaderService()
	if err != nil {
		t.Fatalf("Failed to create downloader service: %v", err)
	}
	defer service.Close()

	// Test that service was created successfully
	if service == nil {
		t.Error("Expected service to be created, got nil")
	}
}

func TestScenario1(t *testing.T) {
	connection := "symfony_dispatcher"

	// Send mock data from scenario 1
	err := test.Scenario1(connection)
	if err != nil {
		// `t.Error*` will report test failures but continue
		// executing the test. `t.Fatal*` will report test
		// failures and stop the test immediately.
		t.Errorf("%s", err.Error())
	}

	// Listen for messages
	// d := test.ReceiveMessage(connection)

	// Test the dispatcher for this message
	// dispatch(connection, d)
}

func TestGit(t *testing.T) {

	// project := types_database.Project{
	// 	Name: "git@github.com:CodeClarityCE/codeclarity.git",
	// 	UID:  1,
	// }

	// Set env variable
	// os.Setenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable")

	// Git(project)

	// Send mock data from scenario 1
	// if err != nil {
	// `t.Error*` will report test failures but continue
	// executing the test. `t.Fatal*` will report test
	// failures and stop the test immediately.
	// t.Errorf(err.Error())
	// }

	// Listen for messages
	// d := test.ReceiveMessage(connection)

	// Test the dispatcher for this message
	// dispatch(connection, d)
}
