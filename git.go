package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// ErrCommitUnresolvable is returned when a historical-commit analysis cannot be
// resolved by any strategy (shallow fetch-by-SHA miss and blobless
// clone+checkout also miss). It is wrapped into the returned error so the
// caller can classify the terminal failure and logs are greppable.
var ErrCommitUnresolvable = errors.New("CommitUnresolvable")

// defaultDownloadTimeout bounds a single Git() resolve/clone so a hung fetch
// (unreachable host, blocking network, credential stall) becomes a terminal
// failure instead of wedging the downloader's serial consumer forever.
const defaultDownloadTimeout = 600 * time.Second

// shallowFetchTimeout bounds the tier-1 shallow fetch-by-SHA attempt on
// historical-commit analyses. Derived as a child of the overall download
// context, so the effective budget is min(shallowFetchTimeout, time remaining);
// a hung shallow attempt cannot eat the whole budget the blobless-clone
// fallback needs on large repos.
const shallowFetchTimeout = 120 * time.Second

// downloadTimeout returns the per-download deadline, overridable via
// DOWNLOAD_TIMEOUT_SECONDS (seconds). Mirrors the env-override convention used
// elsewhere (REAPER_INTERVAL, DB_MAX_OPEN_CONNS).
func downloadTimeout() time.Duration {
	if v := os.Getenv("DOWNLOAD_TIMEOUT_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		log.Printf("[downloader] invalid DOWNLOAD_TIMEOUT_SECONDS=%q, using default %s", v, defaultDownloadTimeout)
	}
	return defaultDownloadTimeout
}

// runGit runs a git command bounded by ctx, with interactive prompts disabled so
// a missing/invalid credential fails fast instead of blocking on a terminal
// prompt. On ctx timeout the process is killed so a hung fetch is reclaimed.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // no interactive username/password prompt
		"GIT_ASKPASS=true",      // no askpass helper hang
		"GCM_INTERACTIVE=never", // git-credential-manager non-interactive
	)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	return cmd.Run()
}

// Git clones a git project and checks out a specific branch or commit.
// It takes an analysis, project, integration, and organization as input parameters.
// The analysis parameter contains information about the branch and commit to clone.
// The project parameter contains the URL of the git project to clone.
// The integration parameter contains the access token for authentication.
// The organization parameter specifies the destination folder for the cloned project.
// If the analysis has a commit specified, Git checks out that commit after cloning the project.
// The function returns an error if any of the git commands fail.
func Git(analysis codeclarity.Analysis, project codeclarity.Project, integration codeclarity.Integration, organization uuid.UUID) error {
	// Bound the whole resolve/clone so a hung fetch becomes a terminal failure
	// rather than wedging the serial consumer.
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout())
	defer cancel()

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
		if err := runGit(ctx, "", "clone", "--depth", "1", "--single-branch",
			"--recurse-submodules", "--shallow-submodules", "-b", analysis.Branch, url, destination); err != nil {
			log.Println(err.Error())
			// The destination may already exist from a previous run — try to
			// refresh it in place rather than failing outright.
			if err2 := runGit(ctx, destination, "pull"); err2 != nil {
				log.Println(err2.Error())
				return err
			}
		}
		return nil
	}

	// Historical-commit analyses resolve in two tiers:
	//  1. Shallow-fetch the exact commit by SHA (GitHub serves reachable SHAs),
	//     the cheap happy path — bounded by its own sub-deadline so a hung
	//     attempt leaves budget for the fallback.
	//  2. Blobless clone (--filter=blob:none): the full commit/tree DAG at
	//     ~5-10x less transfer than a full clone (fits the deadline on large
	//     repos); checkout faults in only the requested commit's blobs. No -b
	//     flag — fetching all refs resolves any reachable commit even if its
	//     branch moved or was deleted.
	shallowCtx, cancelShallow := context.WithTimeout(ctx, shallowFetchTimeout)
	shallowErr := shallowFetchCommit(shallowCtx, url, analysis.Commit, destination)
	cancelShallow()
	if shallowErr == nil {
		return nil
	}
	log.Printf("shallow fetch of %s failed (%s); falling back to blobless clone", analysis.Commit, shallowErr)

	_ = os.RemoveAll(destination)
	if err := runGit(ctx, "", "clone", "--filter=blob:none", "--no-checkout", url, destination); err != nil {
		log.Println(err.Error())
		return fmt.Errorf("%w: commit %s in %s: %v", ErrCommitUnresolvable, analysis.Commit, project.Url, err)
	}
	// Non-fatal: catches SHAs the server serves but that are not reachable from
	// any advertised ref (e.g. force-pushed-away commits); the checkout below
	// decides success either way.
	if err := runGit(ctx, destination, "fetch", "origin", analysis.Commit); err != nil {
		log.Printf("fetch of %s by SHA failed (%s); trying checkout from cloned refs", analysis.Commit, err)
	}
	if err := runGit(ctx, destination, "checkout", "--recurse-submodules", analysis.Commit); err != nil {
		log.Println(err.Error())
		return fmt.Errorf("%w: commit %s in %s: %v", ErrCommitUnresolvable, analysis.Commit, project.Url, err)
	}
	return nil
}

// shallowFetchCommit initialises a repo at destination and shallow-fetches
// exactly the requested commit (depth 1), then checks it out. This is far
// cheaper than any clone for historical snapshots. Returns an error if any
// git step fails so the caller can fall back to a blobless clone.
func shallowFetchCommit(ctx context.Context, url, commit, destination string) error {
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
		if err := runGit(ctx, destination, args...); err != nil {
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
