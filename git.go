package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// cacheLockPollInterval paces the non-blocking flock retry loop on the
// per-project cache lock; polling (instead of a blocking flock) keeps the wait
// cancellable by ctx so a replica can never leak a goroutine stuck in flock(2).
const cacheLockPollInterval = 250 * time.Millisecond

// defaultCacheFailureBackoff spaces out retries after a failed cache creation,
// so a repo whose full history cannot be cloned within budget does not pay the
// doomed multi-minute clone attempt again on every one of its ~19 snapshot
// analyses (each one falls back to the direct-clone tiers immediately instead).
const defaultCacheFailureBackoff = 30 * time.Minute

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

// cacheFailureBackoff returns how long to skip cache attempts after a failed
// cache creation, overridable via GIT_CACHE_FAILURE_BACKOFF_SECONDS (seconds).
func cacheFailureBackoff() time.Duration {
	if v := os.Getenv("GIT_CACHE_FAILURE_BACKOFF_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		log.Printf("[downloader] invalid GIT_CACHE_FAILURE_BACKOFF_SECONDS=%q, using default %s", v, defaultCacheFailureBackoff)
	}
	return defaultCacheFailureBackoff
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

// gitOutput runs a git command bounded by ctx and returns its trimmed stdout,
// with the same prompt hardening as runGit (stderr passes through for logs).
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GCM_INTERACTIVE=never",
	)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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

	// Persistent per-project bare cache: the study corpus analyses each project
	// at ~19 snapshots, and without it every snapshot re-transfers the repo.
	// Try the cache first — HEAD analyses too, so branch runs share the same
	// objects — and on ANY cache failure fall through to the direct-clone tiers
	// below unchanged, so the cache can make downloads faster but never less
	// reliable.
	cacheBase := fmt.Sprintf("%s/%s/cache/%s", path, organization, project.Id)

	// HEAD analyses: serve from the cache (fetch the branch tip, materialize at
	// it); otherwise a shallow single-branch clone is far faster and smaller on
	// large repos.
	if analysis.Commit == "" || analysis.Commit == " " {
		if err := tryCacheFirst(ctx, url, cacheBase, destination, "", analysis.Branch); err == nil {
			return nil
		}
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

	// Historical-commit analyses. The leaf directory is keyed by the commit, so
	// a valid existing checkout IS the requested tree — left by a previous
	// attempt, or by a concurrent analysis of another snapshot date that
	// resolved to the same commit (stale repos pin many dates to one SHA).
	// Reuse it instead of racing to re-clone it.
	if checkedOutAt(ctx, destination, analysis.Commit) {
		return nil
	}

	if err := tryCacheFirst(ctx, url, cacheBase, destination, analysis.Commit, ""); err == nil {
		return nil
	}

	// Resolve in two tiers, working in a private temp dir promoted by an atomic
	// rename — a half-finished clone is never visible at the leaf path, and
	// concurrent same-commit attempts converge instead of clobbering each other:
	//  1. Shallow-fetch the exact commit by SHA (GitHub serves reachable SHAs),
	//     the cheap happy path — bounded by its own sub-deadline so a hung
	//     attempt leaves budget for the fallback.
	//  2. Blobless clone (--filter=blob:none): the full commit/tree DAG at
	//     ~5-10x less transfer than a full clone (fits the deadline on large
	//     repos); checkout faults in only the requested commit's blobs. No -b
	//     flag — fetching all refs resolves any reachable commit even if its
	//     branch moved or was deleted.
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), filepath.Base(destination)+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	shallowCtx, cancelShallow := context.WithTimeout(ctx, shallowFetchTimeout)
	shallowErr := shallowFetchCommit(shallowCtx, url, analysis.Commit, tmp)
	cancelShallow()
	if shallowErr != nil {
		log.Printf("shallow fetch of %s failed (%s); falling back to blobless clone", analysis.Commit, shallowErr)
		_ = os.RemoveAll(tmp)
		if err := runGit(ctx, "", "clone", "--filter=blob:none", "--no-checkout", url, tmp); err != nil {
			log.Println(err.Error())
			return fmt.Errorf("%w: commit %s in %s: %v", ErrCommitUnresolvable, analysis.Commit, project.Url, err)
		}
		// Non-fatal: catches SHAs the server serves but that are not reachable from
		// any advertised ref (e.g. force-pushed-away commits); the checkout below
		// decides success either way.
		if err := runGit(ctx, tmp, "fetch", "origin", analysis.Commit); err != nil {
			log.Printf("fetch of %s by SHA failed (%s); trying checkout from cloned refs", analysis.Commit, err)
		}
		if err := runGit(ctx, tmp, "checkout", "--recurse-submodules", analysis.Commit); err != nil {
			log.Println(err.Error())
			return fmt.Errorf("%w: commit %s in %s: %v", ErrCommitUnresolvable, analysis.Commit, project.Url, err)
		}
	}

	_ = os.RemoveAll(destination)
	if err := os.Rename(tmp, destination); err != nil {
		// A concurrent attempt may have promoted its own checkout first — the
		// leaf holding the right commit is success, whoever produced it.
		if checkedOutAt(ctx, destination, analysis.Commit) {
			return nil
		}
		return fmt.Errorf("%w: commit %s in %s: %v", ErrCommitUnresolvable, analysis.Commit, project.Url, err)
	}
	return nil
}

