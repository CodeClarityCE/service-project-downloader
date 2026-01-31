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
)

// Archive extracts uploaded archives to the project directory.
// It handles both ZIP and TAR.GZ formats.
// Files are extracted to the same destination structure as Git clones:
// {DOWNLOAD_PATH}/{organization_id}/projects/{project_id}/{branch}
func Archive(analysis codeclarity.Analysis, project codeclarity.Project, organization uuid.UUID) error {
	path := os.Getenv("DOWNLOAD_PATH")
	if path == "" {
		path = "/private"
	}

	// Find the uploaded archive file
	// Files are stored at: {DOWNLOAD_PATH}/{user_id}/{project_id}/{filename}
	sourcePath, err := findUploadedArchive(path, project)
	if err != nil {
		return fmt.Errorf("failed to find uploaded archive: %w", err)
	}

	log.Printf("Found uploaded archive at: %s", sourcePath)

	// Destination: same structure as Git clones
	// {DOWNLOAD_PATH}/{organization_id}/projects/{project_id}/{branch}
	branch := analysis.Branch
	if branch == "" {
		branch = "main" // Default branch name for FILE projects
	}

	destination := filepath.Join(path, organization.String(), "projects", project.Id.String(), branch)

	// Create destination directory
	if err := os.MkdirAll(destination, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	log.Printf("Extracting archive to: %s", destination)

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

		// Check if this directory contains the project folder
		projectPath := filepath.Join(basePath, entry.Name(), project.Id.String())
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			continue
		}

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

// extractZip extracts a ZIP archive to the destination directory.
func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("failed to open zip file: %w", err)
	}
	defer r.Close()

	// Determine if there's a single top-level directory to strip
	stripPrefix := detectSingleRootDir(r.File)

	for _, f := range r.File {
		// Get the path, potentially stripping the root directory
		fpath := f.Name
		if stripPrefix != "" && strings.HasPrefix(fpath, stripPrefix) {
			fpath = strings.TrimPrefix(fpath, stripPrefix)
			if fpath == "" {
				continue // Skip the root directory itself
			}
		}

		fpath = filepath.Join(dest, fpath)

		// Check for ZipSlip (directory traversal vulnerability)
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", fpath)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, os.ModePerm); err != nil {
				return err
			}
			continue
		}

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		if err != nil {
			return err
		}
	}

	log.Printf("Successfully extracted ZIP archive: %d files", len(r.File))
	return nil
}

// extractTarGz extracts a TAR.GZ archive to the destination directory.
func extractTarGz(src, dest string) error {
	file, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz file: %w", err)
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	// First pass: detect if there's a single root directory
	file.Seek(0, 0)
	gzrDetect, _ := gzip.NewReader(file)
	trDetect := tar.NewReader(gzrDetect)
	stripPrefix := detectSingleRootDirTar(trDetect)
	gzrDetect.Close()

	// Reset and re-read
	file.Seek(0, 0)
	gzr2, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr2.Close()
	tr = tar.NewReader(gzr2)

	fileCount := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Get the path, potentially stripping the root directory
		fpath := header.Name
		if stripPrefix != "" && strings.HasPrefix(fpath, stripPrefix) {
			fpath = strings.TrimPrefix(fpath, stripPrefix)
			if fpath == "" {
				continue // Skip the root directory itself
			}
		}

		fpath = filepath.Join(dest, fpath)

		// Check for directory traversal vulnerability
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("illegal file path: %s", fpath)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(fpath, os.ModePerm); err != nil {
				return err
			}
		case tar.TypeReg:
			// Create parent directories
			if err := os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
				return err
			}

			outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
			fileCount++
		}
	}

	log.Printf("Successfully extracted TAR.GZ archive: %d files", fileCount)
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
