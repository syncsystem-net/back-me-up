//go:build windows

package handlers

import (
	"fmt"
	"os/exec"
	"strings"
)

func openFolderDialog() (string, error) {
	// TopMost owner form forces the dialog in front of the browser window.
	script := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`$f = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$f.Description = 'Select folder to backup'; ` +
		`$f.ShowNewFolderButton = $false; ` +
		`$owner = New-Object System.Windows.Forms.Form; ` +
		`$owner.TopMost = $true; ` +
		`if ($f.ShowDialog($owner) -eq 'OK') { Write-Output $f.SelectedPath }`

	cmd := exec.Command("powershell",
		"-NoProfile", "-STA", "-ExecutionPolicy", "Bypass",
		"-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("folder picker: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
