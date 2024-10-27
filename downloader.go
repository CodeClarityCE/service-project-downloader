// Package main is the entry point for the downloader service.
package main

func main() {
	Downloader()
}

// Downloader is a function that starts the downloader service.
// It creates a channel to wait indefinitely and starts receiving messages from the "dispatcher_downloader" queue.
func Downloader() {
	forever := make(chan bool)
	go receiveMessage("dispatcher_downloader")
	<-forever
}
