package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, body, link string
	dir              bool
}

func writeZip(t *testing.T, path string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		switch {
		case e.dir:
			h.SetMode(os.ModeDir | 0o755)
		case e.link != "":
			h.SetMode(os.ModeSymlink | 0o777)
			e.body = e.link
		default:
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTarGz(t *testing.T, path string, entries []entry) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.dir {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		if e.link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

type extractor struct {
	name    string
	ext     string
	write   func(*testing.T, string, []entry)
	extract func(string, string) error
}

var extractors = []extractor{
	{"zip", ".zip", writeZip, extractZip},
	{"tar.gz", ".tar.gz", writeTarGz, extractTarGz},
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test reads a file it just extracted
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestExtractStripsSingleRootDirectory(t *testing.T) {
	for _, x := range extractors {
		t.Run(x.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "upload"+x.ext)
			dest := t.TempDir()
			x.write(t, src, []entry{
				{name: "repo/", dir: true},
				{name: "repo/package.json", body: "{}"},
				{name: "repo/src/index.js", body: "x"},
			})
			if err := x.extract(src, dest); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := readFile(t, filepath.Join(dest, "package.json")); got != "{}" {
				t.Errorf("package.json = %q", got)
			}
			if got := readFile(t, filepath.Join(dest, "src", "index.js")); got != "x" {
				t.Errorf("src/index.js = %q", got)
			}
		})
	}
}

func TestExtractKeepsSeveralRootEntries(t *testing.T) {
	for _, x := range extractors {
		t.Run(x.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "upload"+x.ext)
			dest := t.TempDir()
			x.write(t, src, []entry{{name: "package.json", body: "{}"}, {name: "lib/a.js", body: "a"}})
			if err := x.extract(src, dest); err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got := readFile(t, filepath.Join(dest, "lib", "a.js")); got != "a" {
				t.Errorf("lib/a.js = %q", got)
			}
		})
	}
}

func TestExtractRejectsEntriesOutsideDestination(t *testing.T) {
	for _, x := range extractors {
		t.Run(x.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "dest")
			if err := os.Mkdir(dest, 0o750); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(t.TempDir(), "upload"+x.ext)
			x.write(t, src, []entry{{name: "a/../../escaped", body: "x"}, {name: "b", body: "y"}})
			if err := x.extract(src, dest); err == nil {
				t.Error("extract accepted a traversing entry")
			}
			if _, err := os.Stat(filepath.Join(parent, "escaped")); err == nil {
				t.Error("an entry was written outside the destination")
			}
		})
	}
}

func TestExtractDoesNotWriteThroughSymlinks(t *testing.T) {
	for _, x := range extractors {
		t.Run(x.name, func(t *testing.T) {
			outside := t.TempDir()
			dest := t.TempDir()
			src := filepath.Join(t.TempDir(), "upload"+x.ext)
			x.write(t, src, []entry{
				{name: "link", link: outside},
				{name: "link/pwned", body: "x"},
			})
			_ = x.extract(src, dest)
			if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
				t.Error("an entry was written through a symlink outside the destination")
			}
		})
	}
}