// checkedOutAt reports whether destination already holds a work tree whose
// HEAD is exactly the wanted commit.
func checkedOutAt(ctx context.Context, destination, commit string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", destination, "rev-parse", "HEAD")
	out, err := cmd.Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), commit)
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

// tryCacheFirst attempts to serve the analysis from the per-project bare cache
// under a child deadline of half the remaining download budget, so even a slow
// initial full-history clone can never starve the direct-clone fallback of the
// time it needs to succeed on its own.
func tryCacheFirst(ctx context.Context, url, cacheBase, destination, commit, branch string) error {
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Until(deadline)/2)
		defer cancel()
	}
	start := time.Now()
	if err := materializeFromCache(ctx, url, cacheBase, destination, commit, branch); err != nil {
		log.Printf("[downloader] git cache fallback cache=%s.git commit=%q branch=%q after=%s: %v",
			cacheBase, commit, branch, time.Since(start).Round(time.Millisecond), err)
		return err
	}
	return nil
}

// materializeFromCache serves one analysis from the persistent per-project
// bare cache at {DOWNLOAD_PATH}/{org}/cache/{project}.git: ensure the cache
// exists and holds the wanted rev (under a cross-process flock), then
// materialize the leaf all-locally with a shared clone, promoted by the same
// tmp+atomic-rename idiom as the direct tiers. commit=="" means HEAD analysis:
// the branch tip is fetched and the leaf checked out on that branch.
//
// Empirically chosen over a blobless (--filter=blob:none) cache: a shared
// borrower of a blobless cache has no promisor config, so checkout errors on
// the missing blobs ("unable to read sha1 file"), and wiring a promisor into
// the borrower makes every leaf re-fetch its blobs from the network into its
// own objects — per-leaf transfer that defeats the cache. The full clone pays
// one bigger initial transfer, then all ~19 materializations are network-free.
func materializeFromCache(ctx context.Context, url, cacheBase, destination, commit, branch string) error {
	gitDir := cacheBase + ".git"
	marker := cacheBase + ".unavailable"

	// A recent cache-creation failure backs off further attempts, so a repo
	// whose history cannot be cloned within budget doesn't re-pay the doomed
	// attempt on every snapshot analysis.
	if fi, err := os.Stat(marker); err == nil && time.Since(fi.ModTime()) < cacheFailureBackoff() {
		return fmt.Errorf("cache creation backoff since %s", fi.ModTime().UTC().Format(time.RFC3339))
	}
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o755); err != nil {
		return err
	}
	unlock, err := acquireCacheLock(ctx, cacheBase+".lock")
	if err != nil {
		return err
	}
	defer unlock()

	ensureStart := time.Now()
	created := false
	if _, err := gitOutput(ctx, gitDir, "rev-parse", "--git-dir"); err != nil {
		if err := createBareCache(ctx, url, gitDir); err != nil {
			_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o644)
			return fmt.Errorf("cache creation: %w", err)
		}
		_ = os.Remove(marker)
		created = true
	}
	fetched := false
	sha := commit
	if commit != "" {
		// The full clone already carries every ref-reachable commit; fetch by
		// SHA only on a miss (commit newer than the cache, or unreachable from
		// advertised refs), pinned to a ref so it stays inspectable.
		if runGit(ctx, gitDir, "cat-file", "-e", commit+"^{commit}") != nil {
			if err := runGit(ctx, gitDir, "fetch", url, "+"+commit+":refs/snapshots/"+commit); err != nil {
				return fmt.Errorf("cache fetch of %s: %w", commit, err)
			}
			fetched = true
		}
	} else {
		// HEAD analysis: always refresh the branch tip (freshness is the
		// contract), then materialize at exactly what was fetched. "fetched"
		// reports whether the tip actually moved, so an unchanged branch still
		// logs as a hit.
		// --verify --quiet: a not-yet-fetched branch yields "" without log noise
		// (plain rev-parse echoes the ref name and complains on stderr).
		prevTip, _ := gitOutput(ctx, gitDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
		if err := runGit(ctx, gitDir, "fetch", url, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
			return fmt.Errorf("cache fetch of branch %s: %w", branch, err)
		}
		if sha, err = gitOutput(ctx, gitDir, "rev-parse", "refs/heads/"+branch); err != nil {
			return fmt.Errorf("cache resolve of branch %s: %w", branch, err)
		}
		fetched = sha != prevTip
	}
	ensureDur := time.Since(ensureStart)

	// Creation and fetches are the only cache mutations, so only they run under
	// the lock; the shared-clone read below proceeds lock-free even against a
	// concurrent fetch in another replica: git objects are immutable and
	// append-only, ref updates are atomic (lockfile+rename), and gc/prune are
	// disabled in the cache, so a reader can never observe an object vanishing.
	// The one theoretical race — a replica re-creating a corrupt cache while
	// another reads it — makes the reader's clone/checkout fail, which falls
	// back to the direct-clone tiers.
	unlock()

	// The leaf may already hold the resolved rev (commit path: re-check after
	// waiting on the lock; HEAD path: destination still at the fresh tip).
	if checkedOutAt(ctx, destination, sha) {
		log.Printf("[downloader] git cache hit (existing checkout) cache=%s rev=%s ensure=%s",
			gitDir, sha, ensureDur.Round(time.Millisecond))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), filepath.Base(destination)+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	materializeStart := time.Now()
	if err := runGit(ctx, "", "clone", "--shared", "--no-checkout", gitDir, tmp); err != nil {
		return fmt.Errorf("cache shared clone: %w", err)
	}
	ref := commit
	if commit == "" {
		ref = branch
	}
	if err := runGit(ctx, tmp, "checkout", "--recurse-submodules", ref); err != nil {
		return fmt.Errorf("cache checkout of %s: %w", ref, err)
	}
	_ = os.RemoveAll(destination)
	if err := os.Rename(tmp, destination); err != nil {
		// A concurrent attempt may have promoted its own checkout first — the
		// leaf holding the right commit is success, whoever produced it.
		if checkedOutAt(ctx, destination, sha) {
			return nil
		}
		return fmt.Errorf("cache promote: %w", err)
	}
	log.Printf("[downloader] git cache %s cache=%s rev=%s created=%t fetched=%t ensure=%s materialize=%s",
		map[bool]string{true: "miss", false: "hit"}[created || fetched], gitDir, sha, created, fetched,
		ensureDur.Round(time.Millisecond), time.Since(materializeStart).Round(time.Millisecond))
	return nil
}

