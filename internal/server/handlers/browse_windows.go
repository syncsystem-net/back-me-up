//go:build windows

package handlers

import (
	"fmt"
	"os/exec"
	"strings"
)

func openFolderDialog() (string, error) {
	// Show a tiny TopMost owner form before calling ShowDialog so the folder
	// picker appears in front of the browser rather than behind it.
	script := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`[System.Windows.Forms.Application]::EnableVisualStyles(); ` +
		`$f = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$f.Description = 'Select folder to backup'; ` +
		`$f.ShowNewFolderButton = $false; ` +
		`$owner = New-Object System.Windows.Forms.Form; ` +
		`$owner.TopMost = $true; ` +
		`$owner.Size = New-Object System.Drawing.Size(1,1); ` +
		`$owner.StartPosition = 'CenterScreen'; ` +
		`$owner.ShowInTaskbar = $false; ` +
		`$owner.Show(); ` +
		`$owner.BringToFront(); ` +
		`[System.Windows.Forms.Application]::DoEvents(); ` +
		`$result = $f.ShowDialog($owner); ` +
		`$owner.Dispose(); ` +
		`if ($result -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $f.SelectedPath }`

	cmd := exec.Command("powershell",
		"-NoProfile", "-STA", "-ExecutionPolicy", "Bypass",
		"-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("folder picker: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
