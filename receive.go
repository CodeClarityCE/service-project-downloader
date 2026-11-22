package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/CodeClarityCE/utility-boilerplates"
	types_amqp "github.com/CodeClarityCE/utility-types/amqp"
	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sirupsen/logrus"
)

// projectSourcePath returns where a project's files were placed for this analysis.
// FILE projects extract to the branch directory even when a commit is set.
func projectSourcePath(downloadPath string, organization uuid.UUID, project codeclarity.Project, analysis codeclarity.Analysis) string {
	folder := analysis.Commit
	if project.Type == "FILE" {
		folder = archiveBranch(analysis)
		downloadPath = archiveRoot(downloadPath)
	} else if folder == "" || folder == " " {
		folder = analysis.Branch
	}
	return filepath.Join(downloadPath, organization.String(), "projects", project.Id.String(), folder)
}

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
		if err := json.Unmarshal(d.Body, &apiMessage); err != nil {
			logrus.WithError(err).Warn("Dropping a malformed dispatcher message")
			return
		}

		// Get info. A failed analysis load is left non-terminal so the dispatcher's
		// reaper retries it (the load itself may be a transient DB hiccup); every
		// later error is a terminal download failure and marks the analysis FAILURE
		// so the reaper stops re-driving it.
		analysis_info, err := getAnalysis(service.DB.CodeClarity, apiMessage.AnalysisId)
		if errors.Is(err, sql.ErrNoRows) {
			logSkipped(service, apiMessage.AnalysisId, "the analysis was deleted")
			return
		}
		if err != nil {
			log.Printf("Failed to load analysis %s: %v", apiMessage.AnalysisId, err)
			return
		}
		if analysis_info.Status == codeclarity.CANCELLED {
			logSkipped(service, apiMessage.AnalysisId, "the analysis was cancelled")
			return
		}

		project_info, err := getProject(service.DB.CodeClarity, *analysis_info.ProjectId)
		if err != nil {
			log.Printf("Failed to load project for analysis %s: %v", apiMessage.AnalysisId, err)
			markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, "the project for this analysis could not be loaded")
			return
		}

		// Handle based on project type
		if project_info.Type == "FILE" {
			// FILE project - extract uploaded archive
			log.Printf("Processing FILE project: %s", project_info.Id)
			err = Archive(analysis_info, project_info, apiMessage.OrganizationId)
			if err != nil {
				log.Printf("Failed to extract archive: %v", err)
				reason := "the uploaded archive could not be extracted; check that it is a valid zip or tar.gz file"
				if errors.Is(err, ErrInvalidRevision) {
					reason = downloadFailureReason(err)
				}
				markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, reason)
				return
			}
		} else {
			// VCS project (GITHUB, GITLAB) - git clone
			log.Printf("Processing VCS project: %s (type: %s)", project_info.Id, project_info.Type)
			integration_info, err := getIntegration(service.DB.CodeClarity, apiMessage.IntegrationId)
			if err != nil {
				log.Printf("Failed to load integration for analysis %s: %v", apiMessage.AnalysisId, err)
				markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, "the integration for this project could not be loaded; check that it still exists")
				return
			}

			err = Git(analysis_info, project_info, integration_info, apiMessage.OrganizationId)
			if err != nil {
				if errors.Is(err, ErrCommitUnresolvable) || errors.Is(err, context.DeadlineExceeded) {
					log.Printf("[downloader] analysis %s: commit unresolvable/timed out, failing: %v", apiMessage.AnalysisId, err)
				} else {
					log.Printf("[downloader] analysis %s: download failed: %v", apiMessage.AnalysisId, err)
				}
				markAnalysisFailed(service.DB.CodeClarity, apiMessage.AnalysisId, downloadFailureReason(err))
				return
			}
		}

		// Detect languages from the downloaded repository
		// Build the project path where the repository was cloned
		destination := projectSourcePath(os.Getenv("DOWNLOAD_PATH"), apiMessage.OrganizationId, project_info, analysis_info)

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

// logSkipped records that a download was not started because its analysis is gone or cancelled.
func logSkipped(service *boilerplates.ServiceBase, analysisId uuid.UUID, reason string) {
	service.LogInfo("Skipping download: "+reason, logrus.Fields{"analysis_id": analysisId.String()})
}
