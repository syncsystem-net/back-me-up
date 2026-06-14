//go:build windows

package handlers

import (
	"fmt"
	"os/exec"
	"strings"
)

func openFolderDialog() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`$f = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$f.Description = 'Select folder to backup'; ` +
		`$f.ShowNewFolderButton = $false; ` +
		`if ($f.ShowDialog() -eq 'OK') { Write-Output $f.SelectedPath }`

	cmd := exec.Command("powershell",
		"-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden",
		"-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("folder picker: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
