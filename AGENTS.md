# AGENTS.md — gem-usage-lens

## What this is

A util-series CLI that parses **gem-agent** session transcripts (Vertex AI
Gemini) to collect token usage, price it at the Vertex AI list price,
accumulate it in a durable SQLite store, and report it by day / session /
project / model / call source, plus a calendar-month budget with a pace
forecast. The counterpart of `claude-usage-lens`, forked from its skeleton.
The menu-bar front-end is the separate `gem-usage-lens-gui` (SwiftUI), which
only calls this CLI's `--json` outputs.

Current state: every command works end-to-end on the author's real data
(`verify` reports 0 checksum mismatches): `ingest`, `reprice`, `report`,
`budget`, `sessions`, `models`, `verify`, `doctor`, `watch`, `daemon`.

## Build & test

```sh
make build      # → dist/gem-usage-lens  (NEVER `go build` directly)
make test       # go test ./...
make vet        # go vet on host + GOOS=windows + GOOS=linux
make build-all  # cross-compile, CGO-free
make package    # archives + notarize the darwin zip
make verify-release
```

Go 1.26+. No CGO, no external services, no network at runtime.

## Structure

```
main.go                 thin entry → cmd.Execute
cmd/                    stdlib-flag dispatch (cmd.go), commands.go, budget.go [tested]
core/
  model/                UsageRecord{Key, Source, Location, Partial, Usage{Prompt,Output,Thoughts,Cached,Total}}
  collect/              Discover, ReadHeader, ParseFrom (offset continuation, legacy fill-in) [tested]
  pricing/              Rates + Default() + VerifiedOn + Lookup [tested]
  cost/                 pure cost engine [tested]
  aggregate/            group-by / dense / sort / Summary(+unpriced, partial) [tested]
  budget/               MonthWindow / Consume / Project / Build (pure) [tested]
  store/                SQLite (modernc.org/sqlite), record_key PK, ingest_state [tested]
  ingest/               collect → price → store [tested]
  config/               strict TOML: [sources] [pricing.models] [budget] [tested]
  platform/             sessions root, config search paths, data dir, launchd [tested]
docs/{en,ja}/           RFP (canonical design)
```

## Conventions & deliberate choices (gotchas)

- **Accounting rules come from gem-agent ADR-0057 and are pinned by tests.**
  `thoughts` bills at the output price; `cached` is a *share* of `prompt`
  (subtract it, never add it); `prompt + output + thoughts + tool_prompt ==
  total` is the checksum. `cost_test.go` pins each rule separately. If the sum
  ever fails on real data, this build misreads the transcript — `verify` exits 1.
- **`tool_prompt` is the bucket gem-agent does not write.** The API defines
  `totalTokenCount` as prompt + candidates + tool_use_prompt + thoughts
  (genai `GenerateContentResponseUsageMetadata`); `toolUsePromptTokenCount`
  is the results of built-in tools (search grounding, URL context) fed back
  as input. v0.1.0 flagged such records as checksum failures. Since v0.1.1
  `Usage.WithDerivedToolPrompt` fills it from the remainder — exact, because
  it is the only unwritten bucket — at parse time and on every store read
  (`Query`, `Reprice`), billed at the input price. A `tool_prompt` field in a
  future transcript is read as written. A total *below* the buckets stays a
  mismatch. Store column `tool_prompt_tokens` was added via `addedColumns`;
  `reprice` persists the derived value and the restored checksum verdict.
- **Dedup key = `<rel path>@<byte offset>`.** Records carry no message id.
  Transcripts are append-only and a resume appends to the same file, so the
  offset is stable. `ParseFrom` advances only past complete lines: a torn
  last line (gem-agent mid-write) is re-read next pass, never counted twice.
  The header is re-read on every incremental pass (legacy records take their
  model from it).
- **Legacy transcripts (before gem-agent v0.55 / 2026-08-30) are partial, and say so.** A
  `usage` record without `source` is a pre-0057 main-loop round: source=main,
  model from the header, `partial=true`. Their files never recorded risk /
  compaction spend. Legacy side-call records with a model (`summary_usage`)
  are taken as a lower bound; those without (`web_search` etc.) are counted
  as `skipped`, not guessed. The `partial` column flows to every report,
  `budget` and the GUI.
