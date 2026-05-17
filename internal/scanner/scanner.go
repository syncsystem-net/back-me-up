package scanner

import (
	"os"
	"path/filepath"
	"strings"
)

type Entry struct {
	Name      string
	Path      string
	Level     int
	SizeBytes int64
}

func Scan(rootPath string) ([]Entry, error) {
	level1Entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, err
	}

	var entries []Entry

	for _, e1 := range level1Entries {
		if !e1.IsDir() {
			continue
		}

		l1Path := filepath.Join(rootPath, e1.Name())
		l1Size, err := dirImmediateSize(l1Path)
		if err != nil {
			return nil, err
		}

		entries = append(entries, Entry{
			Name:      e1.Name(),
			Path:      e1.Name(),
			Level:     1,
			SizeBytes: l1Size,
		})

		level2Entries, err := os.ReadDir(l1Path)
		if err != nil {
			return nil, err
		}

		for _, e2 := range level2Entries {
			if !e2.IsDir() {
				continue
			}

			l2Path := filepath.Join(l1Path, e2.Name())
			l2Size, err := dirImmediateSize(l2Path)
			if err != nil {
				return nil, err
			}

			entries = append(entries, Entry{
				Name:      e2.Name(),
				Path:      strings.Join([]string{e1.Name(), e2.Name()}, "/"),
				Level:     2,
				SizeBytes: l2Size,
			})
		}
	}

	return entries, nil
}

func dirImmediateSize(dirPath string) (int64, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}
