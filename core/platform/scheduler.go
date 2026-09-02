package platform

import "errors"

// ErrDaemonUnsupported is returned by the daemon functions on OSes where a
// built-in scheduler integration is not implemented yet. `watch` works
// everywhere; on those OSes, schedule `gem-usage-lens ingest` with the native
// scheduler (cron / systemd timer / Task Scheduler) manually.
var ErrDaemonUnsupported = errors.New(
	"daemon scheduling is not built-in on this OS yet — run `gem-usage-lens watch`, " +
		"or schedule `gem-usage-lens ingest` via cron / systemd / Task Scheduler")

// DaemonInfo describes the installed (or would-be) periodic-ingest service.
type DaemonInfo struct {
	Kind       string // e.g. "launchd"
	Label      string // service identifier
	ConfigPath string // where the service config lives
	Loaded     bool   // whether it is currently registered/loaded
}

// RenderDaemonConfig returns the scheduler config that would run
// `<binPath> ingest` every intervalSec seconds, without installing anything.
func RenderDaemonConfig(binPath string, intervalSec int) (string, error) {
	return renderDaemonConfig(binPath, intervalSec)
}

// InstallDaemon writes and registers the periodic-ingest service.
func InstallDaemon(binPath string, intervalSec int) (DaemonInfo, error) {
	return installDaemon(binPath, intervalSec)
}

// UninstallDaemon unregisters and removes the service.
func UninstallDaemon() (DaemonInfo, error) { return uninstallDaemon() }

// DaemonStatus reports whether the service is installed/loaded.
func DaemonStatus() (DaemonInfo, error) { return daemonStatus() }
