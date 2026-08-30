package archive

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RemoteName is the clean cloud filename for a backup of srcDir: the directory's
// base name plus ".zip". It is stable across repeat backups of the same
// directory (unlike the local temp zip, which is uniquely suffixed), so it is
// what conflict detection checks for and what the worker uploads under.
//
// A split backup uses VolumeName instead, one name per volume.
func RemoteName(srcDir string) string {
	return filepath.Base(filepath.Clean(srcDir)) + ".zip"
}

// Zip compresses srcDir into a temp zip created at the same level as srcDir and
// returns the local path. The file name carries a random suffix so that backing
// up the same directory twice cannot clobber a zip an in-flight upload is still
// reading; the clean cloud name is provided separately by RemoteName.
func Zip(srcDir string) (string, error) {
	return ZipItems(srcDir, []Item{{Rel: ".", IsDir: true}})
}

// ZipItems compresses just the named items of srcDir into a temp zip beside it.
// It is how one volume of a split backup is produced: every entry keeps the
// source directory's name as its prefix, exactly as a whole-directory zip does,
// so extracting all of a backup's volumes into one place reconstructs the
// original tree — and so the ZIP tree reader that Auto-Sync uses sees the same
// shape it would from an unsplit archive.
//
// An item Rel of "." means the whole directory, which is what Zip passes.
func ZipItems(srcDir string, items []Item) (string, error) {
	srcDir = filepath.Clean(srcDir)
	baseName := filepath.Base(srcDir)
	zipPath := filepath.Join(filepath.Dir(srcDir), baseName+"-"+uniqueToken()+".zip")

	zipFile, err := os.Create(zipPath)
	if err != nil {
		return "", fmt.Errorf("creating zip file: %w", err)
	}

	w := zip.NewWriter(zipFile)

	abandon := func(err error) (string, error) {
		w.Close()
		zipFile.Close()
		os.Remove(zipPath)
		return "", err
	}

	for _, item := range items {
		target := srcDir
		if relKey(item.Rel) != "." {
			target = filepath.Join(srcDir, item.Rel)
		}

		// A failure here fails the whole backup, deliberately: unlike the planning
		// walk (which tolerates an unreadable entry because it is only estimating
		// sizes), an archive that silently omits files is worse than no archive.
		walkErr := filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
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
			return abandon(fmt.Errorf("walking source directory: %w", walkErr))
		}
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

// uniqueToken returns a short random hex string for temp-zip naming. It falls
// back to a fixed token only if the system RNG fails, which is effectively never.
func uniqueToken() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "tmp"
	}
	return hex.EncodeToString(b)
}
