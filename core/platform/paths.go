// Package platform resolves where gem-agent keeps its transcripts and where
// this tool keeps its config and store. The rest of the codebase depends only
// on these functions — never on hardcoded paths.
package platform

import (
	"os"
	"path/filepath"
	"runtime"
)

// AppName is the directory name used under every config/data root.
const AppName = "gem-usage-lens"

// StateDirEnv is gem-agent's own override for its state root (ADR-0022);
// honouring it here means an isolated gem-agent is measured where it writes.
const StateDirEnv = "GEMAGENT_STATE_DIR"

// SessionsRoot returns the directory gem-agent writes transcripts under:
// `$GEMAGENT_STATE_DIR/sessions` when the variable is set, otherwise
// `~/.local/state/gem-agent/sessions`. This is only a default — the user can
// override it via config [sources] / --sessions-root.
func SessionsRoot() (string, error) {
	if d := os.Getenv(StateDirEnv); d != "" {
		return filepath.Join(d, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "gem-agent", "sessions"), nil
}

// ConfigSearchPaths lists the directories a config.toml is looked for, in
// order: `$XDG_CONFIG_HOME/gem-usage-lens`, `~/.config/gem-usage-lens`, and
// on macOS also `~/Library/Application Support/gem-usage-lens`. CLI users on
// macOS reach for ~/.config first; a file that exists but is silently not
// read is worse than none, so both homes are searched.
func ConfigSearchPaths() ([]string, error) {
	var out []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		out = append(out, filepath.Join(x, AppName))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	out = append(out, filepath.Join(home, ".config", AppName))
	if runtime.GOOS == "darwin" {
		out = append(out, filepath.Join(home, "Library", "Application Support", AppName))
	}
	return out, nil
}

// ConfigFileName is the config file's name inside a search directory.
const ConfigFileName = "config.toml"

// ConfigFilePath returns the first existing config.toml among the search
// paths (found=true), or the canonical location to create one (found=false).
func ConfigFilePath() (path string, found bool, err error) {
	dirs, err := ConfigSearchPaths()
	if err != nil {
		return "", false, err
	}
	for _, d := range dirs {
		p := filepath.Join(d, ConfigFileName)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true, nil
		}
	}
	return filepath.Join(canonicalConfigDir(dirs), ConfigFileName), false, nil
}

// canonicalConfigDir is where a new config should be written: the XDG
// location when the user set one, else ~/.config.
func canonicalConfigDir(dirs []string) string {
	if len(dirs) == 0 {
		return "."
	}
	return dirs[0]
}

// DataDir returns where usage.db lives: `~/Library/Application Support/
// gem-usage-lens` on macOS, `$XDG_DATA_HOME/gem-usage-lens` (default
// `~/.local/share/gem-usage-lens`) elsewhere.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", AppName), nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, AppName), nil
	}
	return filepath.Join(home, ".local", "share", AppName), nil
}
