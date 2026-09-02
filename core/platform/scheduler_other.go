//go:build !darwin

package platform

// Windows and Linux daemon integration is not implemented yet (would use Task
// Scheduler / systemd user timers, which can't be verified on real hardware from
// this project). `watch` works everywhere; schedule `ingest` manually otherwise.

func renderDaemonConfig(binPath string, intervalSec int) (string, error) {
	return "", ErrDaemonUnsupported
}

func installDaemon(binPath string, intervalSec int) (DaemonInfo, error) {
	return DaemonInfo{}, ErrDaemonUnsupported
}

func uninstallDaemon() (DaemonInfo, error) { return DaemonInfo{}, ErrDaemonUnsupported }

func daemonStatus() (DaemonInfo, error) { return DaemonInfo{}, ErrDaemonUnsupported }
