package main

import (
	"context"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// failOnError checks if an error occurred and panics with a formatted error message if it did.
// It takes an error and a message as parameters.
// If the error is not nil, it logs a panic message with the given message and the error.
// This function is typically used to handle critical errors that should cause the program to stop execution.
func failOnError(err error, msg string) {
	if err != nil {
		log.Panicf("%s: %s", msg, err)
	}
}

// send sends a message to the specified connection using AMQP protocol.
// It establishes a connection to RabbitMQ server, opens a channel, declares a queue,
// and publishes the message to the queue.
// The connection parameter specifies the name of the queue to send the message to.
// The data parameter contains the message data to be sent.
func send(connection string, data []byte) {
	log.Println("Sending message to " + connection)
	conn, err := amqp.Dial("amqp://guest:guest@rabbitmq:5672/")
	failOnError(err, "Failed to connect to RabbitMQ")
	defer conn.Close()

	ch, err := conn.Channel()
	failOnError(err, "Failed to open a channel")
	defer ch.Close()

	q, err := ch.QueueDeclare(
		connection, // name
		true,       // durable
		false,      // delete when unused
		false,      // exclusive
		false,      // no-wait
		nil,        // arguments
	)
	failOnError(err, "Failed to declare a queue")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = ch.PublishWithContext(ctx,
		"",     // exchange
		q.Name, // routing key
		false,  // mandatory
		false,  // immediate
		amqp.Publishing{
			ContentType: "text/javascript",
			Body:        data,
		})
	failOnError(err, "Failed to publish a message")
	// log.Printf(" [x] Sent %s\n", data)
}
