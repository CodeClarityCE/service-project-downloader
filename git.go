package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

	// HEAD analyses: a shallow single-branch clone is far faster and smaller on
	// large repos.
	if analysis.Commit == "" || analysis.Commit == " " {
		cmd := exec.Command("git", "clone", "--depth", "1", "--single-branch",
			"--recurse-submodules", "--shallow-submodules", "-b", analysis.Branch, url, destination)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Println(err.Error())
			// The destination may already exist from a previous run — try to
			// refresh it in place rather than failing outright.
			pull := exec.Command("git", "pull")
			pull.Dir = destination
			pull.Stdout = os.Stdout
			pull.Stderr = os.Stderr
			if err2 := pull.Run(); err2 != nil {
				log.Println(err2.Error())
				return err
			}
		}
		return nil
	}

	// Historical-commit analyses: shallow-fetch the exact commit by SHA (GitHub
	// serves reachable SHAs), avoiding a full-history clone. Fall back to a full
	// clone + checkout if the server won't serve the SHA directly.
	if err := shallowFetchCommit(url, analysis.Commit, destination); err == nil {
		return nil
	} else {
		log.Printf("shallow fetch of %s failed (%s); falling back to full clone", analysis.Commit, err)
	}

	_ = os.RemoveAll(destination)
	cmd := exec.Command("git", "clone", "--recursive", "-b", analysis.Branch, url, destination)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Println(err.Error())
		return err
	}
	cmd = exec.Command("git", "checkout", analysis.Commit)
	cmd.Dir = destination
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Println(err.Error())
		return err
	}
	return nil
}

// shallowFetchCommit initialises a repo at destination and shallow-fetches
// exactly the requested commit (depth 1), then checks it out. This is far
// cheaper than a full clone for historical snapshots. Returns an error if any
// git step fails so the caller can fall back to a full clone.
func shallowFetchCommit(url, commit, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", url},
		{"fetch", "--depth", "1", "--recurse-submodules", "origin", commit},
		{"checkout", "--recurse-submodules", "FETCH_HEAD"},
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = destination
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	return nil
}

// LanguageDetectionResult represents the result of language detection
type LanguageDetectionResult struct {
	DetectedLanguages   []string `json:"detected_languages"`
	PrimaryLanguage     string   `json:"primary_language"`
	DetectionConfidence float64  `json:"detection_confidence"`
}

// detectLanguagesFromRepository scans the downloaded repository to detect programming languages
// based on manifest files and file extensions
func detectLanguagesFromRepository(projectPath string) LanguageDetectionResult {
	detectedLanguages := []string{}

	// Check for JavaScript/Node.js
	if fileExists(filepath.Join(projectPath, "package.json")) ||
		fileExists(filepath.Join(projectPath, "package-lock.json")) ||
		fileExists(filepath.Join(projectPath, "yarn.lock")) ||
		fileExists(filepath.Join(projectPath, "pnpm-lock.yaml")) {
		detectedLanguages = append(detectedLanguages, "javascript")
	}

	// Check for PHP
	if fileExists(filepath.Join(projectPath, "composer.json")) ||
		fileExists(filepath.Join(projectPath, "composer.lock")) {
		detectedLanguages = append(detectedLanguages, "php")
	}

	// Determine primary language based on priority and manifest files
	primaryLanguage := "unknown"
	confidence := 0.0

	if len(detectedLanguages) > 0 {
		// If both languages detected, check which has more indicators
		if contains(detectedLanguages, "php") && contains(detectedLanguages, "javascript") {
			// Both detected - check for more specific indicators
			phpScore := 0
			jsScore := 0

			// PHP scoring
			if fileExists(filepath.Join(projectPath, "composer.json")) {
				phpScore += 2
			}
			if fileExists(filepath.Join(projectPath, "composer.lock")) {
				phpScore += 1
			}

			// JavaScript scoring
			if fileExists(filepath.Join(projectPath, "package.json")) {
				jsScore += 2
			}
			if fileExists(filepath.Join(projectPath, "package-lock.json")) ||
				fileExists(filepath.Join(projectPath, "yarn.lock")) {
				jsScore += 1
			}

			if phpScore > jsScore {
				primaryLanguage = "php"
			} else {
				primaryLanguage = "javascript"
			}
			confidence = 0.9
		} else {
			// Only one language detected
			primaryLanguage = detectedLanguages[0]
			confidence = 0.95
		}
	}

	log.Printf("Language detection for project %s: detected=%v, primary=%s, confidence=%.2f",
		projectPath, detectedLanguages, primaryLanguage, confidence)

	return LanguageDetectionResult{
		DetectedLanguages:   detectedLanguages,
		PrimaryLanguage:     primaryLanguage,
		DetectionConfidence: confidence,
	}
}

// fileExists checks if a file exists at the given path
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// contains checks if a slice contains a string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
