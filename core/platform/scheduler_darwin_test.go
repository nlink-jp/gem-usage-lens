//go:build darwin

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDaemonLogPath(t *testing.T) {
	// Data dir available → log lives beside the store.
	if got := resolveDaemonLogPath("/data/dir", nil); got != "/data/dir/daemon.log" {
		t.Errorf("got %q, want /data/dir/daemon.log", got)
	}
	// Data dir unavailable → per-user os.TempDir(), not the world-writable /tmp.
	got := resolveDaemonLogPath("", errors.New("no data dir"))
	want := filepath.Join(os.TempDir(), "gem-usage-lens-daemon.log")
	if got != want {
		t.Errorf("fallback = %q, want %q (per-user temp dir)", got, want)
	}
}

func TestRenderDaemonConfig(t *testing.T) {
	out, err := renderDaemonConfig("/usr/local/bin/gem-usage-lens", 900)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"com.nlink-jp.gem-usage-lens",
		"<string>/usr/local/bin/gem-usage-lens</string>",
		"<string>ingest</string>",
		"<integer>900</integer>",
		"<key>RunAtLoad</key>",
	} {
		if !contains(out, want) {
			t.Errorf("plist missing %q\n%s", want, out)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
