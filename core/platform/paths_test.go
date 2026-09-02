package platform

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSessionsRootFollowsGemAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(StateDirEnv, "")
	got, err := SessionsRoot()
	if err != nil || got != filepath.Join(home, ".local", "state", "gem-agent", "sessions") {
		t.Fatalf("%s %v", got, err)
	}
	t.Setenv(StateDirEnv, "/isolated/state")
	if got, _ = SessionsRoot(); got != filepath.Join("/isolated/state", "sessions") {
		t.Fatalf("GEMAGENT_STATE_DIR must be honoured: %s", got)
	}
}

func TestConfigSearchPathsOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	dirs, err := ConfigSearchPaths()
	if err != nil {
		t.Fatal(err)
	}
	if dirs[0] != filepath.Join("/xdg", AppName) || dirs[1] != filepath.Join(home, ".config", AppName) {
		t.Fatalf("%v", dirs)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(dirs[2], "Application Support") {
		t.Fatalf("macOS must also search Application Support: %v", dirs)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	dirs, _ = ConfigSearchPaths()
	if dirs[0] != filepath.Join(home, ".config", AppName) {
		t.Fatalf("without XDG, ~/.config first: %v", dirs)
	}
	p, found, _ := ConfigFilePath()
	if found || p != filepath.Join(home, ".config", AppName, ConfigFileName) {
		t.Fatalf("canonical when absent: %s %v", p, found)
	}
}

func TestDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	d, err := DataDir()
	if err != nil || !strings.HasPrefix(d, home) || !strings.HasSuffix(d, AppName) {
		t.Fatalf("%s %v", d, err)
	}
}
