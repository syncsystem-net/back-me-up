//go:build !windows && !darwin

package handlers

import "fmt"

func openFolderDialog() (string, error) {
	return "", fmt.Errorf("native folder picker not supported on this platform; type the path manually")
}