// createBareCache builds the persistent bare cache with a full-history clone,
// promoted atomically so a crashed clone never leaves a half-written cache
// behind. gc is disabled because shared-clone leaves borrow the cache's object
// store: any prune could delete objects a live leaf still references. The
// origin remote is removed so the token-bearing URL is never persisted (and a
// rotated token can't strand the cache); every fetch passes the current URL
// explicitly instead. Caller must hold the project cache lock. On creation the
// pack size is logged so the one-time transfer cost is visible next to the
// per-analysis savings.
func createBareCache(ctx context.Context, url, gitDir string) error {
	tmp, err := os.MkdirTemp(filepath.Dir(gitDir), filepath.Base(gitDir)+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := runGit(ctx, "", "clone", "--bare", url, tmp); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"config", "gc.auto", "0"},
		{"config", "gc.pruneExpire", "never"},
		{"remote", "remove", "origin"},
	} {
		if err := runGit(ctx, tmp, args...); err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
	}
	if out, err := gitOutput(ctx, tmp, "count-objects", "-v"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if kib, ok := strings.CutPrefix(line, "size-pack: "); ok {
				log.Printf("[downloader] git cache created cache=%s size-pack=%sKiB", gitDir, kib)
			}
		}
	}
	_ = os.RemoveAll(gitDir)
	return os.Rename(tmp, gitDir)
}

// acquireCacheLock takes the cross-process per-project cache lock. flock is
// used because replicas share {DOWNLOAD_PATH} on one volume (an in-memory
// mutex would only serialize within a replica) and the kernel releases it if
// the holder crashes, so a dead replica can never wedge a project. The lock is
// polled non-blocking so ctx cancellation can't leak a thread stuck in
// flock(2). The returned unlock is idempotent: call it early to end the
// critical section, keep it deferred for error paths.
func acquireCacheLock(ctx context.Context, lockPath string) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(cacheLockPollInterval):
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, nil
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
	return slices.Contains(slice, item)
}
