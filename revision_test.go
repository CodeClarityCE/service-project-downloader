package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
)

// localRepo makes a bare repo with one commit on main and on feature/x.
func localRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "repo.git")
	for _, args := range [][]string{
		{"-C", work, "init", "-q", "-b", "main"},
		{"-C", work, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", work, "branch", "feature/x"},
		{"clone", "-q", "--bare", work, bare},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed test setup commands
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return bare
}

func TestGitRejectsOptionShapedOrEscapingRevisions(t *testing.T) {
	base := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", base)
	repo := localRepo(t)
	marker := filepath.Join(t.TempDir(), "pwned")

	cases := []codeclarity.Analysis{
		{Commit: "--upload-pack=touch " + marker + ";"},
		{Commit: "abc def"},
		{Branch: "--upload-pack=touch " + marker + ";"},
		{Branch: "../../../escape"},
		{Branch: "a/../../b"},
		{Branch: ""},
	}
	for _, analysis := range cases {
		err := Git(analysis, codeclarity.Project{Id: uuid.New(), Url: repo}, codeclarity.Integration{}, uuid.New())
		if !errors.Is(err, ErrInvalidRevision) {
			t.Errorf("Git(branch=%q commit=%q) = %v, want ErrInvalidRevision", analysis.Branch, analysis.Commit, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an option-shaped revision ran a command")
	}
	if _, err := os.Stat(filepath.Join(base, "..", "escape")); err == nil {
		t.Fatal("a branch escaped the download path")
	}
}

func TestGitClonesBranchWithSlash(t *testing.T) {
	base := t.TempDir()
	t.Setenv("DOWNLOAD_PATH", base)
	org, project := uuid.New(), codeclarity.Project{Id: uuid.New(), Url: localRepo(t)}

	if err := Git(codeclarity.Analysis{Branch: "feature/x"}, project, codeclarity.Integration{}, org); err != nil {
		t.Fatalf("Git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, org.String(), "projects", project.Id.String(), "feature", "x", ".git")); err != nil {
		t.Fatalf("checkout missing: %v", err)
	}
}

func TestArchiveRejectsEscapingBranch(t *testing.T) {
	t.Setenv("DOWNLOAD_PATH", t.TempDir())
	err := Archive(codeclarity.Analysis{Branch: "../../escape"}, codeclarity.Project{Id: uuid.New()}, uuid.New())
	if !errors.Is(err, ErrInvalidRevision) {
		t.Fatalf("Archive = %v, want ErrInvalidRevision", err)
	}
}
