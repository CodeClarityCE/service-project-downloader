package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Git clones a git project and checks out a specific branch or commit.
// It takes an analysis, project, integration, and organization as input parameters.
// The analysis parameter contains information about the branch and commit to clone.
// The project parameter contains the URL of the git project to clone.
// The integration parameter contains the access token for authentication.
// The organization parameter specifies the destination folder for the cloned project.
// If the analysis has a commit specified, Git checks out that commit after cloning the project.
// The function returns an error if any of the git commands fail.
func Git(analysis codeclarity.Analysis, project codeclarity.Project, integration codeclarity.Integration, organization uuid.UUID) error {
	// Clone git project
	url := ""
	if strings.Contains(project.Url, "gitlab") {
		url = strings.ReplaceAll(project.Url, "://", "://oauth2:"+integration.AccessToken+"@")
	} else {
		url = strings.ReplaceAll(project.Url, "://", "://"+integration.AccessToken+"@")
	}

	// GET download path from ENV
	path := os.Getenv("DOWNLOAD_PATH")

	// Destination folder
	destination := fmt.Sprintf("%s/%s/%s/%s", path, organization, "projects", project.Id)

	if analysis.Commit == "" || analysis.Commit == " " {
		destination = fmt.Sprintf("%s/%s", destination, analysis.Branch)
	} else {
		destination = fmt.Sprintf("%s/%s", destination, analysis.Commit)
	}

	// Clone project
	cmd := exec.Command("git", "clone", "--recursive", "-b", analysis.Branch, url, destination)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		log.Println(err.Error())
		// updateDownloadStatus(name, project, "f")
		cmd := exec.Command("git", "pull")
		cmd.Dir = destination
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			log.Println(err.Error())
			return err
		}
	}

	if analysis.Commit == "" || analysis.Commit == " " {
		return nil
	}

	// Check branches
	cmd = exec.Command("git", "checkout", analysis.Commit)
	cmd.Dir = destination
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Println(err.Error())
		// updateDownloadStatus(name, project, "f")
		return err
	}

	// Update download status
	// updateDownloadStatus(name, project, "t")
	return nil
}
