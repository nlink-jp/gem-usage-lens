package collect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

// maxLineBytes bounds one transcript line. Transcripts embed tool output and
// base64 images, so lines can be large; an accounting record never is, but
// the reader has to get past the large ones.
const maxLineBytes = 64 * 1024 * 1024

// Header is the first record of a session file (gem-agent ADR-0005): what
// produced it and — since ADR-0057 §4 — which Vertex location it billed to.
type Header struct {
	Found    bool
	Schema   int
	Version  string
	Model    string
	Project  string
	Location string
}

// FileStats counts what one parse pass saw, for `verify` and `doctor`.
type FileStats struct {
	Records          int // usage records produced
	Legacy           int // main-loop records written before ADR-0057 (no source/model)
	LegacySide       int // legacy side-call records taken as a lower bound (had a model)
	Skipped          int // legacy side-call records dropped (no model to price with)
	ChecksumMismatch int // records where prompt + output + thoughts != total
}

// rawLine mirrors the envelope of every transcript record. Unknown fields
// are ignored by encoding/json — that tolerance is what lets the parser
// survive schema drift.
type rawLine struct {
	TS   string          `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type rawHeader struct {
	Schema   int    `json:"schema"`
	Version  string `json:"version"`
	Model    string `json:"model"`
	Project  string `json:"project"`
	Location string `json:"location"`
}

// rawUsage covers the ADR-0057 accounting record and the legacy side-call
// records (summary_usage etc.), which carry prompt/output and sometimes a
// model but never thoughts/cached/total.
type rawUsage struct {
	Source   string `json:"source"`
	Model    string `json:"model"`
	Prompt   int64  `json:"prompt"`
	Output   int64  `json:"output"`
	Thoughts int64  `json:"thoughts"`
	Cached   int64  `json:"cached"`
	Total    int64  `json:"total"`
}

// ReadHeader reads the session header (the first line). A file whose first
// line is not a session record yields Found=false, not an error: the parse
// still runs, it just cannot fill in a legacy record's model.
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Header{}, err
	}
	return parseHeader(line), nil
}

func parseHeader(line []byte) Header {
	var raw rawLine
	if err := json.Unmarshal(bytes.TrimSpace(line), &raw); err != nil || raw.Kind != "session" {
		return Header{}
	}
	var h rawHeader
	if err := json.Unmarshal(raw.Data, &h); err != nil {
		return Header{}
	}
	return Header{Found: true, Schema: h.Schema, Version: h.Version, Model: h.Model, Project: h.Project, Location: h.Location}
}

// ParseFile parses a whole transcript from the start.
func ParseFile(path, relKey, host string) ([]model.UsageRecord, FileStats, error) {
	recs, st, _, err := ParseFrom(path, relKey, 0, host)
	return recs, st, err
}

// ParseFrom reads a transcript starting at byte offset and returns its usage
// records, the parse statistics, and the new offset to persist. Transcripts
// are append-only JSONL, so a previously recorded offset lands on a line
// boundary; a file that has shrunk below offset (rewritten) is re-read from
// the start. Only complete lines advance the offset: a torn last line — a
// record gem-agent was still writing — is left for the next pass, so it is
// neither lost nor counted twice.
//
// relKey prefixes every record's Key (see model.UsageRecord.Key); host stamps
// provenance. The header is re-read on every pass because a legacy record
// takes its model from it, and an incremental pass never sees line one.
func ParseFrom(path, relKey string, offset int64, host string) (recs []model.UsageRecord, st FileStats, newOffset int64, err error) {
	hdr, err := ReadHeader(path)
	if err != nil {
		return nil, st, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, st, 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, st, 0, err
	}
	size := fi.Size()
	if offset < 0 || offset > size {
		offset = 0
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, st, 0, err
		}
	}
	recs, st, consumed, err := parseReader(f, hdr, relKey, SessionID(path), host, offset)
	return recs, st, offset + consumed, err
}

// parseReader is the testable core of ParseFrom. start is the absolute byte
// offset of the reader's first byte, used to key records; consumed is how many
// bytes of complete lines were processed.
func parseReader(r io.Reader, hdr Header, relKey, sessionID, host string, start int64) (recs []model.UsageRecord, st FileStats, consumed int64, err error) {
	br := bufio.NewReaderSize(r, 256*1024)
	var pos int64
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				// A partial final line (no newline yet) stays unconsumed.
				break
			}
			if errors.Is(rerr, bufio.ErrBufferFull) {
				// Unreachable with ReadBytes (it grows), kept for clarity.
				return recs, st, consumed, rerr
			}
			return recs, st, consumed, rerr
		}
		lineStart := start + pos
		pos += int64(len(line))
		consumed = pos
		if len(line) > maxLineBytes {
			continue
		}
		if rec, ok := parseLine(line, hdr, relKey, sessionID, host, lineStart, &st); ok {
			recs = append(recs, rec)
		}
	}
	return recs, st, consumed, nil
}

// parseLine turns one transcript line into a record when it is an
// accounting record. It also recognises the pre-ADR-0057 shapes.
func parseLine(line []byte, hdr Header, relKey, sessionID, host string, lineStart int64, st *FileStats) (model.UsageRecord, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return model.UsageRecord{}, false
	}
	var raw rawLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return model.UsageRecord{}, false // tolerant: skip malformed lines
	}

	var u rawUsage
	var src model.Source
	var partial bool
	switch raw.Kind {
	case "usage":
		if err := json.Unmarshal(raw.Data, &u); err != nil {
			return model.UsageRecord{}, false
		}
		src = model.Source(u.Source)
		if u.Source == "" {
			// Pre-0057 main-loop round: source and model live in the header.
			src = model.SourceMain
			partial = true
			st.Legacy++
			if u.Model == "" {
				u.Model = hdr.Model
			}
		}
	case "summary_usage", "web_search", "web_fetch", "agentic_search_usage":
		// Since ADR-0057 §3 these are descriptive and carry no tokens; before
		// it they carried prompt/output only. A token-bearing one is legacy.
		if err := json.Unmarshal(raw.Data, &u); err != nil {
			return model.UsageRecord{}, false
		}
		if u.Prompt == 0 && u.Output == 0 {
			return model.UsageRecord{}, false
		}
		if u.Model == "" {
			st.Skipped++ // nothing to price it with; counted, not guessed
			return model.UsageRecord{}, false
		}
		src = legacySideSource(raw.Kind)
		partial = true
		st.LegacySide++
	default:
		return model.UsageRecord{}, false
	}

	usage := model.Usage{Prompt: u.Prompt, Output: u.Output, Thoughts: u.Thoughts, Cached: u.Cached, Total: u.Total}
	if !usage.ChecksumOK() {
		st.ChecksumMismatch++
	}
	st.Records++
	return model.UsageRecord{
		Key:       relKey + "@" + strconv.FormatInt(lineStart, 10),
		Timestamp: parseTime(raw.TS),
		Host:      host,
		SessionID: sessionID,
		Project:   hdr.Project,
		Model:     u.Model,
		Source:    src,
		Location:  hdr.Location,
		Partial:   partial,
		Usage:     usage,
	}, true
}

func legacySideSource(kind string) model.Source {
	switch kind {
	case "summary_usage":
		return model.SourceSummarizeFile
	case "web_search":
		return model.SourceWebSearch
	case "web_fetch":
		return model.SourceWebFetch
	default:
		return model.SourceFileSearch
	}
}

// parseTime parses the record timestamp (RFC 3339 with an offset), returning
// the zero time on failure — the record is still kept; a missing timestamp
// should not drop spend.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
