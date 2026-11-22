package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	codeclarity "github.com/CodeClarityCE/utility-types/codeclarity_db"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// archiveRoot returns the base directory for FILE projects.
func archiveRoot(downloadPath string) string {
	if downloadPath == "" {
		return "/private"
	}
	return downloadPath
}

// archiveBranch names the directory a FILE project is extracted into.
func archiveBranch(analysis codeclarity.Analysis) string {
	if analysis.Branch == "" {
		return "main"
	}
	return analysis.Branch
}

// Archive extracts uploaded archives to the project directory.
// It handles both ZIP and TAR.GZ formats.
// Files are extracted to the same destination structure as Git clones:
// {DOWNLOAD_PATH}/{organization_id}/projects/{project_id}/{branch}
func Archive(analysis codeclarity.Analysis, project codeclarity.Project, organization uuid.UUID) error {
	path := archiveRoot(os.Getenv("DOWNLOAD_PATH"))

	// Find the uploaded archive file
	// Files are stored at: {DOWNLOAD_PATH}/{user_id}/{project_id}/{filename}
	sourcePath, err := findUploadedArchive(path, project)
	if err != nil {
		return fmt.Errorf("failed to find uploaded archive: %w", err)
	}

	log.Printf("Found uploaded archive at: %s", sourcePath)

	// Destination: same structure as Git clones
	// {DOWNLOAD_PATH}/{organization_id}/projects/{project_id}/{branch}
	branch := archiveBranch(analysis)

	destination := filepath.Join(path, organization.String(), "projects", project.Id.String(), branch)

	// Create destination directory
	if err := os.MkdirAll(destination, sharedDirMode); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	logrus.WithField("destination", destination).Info("Extracting archive")

	// Detect format and extract
	lowerPath := strings.ToLower(sourcePath)
	if strings.HasSuffix(lowerPath, ".zip") {
		return extractZip(sourcePath, destination)
	} else if strings.HasSuffix(lowerPath, ".tar.gz") || strings.HasSuffix(lowerPath, ".tgz") {
		return extractTarGz(sourcePath, destination)
	}

	return fmt.Errorf("unsupported archive format: %s", sourcePath)
}

// findUploadedArchive searches for the uploaded archive file in the project directory.
// Files are stored at: {DOWNLOAD_PATH}/{user_id}/{project_id}/{filename}
// Since the Project struct doesn't have user_id, we search all user directories.
func findUploadedArchive(basePath string, project codeclarity.Project) (string, error) {
	// Search all directories for the project folder
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return "", fmt.Errorf("failed to read base path: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip the "projects" directory (used for extracted files)
		if entry.Name() == "projects" {
			continue
		}

		// A user directory without this project's folder fails the read and is skipped.
		projectPath := filepath.Join(basePath, entry.Name(), project.Id.String())
		archivePath, err := findArchiveInDirectory(projectPath)
		if err == nil {
			return archivePath, nil
		}
	}

	return "", fmt.Errorf("no archive found for project %s", project.Id.String())
}

// findArchiveInDirectory searches for archive files in a directory.
func findArchiveInDirectory(dirPath string) (string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", err
	}

	archiveExtensions := []string{".zip", ".tar.gz", ".tgz"}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := strings.ToLower(entry.Name())
		for _, ext := range archiveExtensions {
			if strings.HasSuffix(name, ext) {
				return filepath.Join(dirPath, entry.Name()), nil
			}
		}
	}

	return "", fmt.Errorf("no archive found in directory %s", dirPath)
}

// sharedDirMode is the mode of directories on the download volume, which plugin
// containers running as other users read.
const sharedDirMode os.FileMode = 0o755

// extraction writes archive entries under one root, so no entry can land outside it.
type extraction struct {
	root  *os.Root
	files int
}

// entryPath strips the single root directory and rejects names that leave the destination.
func entryPath(name, stripPrefix string) (string, bool, error) {
	if stripPrefix != "" {
		name = strings.TrimPrefix(name, stripPrefix)
	}
	name = strings.TrimLeft(name, "/")
	if name == "" {
		return "", false, nil
	}
	rel := filepath.FromSlash(name)
	if !filepath.IsLocal(rel) {
		return "", false, fmt.Errorf("illegal file path: %s", name)
	}
	return rel, true, nil
}

func (x *extraction) dir(rel string) error {
	return x.root.MkdirAll(rel, sharedDirMode)
}

// file copies r to rel.
func (x *extraction) file(rel string, mode os.FileMode, r io.Reader) error {
	if err := x.root.MkdirAll(filepath.Dir(rel), sharedDirMode); err != nil {
		return err
	}
	out, err := x.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	x.files++
	return closeErr
}

// extractZip extracts a ZIP archive to the destination directory.
func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer func() { _ = r.Close() }()

	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	x := &extraction{root: root}

	// Determine if there's a single top-level directory to strip
	stripPrefix := detectSingleRootDir(r.File)

	for _, f := range r.File {
		rel, ok, err := entryPath(f.Name, stripPrefix)
		if err != nil {
			return err
		}
		if !ok {
			continue // Skip the root directory itself
		}
		if f.FileInfo().IsDir() {
			if err := x.dir(rel); err != nil {
				return err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = x.file(rel, f.Mode(), rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}

	log.Printf("Successfully extracted ZIP archive: %d files", len(r.File))
	return nil
}

// tarRootPrefix reads the archive once to find a single root directory to strip,
// then rewinds the file.
func tarRootPrefix(file *os.File) (string, error) {
	gzr, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	prefix := detectSingleRootDirTar(tar.NewReader(gzr))
	_ = gzr.Close()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return prefix, nil
}

// extractTarGz extracts a TAR.GZ archive to the destination directory.
func extractTarGz(src, dest string) error {
	file, err := os.Open(src) //nolint:gosec // G304: path found by listing the upload directory
	if err != nil {
		return fmt.Errorf("failed to open tar.gz file: %w", err)
	}
	defer func() { _ = file.Close() }()

	stripPrefix, err := tarRootPrefix(file)
	if err != nil {
		return err
	}
	gzr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()
	tr := tar.NewReader(gzr)

	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	x := &extraction{root: root}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		rel, ok, err := entryPath(header.Name, stripPrefix)
		if err != nil {
			return err
		}
		if !ok {
			continue // Skip the root directory itself
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := x.dir(rel); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := x.file(rel, header.FileInfo().Mode(), tr); err != nil {
				return err
			}
		}
	}

	logrus.WithField("files", x.files).Info("Extracted TAR.GZ archive")
	return nil
}

// detectSingleRootDir checks if all files in a ZIP are under a single root directory.
// If so, returns that directory name to be stripped during extraction.
func detectSingleRootDir(files []*zip.File) string {
	if len(files) == 0 {
		return ""
	}

	var rootDir string
	for _, f := range files {
		parts := strings.Split(f.Name, "/")
		if len(parts) < 2 {
			return "" // File at root level
		}

		if rootDir == "" {
			rootDir = parts[0]
		} else if parts[0] != rootDir {
			return "" // Multiple root directories
		}
	}

	if rootDir != "" {
		return rootDir + "/"
	}
	return ""
}

// detectSingleRootDirTar checks if all files in a TAR are under a single root directory.
func detectSingleRootDirTar(tr *tar.Reader) string {
	var rootDir string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ""
		}

		parts := strings.Split(header.Name, "/")
		if len(parts) < 2 {
			return "" // File at root level
		}

		if rootDir == "" {
			rootDir = parts[0]
		} else if parts[0] != rootDir {
			return "" // Multiple root directories
		}
	}

	if rootDir != "" {
		return rootDir + "/"
	}
	return ""
}
