package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// layout writes empty files at the given paths below a new temporary folder.
func layout(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLanguageDetection(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		want    []string
		primary string
	}{
		{"root npm project", []string{"package.json", "package-lock.json"}, []string{"javascript"}, "javascript"},
		{"root composer project", []string{"composer.json", "composer.lock"}, []string{"php"}, "php"},
		{"root manifest without a lockfile", []string{"package.json"}, []string{"javascript"}, "javascript"},
		{"npm projects only in subfolders", []string{"api/package.json", "api/pnpm-lock.yaml", "frontend/package.json", "frontend/package-lock.json"}, []string{"javascript"}, "javascript"},
		{"composer project two levels down", []string{"apps/site/composer.json", "apps/site/composer.lock"}, []string{"php"}, "php"},
		{"subfolder manifest without a lockfile", []string{"api/package.json"}, []string{}, "unknown"},
		{"manifests in dependency and hidden folders", []string{"node_modules/a/package.json", "node_modules/a/package-lock.json", "vendor/b/composer.json", "vendor/b/composer.lock", ".git/c/package.json", ".git/c/yarn.lock"}, []string{}, "unknown"},
		{"manifest deeper than the scan", []string{"a/b/c/d/package.json", "a/b/c/d/yarn.lock"}, []string{}, "unknown"},
		{"nothing", []string{"README.md"}, []string{}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectLanguagesFromRepository(layout(t, tc.files...))
			if !slices.Equal(got.DetectedLanguages, tc.want) {
				t.Errorf("languages = %v, want %v", got.DetectedLanguages, tc.want)
			}
			if got.PrimaryLanguage != tc.primary {
				t.Errorf("primary = %q, want %q", got.PrimaryLanguage, tc.primary)
			}
		})
	}
}
