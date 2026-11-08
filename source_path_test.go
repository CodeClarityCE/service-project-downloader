package main

import (
	"testing"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
)

func TestProjectSourcePath(t *testing.T) {
	org := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	projectId := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	base := "/private/" + org.String() + "/projects/" + projectId.String()
	cases := []struct {
		name     string
		kind     string
		analysis codeclarity.Analysis
		want     string
	}{
		{"file uses the branch directory", "FILE", codeclarity.Analysis{Branch: "dev"}, base + "/dev"},
		{"file defaults to main", "FILE", codeclarity.Analysis{}, base + "/main"},
		{"file ignores a commit", "FILE", codeclarity.Analysis{Branch: "dev", Commit: "abc"}, base + "/dev"},
		{"git uses the commit", "GITHUB", codeclarity.Analysis{Branch: "dev", Commit: "abc"}, base + "/abc"},
		{"git without a commit uses the branch", "GITHUB", codeclarity.Analysis{Branch: "dev", Commit: " "}, base + "/dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := codeclarity.Project{Id: projectId, Type: tc.kind}
			if got := projectSourcePath("/private", org, project, tc.analysis); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
