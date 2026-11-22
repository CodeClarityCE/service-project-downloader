package main

import (
	"fmt"
	"regexp"
	"strings"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
)

// commitPattern matches an abbreviated or full SHA-1 or SHA-256 commit id.
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// hasCommit reports whether the analysis pins a commit rather than a branch tip.
func hasCommit(analysis codeclarity.Analysis) bool {
	return analysis.Commit != "" && analysis.Commit != " "
}

// validateRevision rejects a commit or branch that git could read as an option or
// that could leave the project directory once used as a path.
func validateRevision(analysis codeclarity.Analysis, branch string) error {
	if hasCommit(analysis) {
		if !commitPattern.MatchString(analysis.Commit) {
			return fmt.Errorf("%w: commit %q is not a commit id", ErrInvalidRevision, analysis.Commit)
		}
		return nil
	}
	if !validBranch(branch) {
		return fmt.Errorf("%w: branch %q is not a valid branch name", ErrInvalidRevision, branch)
	}
	return nil
}

// validBranch applies the git check-ref-format rules for branch names.
func validBranch(name string) bool {
	if name == "" || name == "@" || strings.HasPrefix(name, "-") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.ContainsFunc(name, func(r rune) bool {
		return r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r)
	}) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
