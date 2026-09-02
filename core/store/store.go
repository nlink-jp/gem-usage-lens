// Package store persists priced records durably so reports are fast and the
// accumulated history outlives the source transcripts.
//
// The implementation uses modernc.org/sqlite (pure Go, no CGO) in WAL mode, so
// a running `watch` and an ad-hoc `report` can touch the DB concurrently.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/nlink-jp/gem-usage-lens/core/model"
)

// Store is the persistence boundary. Implementations must be safe to Open
// from a scheduled `ingest` and a long-running `watch` alike.
type Store interface {
	// Upsert idempotently inserts records keyed by Key. It MUST NOT delete
	// existing rows — that is what makes stored data outlive deletion of the
	// source transcripts. Returns the count newly inserted.
	Upsert(recs []model.PricedRecord) (inserted int, err error)

	// Query returns priced records matching the filter, ordered by timestamp.
	Query(f Filter) ([]model.PricedRecord, error)

	// Reprice recomputes cost_usd for every stored record using price, from the
	// token columns already in the store — no source transcript needed. This is
	// what makes a rate-table change apply to history: ingest is incremental,
	// so already-read bytes are never re-priced otherwise. dryRun reports what
	// would change without writing.
	Reprice(price func(model.UsageRecord) model.Cost, dryRun bool) (RepriceResult, error)

	// IngestState / SetIngestState track how far each source file has been
	// read, so ingest only consumes bytes appended since last time.
	IngestState(path string) (offset int64, ok bool, err error)
	SetIngestState(path string, size, mtime, offset int64) error

	Close() error
}

// RepriceResult summarizes a Reprice pass.
type RepriceResult struct {
	Scanned     int     // rows examined
	Changed     int     // rows whose cost differed (written unless dryRun)
	OldTotalUSD float64 // sum of cost_usd before
	NewTotalUSD float64 // sum of cost_usd after
}

// Filter constrains a Query. Zero values mean "unbounded".
type Filter struct {
	Since  int64        // unix seconds; 0 = no lower bound
	Until  int64        // unix seconds; 0 = no upper bound
	Source model.Source // "" = every call source
}

const schema = `
CREATE TABLE IF NOT EXISTS usage_records (
  record_key      TEXT PRIMARY KEY,
  ts              INTEGER,
  host            TEXT,
  session_id      TEXT,
  project         TEXT,
  model           TEXT,
  source          TEXT,
  location        TEXT,
  prompt_tokens   INTEGER,
  output_tokens   INTEGER,
  thoughts_tokens INTEGER,
  cached_tokens   INTEGER,
  total_tokens    INTEGER,
  checksum_ok     INTEGER,
  partial         INTEGER,
  cost_usd        REAL,
  ingested_at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_usage_ts      ON usage_records(ts);
CREATE INDEX IF NOT EXISTS idx_usage_source  ON usage_records(source);
CREATE INDEX IF NOT EXISTS idx_usage_model   ON usage_records(model);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage_records(session_id);

CREATE TABLE IF NOT EXISTS ingest_state (
  path        TEXT PRIMARY KEY,
  size        INTEGER,
  mtime       INTEGER,
  last_offset INTEGER,
  updated_at  INTEGER
);
`

type sqliteStore struct {
	db *sql.DB
}

// Store file permissions. The DB holds metadata (project paths, timestamps)
// that is personal, so it is kept owner-only: the data dir is 0700 (which also
// shields the WAL/SHM sidecars) and the DB file is 0600.
const (
	dirPerms    os.FileMode = 0o700
	dbFilePerms os.FileMode = 0o600
)

// Open opens (creating if absent) the SQLite store at path, enabling WAL
// mode and creating the schema.
func Open(path string) (Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, dirPerms); err != nil {
			return nil, err
		}
		_ = os.Chmod(dir, dirPerms) // tighten a pre-existing dir too; best-effort
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		_ = os.Chmod(path, dbFilePerms)
	}
	return &sqliteStore{db: db}, nil
}

// addedColumns are columns introduced after the first release. `CREATE TABLE
// IF NOT EXISTS` leaves an existing table untouched, so a new column must
// also be listed here (idempotent `ALTER TABLE`, run on every Open). Reads of
// a migrated column need COALESCE, since old rows are NULL. A migration cannot
// backfill from the transcripts: ingest is incremental and the bytes are
// already consumed. Empty in v0.1; the mechanism is here so the first schema
// change does not have to invent it.
var addedColumns = []struct{ name, decl string }{}

