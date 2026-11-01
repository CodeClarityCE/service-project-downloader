package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
)

// Mid-size repo from the study corpus (19 snapshot analyses in a full run) and
// two of its manifest commits — exactly the workload the cache exists for.
const (
	cacheTestURL  = "https://github.com/webpack/webpack"
	cacheTestSHA1 = "759851cb4500ac29fdef4a69c65f99602a72900c"
	cacheTestSHA2 = "87310da049cc2361ece0273daef7e5e46dc6d3f5"
)

// requireRemote skips network-dependent tests when the git mirror is not
// reachable, so the suite stays runnable offline.
func requireRemote(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "ls-remote", cacheTestURL, "HEAD").Run(); err != nil {
		t.Skipf("git mirror %s not reachable: %v", cacheTestURL, err)
	}
}

func TestGitCacheMaterialize(t *testing.T) {
	requireRemote(t)
	base := t.TempDir()
	cacheBase := filepath.Join(base, "cache", "project-1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// First materialization creates the cache and checks out the commit.
	dest1 := filepath.Join(base, "projects", "project-1", cacheTestSHA1)
	if err := materializeFromCache(ctx, cacheTestURL, cacheBase, dest1, cacheTestSHA1, ""); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if !checkedOutAt(ctx, dest1, cacheTestSHA1) {
		t.Fatalf("dest1 not checked out at %s", cacheTestSHA1)
	}

	// Second materialization at a different SHA must reuse the cache.
	cacheInfo, err := os.Stat(cacheBase + ".git")
	if err != nil {
		t.Fatalf("cache dir missing after first materialize: %v", err)
	}
	dest2 := filepath.Join(base, "projects", "project-1", cacheTestSHA2)
	if err := materializeFromCache(ctx, cacheTestURL, cacheBase, dest2, cacheTestSHA2, ""); err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if !checkedOutAt(ctx, dest2, cacheTestSHA2) {
		t.Fatalf("dest2 not checked out at %s", cacheTestSHA2)
	}
	if again, err := os.Stat(cacheBase + ".git"); err != nil || !again.ModTime().Equal(cacheInfo.ModTime()) {
		t.Fatalf("cache was re-created instead of reused (err=%v)", err)
	}

	// Re-materializing an existing leaf is a no-op (existing-checkout fast path).
	if err := materializeFromCache(ctx, cacheTestURL, cacheBase, dest1, cacheTestSHA1, ""); err != nil {
		t.Fatalf("re-materialize existing leaf: %v", err)
	}

	// HEAD analysis: leaf lands on the branch at the freshly fetched tip.
	destHead := filepath.Join(base, "projects", "project-1", "main")
	if err := materializeFromCache(ctx, cacheTestURL, cacheBase, destHead, "", "main"); err != nil {
		t.Fatalf("HEAD materialize: %v", err)
	}
	tip, err := gitOutput(ctx, cacheBase+".git", "rev-parse", "refs/heads/main")
	if err != nil || tip == "" {
		t.Fatalf("resolve cached branch tip: %q, %v", tip, err)
	}
	if !checkedOutAt(ctx, destHead, tip) {
		t.Fatalf("HEAD dest not checked out at branch tip %s", tip)
	}

	// The token-bearing URL must not be persisted in the cache config.
	if cfg, _ := os.ReadFile(filepath.Join(cacheBase+".git", "config")); strings.Contains(string(cfg), "github.com") {
		t.Fatalf("cache config persists a remote URL:\n%s", cfg)
	}
}

func TestGitCacheFailureBackoff(t *testing.T) {
	base := t.TempDir()
	cacheBase := filepath.Join(base, "cache", "project-2")
	if err := os.MkdirAll(filepath.Dir(cacheBase), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheBase+".unavailable", []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := materializeFromCache(ctx, "https://invalid.invalid/x", cacheBase, filepath.Join(base, "leaf"), cacheTestSHA1, "")
	if err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("expected backoff error, got: %v", err)
	}
}

func TestGitCacheLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "p.lock")
	ctx := context.Background()
	unlock, err := acquireCacheLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// A second holder must block until release; flock is per-fd (BSD lock), so
	// a separate open contends even within one process.
	blockedCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	if _, err := acquireCacheLock(blockedCtx, lockPath); err == nil {
		t.Fatal("second acquire succeeded while lock held")
	}
	unlock()
	unlock() // idempotent
	unlock2, err := acquireCacheLock(ctx, lockPath)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	unlock2()
}

// TestGitFullFlowViaCache drives the public Git() entry point twice for the
// same project (two snapshot commits) and asserts both leaves appear at the
// documented destination layout.
func TestGitFullFlowViaCache(t *testing.T) {
	requireRemote(t)
	base := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", base)
	org := uuid.MustParse("b6bd438b-e783-4b7e-a9c1-2b58e806f425")
	analysis := codeclarity.Analysis{Branch: "main"}
	project := codeclarity.Project{
		Id:  uuid.MustParse("0d6ba0ec-0b8f-42d9-a1b5-63b04358a86e"),
		Url: cacheTestURL,
	}

	for _, sha := range []string{cacheTestSHA1, cacheTestSHA2} {
		analysis.Commit = sha
		if err := Git(analysis, project, codeclarity.Integration{}, org); err != nil {
			t.Fatalf("Git() for %s: %v", sha, err)
		}
		dest := fmt.Sprintf("%s/%s/projects/%s/%s", base, org, project.Id, sha)
		ctx := context.Background()
		if !checkedOutAt(ctx, dest, sha) {
			t.Fatalf("destination %s not checked out at %s", dest, sha)
		}
	}
	if _, err := os.Stat(fmt.Sprintf("%s/%s/cache/%s.git", base, org, project.Id)); err != nil {
		t.Fatalf("expected persistent cache: %v", err)
	}
}
