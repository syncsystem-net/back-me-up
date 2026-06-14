//go:build darwin

package handlers

import (
	"os/exec"
	"strings"
)

func openFolderDialog() (string, error) {
	cmd := exec.Command("osascript", "-e",
		`POSIX path of (choose folder with prompt "Select folder to backup")`)
	out, err := cmd.Output()
	if err != nil {
		// User cancelled the dialog — not an error from the caller's perspective
		return "", nil
	}
	path := strings.TrimSpace(string(out))
	return strings.TrimSuffix(path, "/"), nil
}