func migrate(db *sql.DB) error {
	if len(addedColumns) == 0 {
		return nil
	}
	rows, err := db.Query("PRAGMA table_info(usage_records)")
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notNull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range addedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec("ALTER TABLE usage_records ADD COLUMN " + c.name + " " + c.decl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	return nil
}

const upsertSQL = `INSERT INTO usage_records
 (record_key, ts, host, session_id, project, model, source, location,
  prompt_tokens, output_tokens, thoughts_tokens, cached_tokens, total_tokens,
  checksum_ok, partial, cost_usd, ingested_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(record_key) DO NOTHING`

func (s *sqliteStore) Upsert(recs []model.PricedRecord) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(upsertSQL)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	inserted := 0
	for _, r := range recs {
		if r.Key == "" {
			tx.Rollback()
			return inserted, fmt.Errorf("record without a key (session %s)", r.SessionID)
		}
		var tsUnix int64
		if !r.Timestamp.IsZero() {
			tsUnix = r.Timestamp.Unix()
		}
		res, err := stmt.Exec(
			r.Key, tsUnix, r.Host, r.SessionID, r.Project, r.Model, string(r.Source), r.Location,
			r.Usage.Prompt, r.Usage.Output, r.Usage.Thoughts, r.Usage.Cached, r.Usage.Total,
			boolInt(r.Usage.ChecksumOK()), boolInt(r.Partial), r.Cost.ListPriceUSD, now,
		)
		if err != nil {
			tx.Rollback()
			return inserted, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, err
	}
	return inserted, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const querySelect = `SELECT record_key, ts, host, session_id, project, model, source, location,
 prompt_tokens, output_tokens, thoughts_tokens, cached_tokens, total_tokens, partial, cost_usd
 FROM usage_records WHERE 1=1`

func (s *sqliteStore) Query(f Filter) ([]model.PricedRecord, error) {
	q := querySelect
	var args []any
	if f.Since > 0 {
		q += " AND ts >= ?"
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		q += " AND ts <= ?"
		args = append(args, f.Until)
	}
	if f.Source != "" {
		q += " AND source = ?"
		args = append(args, string(f.Source))
	}
	q += " ORDER BY ts, record_key"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.PricedRecord
	for rows.Next() {
		var r model.PricedRecord
		var tsUnix int64
		var src string
		var partial int
		if err := rows.Scan(
			&r.Key, &tsUnix, &r.Host, &r.SessionID, &r.Project, &r.Model, &src, &r.Location,
			&r.Usage.Prompt, &r.Usage.Output, &r.Usage.Thoughts, &r.Usage.Cached, &r.Usage.Total,
			&partial, &r.Cost.ListPriceUSD,
		); err != nil {
			return out, err
		}
		r.Source = model.Source(src)
		r.Partial = partial != 0
		if tsUnix > 0 {
			r.Timestamp = time.Unix(tsUnix, 0).UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const repriceSelect = `SELECT record_key, model, source, location,
 prompt_tokens, output_tokens, thoughts_tokens, cached_tokens, total_tokens, cost_usd
 FROM usage_records`

func (s *sqliteStore) Reprice(price func(model.UsageRecord) model.Cost, dryRun bool) (RepriceResult, error) {
	var res RepriceResult

	// Collect first, then write: SQLite dislikes UPDATEs issued while its own
	// SELECT cursor is still open on the same table.
	type change struct {
		key  string
		cost float64
	}
	var changes []change

	rows, err := s.db.Query(repriceSelect)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var rec model.UsageRecord
		var src string
		var old float64
		if err := rows.Scan(
			&rec.Key, &rec.Model, &src, &rec.Location,
			&rec.Usage.Prompt, &rec.Usage.Output, &rec.Usage.Thoughts, &rec.Usage.Cached, &rec.Usage.Total, &old,
		); err != nil {
			rows.Close()
			return res, err
		}
		rec.Source = model.Source(src)
		now := price(rec).ListPriceUSD

		res.Scanned++
		res.OldTotalUSD += old
		res.NewTotalUSD += now
		if now != old {
			res.Changed++
			changes = append(changes, change{rec.Key, now})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	if dryRun || len(changes) == 0 {
		return res, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	stmt, err := tx.Prepare("UPDATE usage_records SET cost_usd = ? WHERE record_key = ?")
	if err != nil {
		tx.Rollback()
		return res, err
	}
	defer stmt.Close()
	for _, c := range changes {
		if _, err := stmt.Exec(c.cost, c.key); err != nil {
			tx.Rollback()
			return res, err
		}
	}
	return res, tx.Commit()
}

func (s *sqliteStore) IngestState(path string) (int64, bool, error) {
	var off int64
	err := s.db.QueryRow("SELECT last_offset FROM ingest_state WHERE path = ?", path).Scan(&off)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return off, true, nil
}

func (s *sqliteStore) SetIngestState(path string, size, mtime, offset int64) error {
	_, err := s.db.Exec(
		`INSERT INTO ingest_state (path, size, mtime, last_offset, updated_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET
		   size=excluded.size, mtime=excluded.mtime,
		   last_offset=excluded.last_offset, updated_at=excluded.updated_at`,
		path, size, mtime, offset, time.Now().Unix(),
	)
	return err
}

func (s *sqliteStore) Close() error { return s.db.Close() }
