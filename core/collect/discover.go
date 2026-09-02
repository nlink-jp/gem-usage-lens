// Package collect discovers gem-agent session transcripts and parses their
// accounting records into UsageRecords. It is the only package that reads
// raw JSONL.
package collect

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxEntriesScanned bounds how many filesystem entries Discover will visit,
// as a safety net against a sessions root accidentally set to a filesystem
// root. Far above any real session count; exceeding it is reported as a
// misconfiguration, never a silent truncation. A var so tests can lower it.
var maxEntriesScanned = 1_000_000

// Discover enumerates transcript files (*.jsonl) under root, in walk order.
// A missing root is not an error — gem-agent may simply never have run here.
// The `.project` marker files gem-agent writes beside transcripts are not
// JSONL and are skipped by the suffix test.
func Discover(root string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, nil
	}
	var out []string
	var scanned int
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate unreadable subtrees
		}
		scanned++
		if scanned > maxEntriesScanned {
			return fmt.Errorf("aborting scan of %q after %d entries — is the sessions root misconfigured (e.g. set to a filesystem root)? narrow it via config [sources] or --sessions-root", root, maxEntriesScanned)
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// RelKey returns the transcript's path relative to the sessions root, with
// forward slashes — the stable prefix of every record key from that file.
// Falls back to the base name when the path is not under root.
func RelKey(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}

// SessionID is the transcript's base name without the extension — the id
// gem-agent's `--resume` and `sessions` use.
func SessionID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}
