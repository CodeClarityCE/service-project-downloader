package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	types_amqp "github.com/CodeClarityCE/utility-types/amqp"

	amqp "github.com/rabbitmq/amqp091-go"
)

// receiveMessage receives messages from a RabbitMQ queue and dispatches them for processing.
// It establishes a connection to RabbitMQ, opens a channel, declares a queue, and consumes messages from the queue.
// Each received message is dispatched for processing and the elapsed time is logged.
// The function blocks until a signal is received to exit.
//
// Parameters:
// - connection: The name of the RabbitMQ queue to consume messages from.
//
// Example:
//
//	receiveMessage("my_queue")
//
// Returns: None
func receiveMessage(connection string) {
	// Create connexion
	url := ""
	protocol := os.Getenv("AMQP_PROTOCOL")
	if protocol == "" {
		protocol = "amqp"
	}
	host := os.Getenv("AMQP_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("AMQP_PORT")
	if port == "" {
		port = "5672"
	}
	user := os.Getenv("AMQP_USER")
	if user == "" {
		user = "guest"
	}
	password := os.Getenv("AMQP_PASSWORD")
	if password == "" {
		password = "guest"
	}
	url = protocol + "://" + user + ":" + password + "@" + host + ":" + port + "/"

	conn, err := amqp.Dial(url)
	if err != nil {
		failOnError(err, "Failed to connect to RabbitMQ")
	}
	defer conn.Close()

	// Open channel
	ch, err := conn.Channel()
	if err != nil {
		failOnError(err, "Failed to open a channel")
	}
	defer ch.Close()

	// Declare queue
	q, err := ch.QueueDeclare(
		connection, // name
		true,       // durable
		false,      // delete when unused
		false,      // exclusive
		false,      // no-wait
		nil,        // arguments
	)
	if err != nil {
		failOnError(err, "Failed to declare a queue")
	}

	// Consume messages
	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	if err != nil {
		failOnError(err, "Failed to register a consumer")
	}

	var forever = make(chan struct{})
	go func() {
		for d := range msgs {
			// Start timer
			start := time.Now()

			dispatch(connection, d)

			// Print time elapsed
			t := time.Now()
			elapsed := t.Sub(start)
			log.Println(elapsed)
		}
	}()

	log.Printf(" [*] DOWNLOADER Waiting for messages from " + connection + ". To exit press CTRL+C")
	<-forever
}

// dispatch is a function that handles the received message from the "dispatcher_downloader" connection.
// It reads the message from the API, retrieves analysis, project, and integration information,
// downloads the project, and sends a message to the "downloader_dispatcher" connection.
// Parameters:
// - connection: a string representing the connection name
// - d: an amqp.Delivery object containing the message data
// Returns: None
func dispatch(connection string, d amqp.Delivery) {
	if connection == "dispatcher_downloader" { // If message is from symfony_request
		// Read message from API
		var apiMessage types_amqp.DispatcherDownloaderMessage
		json.Unmarshal([]byte(d.Body), &apiMessage)

		// Get info
		analysis_info, err := getAnalysis(apiMessage.AnalysisId)
		if err != nil {
			log.Printf("%v", err)
			// TODO: Send error message
		}

		project_info, err := getProject(apiMessage.ProjectId)
		if err != nil {
			log.Printf("%v", err)
		}

		integration_info, err := getIntegration(apiMessage.IntegrationId)
		if err != nil {
			log.Printf("%v", err)
		}

		// Download project
		err = Git(analysis_info, project_info, integration_info, apiMessage.OrganizationId)
		if err != nil {
			log.Printf("%v", err)
			// TODO Send error message
		}

		// Send message to dispatcher
		// Change type
		downloaderMessage := types_amqp.DownloaderDispatcherMessage(apiMessage)
		// downloaderMessage := types_amqp.DownloaderDispatcherMessage{
		// 	AnalysisId:     apiMessage.AnalysisId,
		// 	ProjectId:      apiMessage.ProjectId,
		// 	IntegrationId:  apiMessage.IntegrationId,
		// 	OrganizationId: apiMessage.OrganizationId,
		// }
		data, _ := json.Marshal(downloaderMessage)
		send("downloader_dispatcher", data)
	}

}
