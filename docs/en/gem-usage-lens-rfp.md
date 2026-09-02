# RFP: gem-usage-lens

> Generated: 2026-09-02
> Status: Approved (CLI + GUI developed together)

## 1. Problem Statement

[gem-agent](https://github.com/nlink-jp/gem-agent) (an agent runtime on Vertex AI
Gemini) is in daily production use and its session turnover is rising. The Vertex AI
API returns token counts and never money; Cloud Billing reports cost per SKU per day,
which cannot be attributed to a session, a turn, or a call. So there is no way to
answer "what did today cost" or "where is this month heading".

Since ADR-0057 (2026-08-30) gem-agent writes **one accounting record (`usage`) per
model call** into the session transcript — `source` (main / risk / compact /
web_search …), `model`, `prompt` / `output` / `thoughts` / `cached` / `total` — and
the session header records the billing region (`location`). That ADR deliberately
leaves the price table and the cost report out of scope: "this buys the *possibility*
of computing cost later". `gem-usage-lens` is the tool that fills that seat.

`gem-usage-lens` parses gem-agent's local transcripts, computes token usage and cost
(Vertex AI list-price equivalent), accumulates it in SQLite, and reports it by day /
session / project / model / call source. **The budget is a calendar month** (Vertex has
no weekly rolling quota like Claude's, and billing is monthly). A GUI
(`gem-usage-lens-gui`, a macOS menu-bar app) ships at the same time. The target user is
an individual developer who wants near-real-time visibility into gem-agent spend and
the pace against a monthly budget.

It is the counterpart of [claude-usage-lens](https://github.com/nlink-jp/claude-usage-lens)
/ [claude-usage-lens-gui](https://github.com/nlink-jp/claude-usage-lens-gui) and forks
their skeleton (SQLite store, incremental ingest, `--json` contract, watch/daemon,
reprice, unpriced-model surfacing).

## 2. Functional Specification

### Commands / API Surface

```
gem-usage-lens ingest            # incremental (idempotent): read only new/appended bytes, upsert into the store
gem-usage-lens reprice           # recompute stored costs after a rate-table change (no transcript re-read)
gem-usage-lens report            # aggregate the store
    --since <date|Nd|today|month>  #   range filter (month = the 1st of this month, 00:00 local)
    --until <date>
    --group-by hour|day|week|month|session|project|model|source  # comma-separated (default day)
    --source main|risk|compact|web_search|...  # call-source filter
    --model / --project            #   substring filters
    --sort key|cost|prompt|output|thoughts|cached|records / --top N
    --dense                        #   fill time-series gaps with zero rows (for the GUI's contiguous charts)
    --summary                      #   period summary (active days, daily avg, peak day, 30-day projection, unpriced)
    --compare                      #   vs. the preceding equal-length period
    --tz local|utc|<IANA>
    --json
gem-usage-lens budget            # calendar-month budget state (used, remaining, threshold state, pace forecast)
    --limit-usd X --limit-tokens N   #   budget (overrides config [budget])
    --warn 80 --critical 95          #   threshold percents
    --tz / --json                    #   --json is the GUI contract
gem-usage-lens sessions          # session list (id/project/model/tokens/cost/time)
gem-usage-lens models            # rate table (with its verification date) and config overrides
gem-usage-lens verify            # transcript accounting checksum (prompt+output+thoughts == total) and partial-file report
gem-usage-lens doctor            # resolved sessions root / store / config paths and whether they exist
gem-usage-lens watch             # poll + incremental ingest with live deltas
gem-usage-lens daemon install|uninstall|status   # periodic ingest via launchd
gem-usage-lens version / --version
```

### Input

**Input (local file reads only, never writes)**

| Item | Value |
|------|-------|
| sessions root | `$GEMAGENT_STATE_DIR/sessions` when set, else `~/.local/state/gem-agent/sessions` (same resolution as gem-agent ADR-0022) |
| transcripts | `<root>/**/*.jsonl` (currently `projects/<escaped-path>/<YYYYMMDD-HHMMSS>.jsonl`) |
| session id | the file's base name (what gem-agent's `--resume <id>` takes) |

Transcript accounting records (gem-agent ADR-0057):

```
{"ts":"2026-09-01T00:31:43.9+09:00","kind":"session","data":{"schema":2,"version":"v0.58.0","model":"gemini-3.7-flash","project":"/path","location":"global"}}
{"ts":"...","kind":"usage","data":{"source":"main","model":"gemini-3.7-flash","prompt":57068,"output":49,"thoughts":57,"cached":0,"total":57174}}
```

Accounting rules (measured and fixed in ADR-0057):

- `thoughts` is a **separate bucket** from `output` and **bills at the output price**.
- `cached ⊆ prompt`: cached is the discounted *share* of the prompt, not an addition.
- `prompt + output + thoughts == total` is the API's own checksum.

Legacy form (before ADR-0057, i.e. gem-agent before 2026-08-30):

- A `usage` record with no `source` / `model` → `source=main`, `model` from the header.
  Risk / compaction spend was never written, so the file is reported as **partial**.
- Legacy side-call records (`summary_usage` / `web_search` / `web_fetch` /
  `agentic_search_usage`) carry `prompt` / `output` only. Those with a `model`
  (`summary_usage`) are ingested as a lower bound (thoughts=0, cached=0); those without
  are not ingested and are counted among the reasons the file is partial.

### Dedup / incremental ingest

Records have no message id. Transcripts are append-only JSONL and a resume appends to
the same file, so the **dedup key is `<session path>@<byte offset of the line>`**, and
`ingest_state(path, last_offset)` reads only what was appended. The header (first line)
is re-read on every incremental pass (cheap). Only complete lines advance the offset,
so a torn last line is re-read next time.

### Output

Formatted table on stdout by default; `--json` for structured output. A `report --json`
row:

```json
{"key":"2026-09-01","records":12,"prompt_tokens":1,"output_tokens":1,"thoughts_tokens":1,
 "cached_tokens":1,"total_tokens":3,"cost_usd":0.01,"partial_records":0}
```

`report --summary --json` carries `unpriced_records` / `unpriced_models` (rows that hold
tokens at cost 0 — the trace of a model missing from the rate table; the GUI's badge
contract).

`budget --json` (GUI contract):

```json
{"window_start":"2026-09-01T00:00:00+09:00","window_end":"2026-10-01T00:00:00+09:00",
 "next_reset":"2026-10-01T00:00:00+09:00","elapsed_fraction":0.05,
 "cost":{"limit":100,"used":12.3,"remaining":87.7,"percent":12.3,"state":"normal",
         "forecast":{"projected":246,"percent":246,"exhaustion_at":"...","reliable":true,"state":"critical"}},
 "tokens":{"limit":0,"used":123456,"remaining":0,"percent":0,"state":"unset","forecast":null},
 "partial_records":0}
```

### Configuration

- config: `config.toml`, searched in order `$XDG_CONFIG_HOME/gem-usage-lens` →
  `~/.config/gem-usage-lens` → `~/Library/Application Support/gem-usage-lens`
  (the first that exists is read; `doctor` prints which one).
  - `[sources] sessions_root` — override the inferred path (also `--sessions-root`)
  - `[pricing.models."<id>"]` — override / add rates (`input_per_mtok` /
    `output_per_mtok` / `cache_read_multiplier` / `grounding_per_req` /
    `non_global_multiplier`)
  - `[budget] monthly_usd / monthly_tokens / warn_percent / critical_percent`
- store: `~/Library/Application Support/gem-usage-lens/usage.db` (SQLite, WAL, 0700/0600)
- Strict decoding (an unknown key is an error); partial overrides inherit (`*float64`).

### External Dependencies

- External APIs / services: **none** (fully local, read-only)
- Libraries: `modernc.org/sqlite` (pure Go), `BurntSushi/toml`
- Credentials: none

## 3. Design Decisions

- **A new repository, forked — not a `--source gem` in claude-usage-lens.** The source
  format, the accounting rules (thoughts bill as output, cached is a share), the budget
  model (calendar month vs. weekly calibration) and the store schema all differ, and
  bolting them on would churn the existing GUI's JSON contract. The name also names the
  product. No shared library either: the two schemas differ and the shared part is thin.
- **Go / util-series / single binary.** `core/` is pure functions + DI; the GUI consumes
  it through `--json` (no Go import).
- **Store = SQLite (modernc.org/sqlite, pure Go)** for the same reasons as
  claude-usage-lens. **Rows outlive their source transcript** (insurance against a
  deletion accident on the gem-agent side).
- **Built-in rate table + config overrides + `reprice`.** ADR-0057's own lesson says
  "don't bake prices into a tool"; this tool does, knowingly, because running offline
  with no credentials matters more, and it uses claude-usage-lens's mitigation: a
  `VerifiedOn` date in the table that `models` prints. Syncing from the Cloud Billing
  Catalog API (`pricing sync`) is a future option, not in v0.1. Table comments carry
  **no future scheduled prices** — only the verification date and source.
- **Price model (Vertex AI, verified 2026-09-02 on the official pricing page)**:
  - `input_per_mtok` × (prompt − cached) + `input_per_mtok × cache_read_multiplier` ×
    cached + `output_per_mtok` × (output + thoughts)
  - `cache_read_multiplier` is a per-model field (default 0.1), never a constant
    (the claude-usage-lens Fable 5.1 lesson).
  - **Region**: when the header `location` is not `global`, the whole amount is scaled
    by `non_global_multiplier` (default 1.1 = the "non-global" column of the table).
  - **Grounding**: each `source=web_search` record adds `grounding_per_req` (default
    $0.035 = $35 per 1,000). URL context (`web_fetch`) has no extra charge (tokens only).
  - Flash models have no long-context premium (≤200k and >200k are the same price).
    Tiered models like 3.1 Pro are not handled in v0.1 (not gem-agent's main models;
    they surface as unpriced warnings).
  - Cache **storage** ($/M tok·hour) cannot be derived from logs (implicit caching has
    no storage charge) → out of scope, stated as such.
  - Batch / Flex / priority tiers are not used by gem-agent → out of scope.
- **Unpriced models are visible from day one**: stderr warnings from `ingest` /
  `reprice`, plus `unpriced_records` / `unpriced_models` in `report --summary` (derived
  from stored rows). The GUI shows a badge and a Reprice button.
- **Budget = calendar month, local TZ, reset on the 1st at 00:00.** Both bases (USD and
  tokens = `total`), two thresholds (warning / critical), a linear pace forecast from the
  elapsed fraction (`reliable=false` in the first 5% of the window). **No calibration**
  (Vertex has no window-shaped quota to calibrate against). **The budget arithmetic lives
  once, in the CLI's `core/budget`**; the GUI renders `budget --json` (nothing of
  claude-usage-lens-gui's `WeeklyLimit.swift` is ported). For real-bill monitoring,
  pair with GCP Cloud Billing budget alerts.
- **GUI = a fork of claude-usage-lens-gui (SwiftUI `MenuBarExtra`, LSUIElement, macOS
  14+).** The bundled CLI is the trust anchor. Single-instance guard, App Nap opt-out,
  on-screen version, unpriced badge, monthly budget (tinted bar in the popover +
  notifications), analysis window (daily / stacked by model / top projects). No
  calibration UI.
- **Explicitly out of scope**: reconciling against the real bill (Cloud Billing export),
  aggregating from Cloud Logging telemetry (`model.usage`), multi-machine roll-up (the
  `host` column exists), Gemini clients other than gem-agent, and the `image` records of
  local tools (image generation / transcription — not billable).

### Package layout

```
gem-usage-lens/
  main.go                 → cmd.Execute
  cmd/                    stdlib flag dispatch: ingest/reprice/report/budget/sessions/models/verify/doctor/watch/daemon
  core/model              UsageRecord{Key, Timestamp, Host, SessionID, Project, Model, Source, Location, Partial, Usage{Prompt,Output,Thoughts,Cached,Total}}, Cost
  core/collect            Discover / ReadHeader / ParseFrom (offset continuation, legacy fill-in, checksum)
  core/pricing            Rates{InputPerMTok, OutputPerMTok, CacheReadMultiplier, GroundingPerReq, NonGlobalMultiplier}, Default(), Lookup, VerifiedOn
  core/cost               Compute (pure)
  core/aggregate          group-by / dense / sort / summary (+unpriced)
  core/budget             MonthWindow / Consumption / Status / Forecast (pure)
  core/store              SQLite: usage_records(record_key PK) / ingest_state
  core/ingest             collect → price → store
  core/config             config.toml (strict, inheriting partial overrides, [budget])
  core/platform           sessions root / config search / data dir / launchd
  docs/{ja,en}            RFP (this document)
```

### Store schema

```
usage_records(record_key PK, ts, host, session_id, project, model, source, location,
              prompt_tokens, output_tokens, thoughts_tokens, cached_tokens, total_tokens,
              checksum_ok, partial, cost_usd, ingested_at)
ingest_state(path PK, size, mtime, last_offset, updated_at)
```

## 4. Development Plan

### Phase 1: CLI core (started with this RFP)
- All `core/` packages + `ingest` / `reprice` / `report` / `budget` / `sessions` /
  `models` / `verify` / `doctor` / `watch` / `daemon`
- Tests: synthetic transcripts (new and legacy forms, resume appends, checksum
  mismatches) pin parse / cost / aggregate / budget / store / config. Pricing tests pin
  separately: Flash cache 0.1×, non-global 1.1×, per-request grounding on web_search,
  thoughts at the output price.
- Real-data E2E: the author's 79 transcripts (66 legacy) through ingest → report → verify

### Phase 2: GUI (once the CLI's JSON contract is fixed; same release)
- Fork claude-usage-lens-gui; move `Row` / `Summary` / `BudgetPayload` to the new contract
- Monthly-budget UI (settings → CLI `budget` flags); remove the calibration UI
- Off-screen rendering (NSHostingView) of every branch of popover / settings / analysis

### Phase 3: Release
- Independent verification passes (sub-agent) at design freeze and before release
- CLI: `make package` (darwin/arm64 signed + notarized) → GitHub release → tap formula
- GUI: bundle the CLI built with an explicit `VERSION=vX.Y.Z` → `make package`
  (notarize + staple) → release → tap cask
- util-series submodules, org profile / web catalogue / GitHub About / tap README,
  check-org.sh

## 5. Required API Scopes / Permissions

**None.** Local file reads only, under the user's own `~/.local/state/gem-agent/`. No
special macOS permission.

## 6. Series Placement

Series: **util-series** (both CLI and GUI). A pipe-friendly data-processing CLI that
reads local JSONL and emits tables / `--json`, on the same shelf as claude-usage-lens /
claude-usage-lens-gui.

## 7. External Platform Constraints

- **Schema drift (gem-agent side)**: the transcript format is defined by gem-agent's
  ADR-0005 / 0057 and may change. The parser skips unknown fields tolerantly and counts
  missing required fields for `verify` / `doctor`. Extra fields on `usage` are fine. A
  gem-agent ADR that changes these fields obliges this tool to follow.
- **Price drift (Vertex)**: gemini-3.7-flash / 3.6-flash are at an introductory price
  through 2026-12-31 and the official table already lists the price from 2027-01-01.
  The table records **only the verification date and source**; revisions land at the
  next sync (`models` prints the date). Overridable via config.
- **Regional pricing**: the official table has two columns, global and non-global
  (1.1×). Every local session is `global`. The multiplier is overridable.
- **Grounding granularity**: billing is per "grounding prompt"; one gem-agent
  `web_search` call is taken as one prompt. The Gemini Developer API's "5,000 free per
  month" does not exist on Vertex (Vertex is $35 per 1,000).
- **Legacy incompleteness**: transcripts before 2026-08-30 lack risk / compaction spend
  and under-count. The `partial` column marks them and the summary reports the count.

---

## Discussion Log

- **Trigger (2026-09-02)**: the operator: "a gem-agent version of claude-usage-lens would
  be good now that production use has picked up; the budget can be monthly rather than
  Claude-style weekly".
- **Premises checked**: gem-agent ADR-0057 writes a `usage` record for every model call
  and leaves the price table and cost report to another tool. 66 of the 79 local
  transcripts are legacy. Records carry no message id → file + offset dedup key.
- **Prices measured**: on the official Vertex AI pricing page (2026-09-02):
  gemini-3.7-flash (global $0.75 / $3.75 / cached $0.075; non-global 1.1×; doubles
  from 2027-01-01), gemini-3.5-flash-lite ($0.30 / $2.50 / $0.03), gemini-3.5-flash
  ($1.50 / $9.00 / $0.15), Grounding with Google Search $35 per 1,000. "Answer and
  reasoning" share one output price.
- **Decisions**: names `gem-usage-lens` / `gem-usage-lens-gui`, util-series, forks of the
  claude-usage-lens pair, calendar-month budget, no calibration, budget arithmetic in
  the CLI, CLI and GUI shipped together (operator's instruction).