- **Prices are per model, per field — never constants in the engine.**
  `cache_read_multiplier`, `grounding_per_req`, `non_global_multiplier` sit on
  `Rates`; the engine reads the record's model's values. `VerifiedOn` is the
  date the table was checked against the pricing page, and the table carries
  **no scheduled future prices** (the page lists 2027 prices for 3.7 Flash —
  re-check at the next sync, don't pre-write). Tiered models (3.1 Pro) are
  deliberately absent: a flat rate would under-count long prompts.
- **Region**: header `location` other than `global` → ×1.1. An empty location
  (pre-0057 header) is treated as global, not surcharged.
- **Grounding** is a per-request charge invisible in tokens: added once per
  `source=web_search` record. `web_fetch` (URL context) has no extra charge.
- **A model missing from the table costs $0, silently at the engine — never
  silently at the surface.** `ingest` / `reprice` warn on stderr;
  `report --summary` derives `unpriced_records` / `unpriced_models` from the
  stored rows alone (so "priced now, not yet repriced" is caught too); the
  map is never nil (`{}` in JSON — GUI contract). Fixing is two steps: price
  the model (config or table), then `reprice`, because ingest never re-reads
  consumed bytes.
- **Budget arithmetic lives here, once.** `core/budget` is pure and fully
  tested; `budget --json` is the GUI's only source of window / used / state /
  forecast. The GUI stores settings and passes them as flags — it does not
  recompute. Both bases (USD, billed tokens = `total`) are always in the
  payload; an unset limit reports `state: "unset"`. Timestamps are whole
  seconds (`Truncate`) so a plain ISO 8601 decoder reads them.
- **Config search: XDG → ~/.config → Application Support (macOS).** A config
  that exists but is silently unread is worse than none; `doctor` prints the
  path it loaded or every path it searched. Unknown keys are a hard error.
  Override fields are `*float64` so a partial override inherits.
- **Store rows are never deleted by ingest.** History outlives the source.
  Schema changes need an entry in `store.addedColumns` (idempotent ALTER on
  every Open) — `CREATE TABLE IF NOT EXISTS` won't add columns.
- **The dedup key is root-relative.** Changing the sessions root (flag, config
  or `GEMAGENT_STATE_DIR`) re-keys every file and a fresh ingest would double
  count; the documented remedy is deleting `usage.db`. A session id + offset
  key would be root-independent but ids are only unique per project dir.
- **One record is one model call, except `agentic_file_search`**, which
  gem-agent logs as a single summed record for its child rounds: cost is
  linear so the money is right, but `records` under-counts calls there.
- **Concurrent ingests are safe** (WAL + `DO NOTHING`): the GUI's minute
  timer, `watch` and the daemon may all run. `daemon install` records
  `os.Executable()` — install it from the brew / PATH copy, not `dist/`.
- **`daemon` is launchd-only** (darwin build tag); other OSes get
  `ErrDaemonUnsupported` and a `watch` hint. gem-agent is macOS-only anyway.
- **`--version` and `version` print the same line** (the tap formula's test
  greps the flag). Pinned in `cmd_test.go`.
- **JSON contract with gem-usage-lens-gui**: `Row` (report), `Summary`
  (report --summary), `budget.Status`. Change these in lockstep with the
  GUI's `Models.swift`.

## Testing strategy

- Unit tests use synthetic transcripts / rates (PII-free). `parse_test.go`
  covers the modern shape, the legacy shape, checksum mismatch, resume at an
  offset, a torn line, and a shrunk file.
- Real-data E2E before a release: `doctor` → `ingest` → `verify` →
  `report --summary` → `budget --json` on the author's transcripts. `verify`
  must report 0 checksum mismatches.

## Design reference

- [docs/ja/gem-usage-lens-rfp.ja.md](docs/ja/gem-usage-lens-rfp.ja.md) (primary)
- [docs/en/gem-usage-lens-rfp.md](docs/en/gem-usage-lens-rfp.md)
- gem-agent ADR-0057 (accounting records) and ADR-0022 (`GEMAGENT_STATE_DIR`)
