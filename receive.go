package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/CodeClarityCE/utility-boilerplates"
	types_amqp "github.com/CodeClarityCE/utility-types/amqp"
	amqp "github.com/rabbitmq/amqp091-go"
)

// dispatch is a function that handles the received message from the "dispatcher_downloader" connection.
// It reads the message from the API, retrieves analysis, project, and integration information,
// downloads the project, and sends a message to the "downloader_dispatcher" connection.
// Parameters:
// - connection: a string representing the connection name
// - d: an amqp.Delivery object containing the message data
// - service: ServiceBase instance for sending messages
// Returns: None
func dispatch(connection string, d amqp.Delivery, service *boilerplates.ServiceBase) {
	if connection == "dispatcher_downloader" { // If message is from dispatcher
		// Read message from API
		var apiMessage types_amqp.DispatcherDownloaderMessage
		json.Unmarshal([]byte(d.Body), &apiMessage)

		// Get info. A failed analysis load is left non-terminal so the dispatcher's
		// reaper retries it (the load itself may be a transient DB hiccup); every
		// later error is a terminal download failure and marks the analysis FAILURE
		// so the reaper stops re-driving it.
		analysis_info, err := getAnalysis(service.DB.CodeClarity, apiMessage.AnalysisId)
		if err != nil {
			log.Printf("Failed to load analysis %s: %v", apiMessage.AnalysisId, err)
			return
		}

		project_info, err := getProject(service.DB.CodeClarity, *analysis_info.ProjectId)
		if err != nil {
			log.Printf("Failed to load project for analysis %s: %v", apiMessage.AnalysisId, err)
			markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, err)
			return
		}

		// Handle based on project type
		if project_info.Type == "FILE" {
			// FILE project - extract uploaded archive
			log.Printf("Processing FILE project: %s", project_info.Id)
			err = Archive(analysis_info, project_info, apiMessage.OrganizationId)
			if err != nil {
				log.Printf("Failed to extract archive: %v", err)
				markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, err)
				return
			}
		} else {
			// VCS project (GITHUB, GITLAB) - git clone
			log.Printf("Processing VCS project: %s (type: %s)", project_info.Id, project_info.Type)
			integration_info, err := getIntegration(service.DB.CodeClarity, apiMessage.IntegrationId)
			if err != nil {
				log.Printf("Failed to load integration for analysis %s: %v", apiMessage.AnalysisId, err)
				markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, err)
				return
			}

			err = Git(analysis_info, project_info, integration_info, apiMessage.OrganizationId)
			if err != nil {
				if errors.Is(err, ErrCommitUnresolvable) || errors.Is(err, context.DeadlineExceeded) {
					log.Printf("[downloader] analysis %s: commit unresolvable/timed out, failing: %v", apiMessage.AnalysisId, err)
				} else {
					log.Printf("[downloader] analysis %s: download failed: %v", apiMessage.AnalysisId, err)
				}
				markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, err)
				return
			}
		}

		// Detect languages from the downloaded repository
		// Build the project path where the repository was cloned
		path := os.Getenv("DOWNLOAD_PATH")
		destination := fmt.Sprintf("%s/%s/%s/%s", path, apiMessage.OrganizationId, "projects", project_info.Id)
		if analysis_info.Commit == "" || analysis_info.Commit == " " {
			destination = fmt.Sprintf("%s/%s", destination, analysis_info.Branch)
		} else {
			destination = fmt.Sprintf("%s/%s", destination, analysis_info.Commit)
		}

		languageResult := detectLanguagesFromRepository(destination)

		// Send message to dispatcher with language detection results
		downloaderMessage := types_amqp.DownloaderDispatcherMessage{
			AnalysisId:          apiMessage.AnalysisId,
			ProjectId:           apiMessage.ProjectId,
			IntegrationId:       apiMessage.IntegrationId,
			OrganizationId:      apiMessage.OrganizationId,
			DetectedLanguages:   languageResult.DetectedLanguages,
			PrimaryLanguage:     languageResult.PrimaryLanguage,
			DetectionConfidence: languageResult.DetectionConfidence,
		}
		data, _ := json.Marshal(downloaderMessage)
		err = service.SendMessage("downloader_dispatcher", data)
		if err != nil {
			log.Printf("Failed to send message to downloader_dispatcher: %v", err)
		}
	}

}
