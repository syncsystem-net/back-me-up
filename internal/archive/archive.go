package archive

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func Zip(srcDir string) (string, error) {
	srcDir = filepath.Clean(srcDir)
	baseName := filepath.Base(srcDir)
	zipPath := filepath.Join(filepath.Dir(srcDir), baseName+".zip")

	zipFile, err := os.Create(zipPath)
	if err != nil {
		return "", fmt.Errorf("creating zip file: %w", err)
	}

	w := zip.NewWriter(zipFile)

	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		entryName := filepath.ToSlash(filepath.Join(baseName, rel))

		fw, err := w.Create(entryName)
		if err != nil {
			return fmt.Errorf("creating zip entry %s: %w", entryName, err)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", path, err)
		}
		defer f.Close()

		if _, err := io.Copy(fw, f); err != nil {
			return fmt.Errorf("writing file %s to zip: %w", path, err)
		}
		return nil
	})

	if walkErr != nil {
		w.Close()
		zipFile.Close()
		os.Remove(zipPath)
		return "", fmt.Errorf("walking source directory: %w", walkErr)
	}

	if err := w.Close(); err != nil {
		zipFile.Close()
		os.Remove(zipPath)
		return "", fmt.Errorf("closing zip writer: %w", err)
	}

	if err := zipFile.Close(); err != nil {
		os.Remove(zipPath)
		return "", fmt.Errorf("closing zip file: %w", err)
	}

	return zipPath, nil
}
