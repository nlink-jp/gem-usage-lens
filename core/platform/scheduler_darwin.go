//go:build darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const daemonLabel = "com.nlink-jp.gem-usage-lens"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", daemonLabel+".plist"), nil
}

func daemonLogPath() string {
	d, err := DataDir()
	return resolveDaemonLogPath(d, err)
}

// resolveDaemonLogPath picks the daemon log location. It prefers the per-user
// data dir; the fallback uses os.TempDir() (on macOS the per-user $TMPDIR under
// /var/folders) rather than the world-writable, sticky /tmp, which would be open
// to a symlink/pre-creation race on a shared machine. Pure for testability.
func resolveDaemonLogPath(dataDir string, dataDirErr error) string {
	if dataDirErr == nil && dataDir != "" {
		return filepath.Join(dataDir, "daemon.log")
	}
	return filepath.Join(os.TempDir(), "gem-usage-lens-daemon.log")
}

func renderDaemonConfig(binPath string, intervalSec int) (string, error) {
	log := daemonLogPath()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>ingest</string>
	</array>
	<key>StartInterval</key>
	<integer>%d</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, daemonLabel, binPath, intervalSec, log, log), nil
}

func installDaemon(binPath string, intervalSec int) (DaemonInfo, error) {
	p, err := plistPath()
	if err != nil {
		return DaemonInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return DaemonInfo{}, err
	}
	content, err := renderDaemonConfig(binPath, intervalSec)
	if err != nil {
		return DaemonInfo{}, err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return DaemonInfo{}, err
	}
	// Reload: unload any previous version (ignore error), then load.
	_ = exec.Command("launchctl", "unload", p).Run()
	if out, err := exec.Command("launchctl", "load", p).CombinedOutput(); err != nil {
		return DaemonInfo{Kind: "launchd", Label: daemonLabel, ConfigPath: p}, fmt.Errorf("launchctl load: %v: %s", err, out)
	}
	return DaemonInfo{Kind: "launchd", Label: daemonLabel, ConfigPath: p, Loaded: true}, nil
}

func uninstallDaemon() (DaemonInfo, error) {
	p, err := plistPath()
	if err != nil {
		return DaemonInfo{}, err
	}
	_ = exec.Command("launchctl", "unload", p).Run()
	info := DaemonInfo{Kind: "launchd", Label: daemonLabel, ConfigPath: p, Loaded: false}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return info, err
	}
	return info, nil
}

func daemonStatus() (DaemonInfo, error) {
	p, err := plistPath()
	if err != nil {
		return DaemonInfo{}, err
	}
	info := DaemonInfo{Kind: "launchd", Label: daemonLabel, ConfigPath: p}
	if _, err := os.Stat(p); err == nil {
		// `launchctl list <label>` succeeds (exit 0) when the job is loaded.
		info.Loaded = exec.Command("launchctl", "list", daemonLabel).Run() == nil
	}
	return info, nil
}
