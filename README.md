# gem-usage-lens

Token usage and cost analyzer for [gem-agent](https://github.com/nlink-jp/gem-agent)
session transcripts (Vertex AI Gemini).

gem-agent writes one accounting record per model call into its session
transcripts — prompt / output / thinking / cached token counts, the model, and
which part of the agent spent them. Vertex AI never reports money, so
`gem-usage-lens` reads those records, applies the Vertex AI list price, keeps
the result in a local SQLite store, and reports it by day, session, project,
model and call source. It also tracks a **calendar-month budget** with a pace
forecast.

The counterpart of [claude-usage-lens](https://github.com/nlink-jp/claude-usage-lens).
A menu-bar app, [gem-usage-lens-gui](https://github.com/nlink-jp/gem-usage-lens-gui),
renders the same data.

> Costs are a Vertex AI **list-price equivalent** (notional), computed from the
> transcript's own token counts. They are not your bill: committed-use
> discounts, credits and rounding are not modelled. For the real invoice, use
> Cloud Billing.

> macOS releases are **Developer ID signed and Apple-notarized**. Linux and
> Windows binaries are unsigned and experimental.

## Install

Homebrew (macOS, Apple silicon):

```bash
brew install nlink-jp/tap/gem-usage-lens
```

Or download a release archive from the
[Releases](https://github.com/nlink-jp/gem-usage-lens/releases) page and put
the `gem-usage-lens` binary on your PATH.

Requirements: gem-agent's transcripts under `~/.local/state/gem-agent/sessions`
(or wherever `GEMAGENT_STATE_DIR` points). No credentials, no network.

## Quick start

```bash
gem-usage-lens doctor            # where transcripts, config and the store resolve
gem-usage-lens ingest            # load new/appended records (incremental, idempotent)
gem-usage-lens report --since 7d # last 7 days by day
gem-usage-lens budget --limit-usd 100
```

## Commands

| Command | What it does |
|---------|--------------|
| `ingest` | Read only the bytes appended since last time and upsert them into the store. Safe to run any time. |
| `report` | Aggregate the store. `--since` / `--until` / `--group-by` / `--source` / `--model` / `--project` / `--sort` / `--top` / `--dense` / `--summary` / `--compare` / `--tz` / `--json`. |
| `budget` | Calendar-month budget state: used, remaining, warning state, pace forecast. `--limit-usd` / `--limit-tokens` / `--warn` / `--critical` / `--tz` / `--json`. |
| `sessions` | One row per session (`--sort cost --top 10`). |
| `models` | The rate table with its verification date, and which entries come from your config. |
| `reprice` | Recompute stored costs after a rate change (config or a new build). `--dry-run` previews. |
| `verify` | Check every transcript's accounting checksum (`prompt + output + thoughts == total`) and list files written before gem-agent ADR-0057. |
| `doctor` | Show the resolved sessions root, config path (or every path searched) and store path. |
| `watch` | Poll and ingest continuously, printing live cost deltas (`--interval 5s`). |
| `daemon` | `install` / `uninstall` / `status` a launchd job that runs `ingest` periodically (macOS). |

`--since` accepts a date (`2026-09-01`), a datetime (`2026-09-01T09:00`),
RFC 3339, a relative range (`7d`), `today`, or `month` (the 1st of the current
month). Day and month boundaries follow `--tz` (local by default).

### Report

```
$ gem-usage-lens report --since 30d --group-by source
KEY                   RECORDS  PROMPT    CACHED    OUTPUT  THOUGHTS  TOTAL     COST(USD)
main*                 924      93013003  75522739  331618  116003    93460624  $20.4605
risk                  102      117221    0         3151    15199     135571    $0.1567
web_search            5        257       0         2968    1947      5172      $0.1936
...
```

- **CACHED** is the share of PROMPT served from cache — not an addition.
- **THOUGHTS** are thinking tokens; they bill at the output price.
- **TOTAL** is prompt + output + thoughts, the billed count.
- A `*` marks buckets containing records from transcripts written before
  gem-agent ADR-0057 (2026-08-30). Those files never recorded risk and
  compaction spend, so their totals are a lower bound.

`--summary` prints active days, daily average, peak day, a 30-day projection,
and — importantly — `unpriced_records`: calls stored at $0 because their model
was not in the rate table when ingested. See [Pricing](#pricing).

### Budget

```
$ gem-usage-lens budget --limit-usd 100
month:   2026-09-01 → 2026-10-01 (Local)   elapsed 7%
cost:    $1.60 used / $100.00 limit   2% used · $98.40 left (98%)   [normal]
         pace: on pace for $23.81 (24%) by the reset
tokens:  6.2M used (no limit set — pass --limit-usd/--limit-tokens or set [budget] in config.toml)
```

The window is the calendar month in `--tz`, resetting on the 1st at 00:00.
Both bases are reported: USD and billed tokens. The pace line extrapolates the
month linearly from what has elapsed; in the first 5% of the month it says
"too early" instead of projecting from a single session. Limits default from
`[budget]` in config.toml; flags override.

## Pricing

Prices are USD per 1M tokens on the Vertex AI **global** endpoint, taken from
the official pricing page and stamped with the date they were verified
(`gem-usage-lens models` prints it). Per call:

```
(prompt − cached) × input  +  cached × input × 0.1  +  (output + thoughts) × output
+ $0.035 when the call is a web_search (Grounding with Google Search, $35 / 1,000)
× 1.1 when the session's location is not "global"
```

Cache storage charges (explicit caching) and batch / priority tiers are not
modelled — gem-agent uses neither.

A model missing from the table costs **$0**, and every surface says so:
`ingest` and `reprice` warn on stderr, `report --summary` counts
`unpriced_records` per model, and the GUI shows a badge. To fix without waiting
for a release, price the model in `config.toml` and run `reprice`:

```toml
[pricing.models."gemini-4-flash"]
input_per_mtok  = 1.0
output_per_mtok = 5.0
```

Only the fields you set are overridden; the rest inherit (cache multiplier 0.1,
grounding $0.035, non-global 1.1). See
[config.example.toml](config.example.toml) for every field.

## Configuration

`config.toml` is looked for at, in order:

1. `$XDG_CONFIG_HOME/gem-usage-lens/config.toml` (when the variable is set)
2. `~/.config/gem-usage-lens/config.toml`
3. `~/Library/Application Support/gem-usage-lens/config.toml` (macOS)

Every setting is optional. Unknown keys are an error. Sections:
`[sources] sessions_root`, `[pricing.models."<id>"]`, `[budget]`.
`doctor` prints which file loaded.

The store lives at `~/Library/Application Support/gem-usage-lens/usage.db` on
macOS (`$XDG_DATA_HOME/gem-usage-lens` elsewhere), owner-only. Rows are never
deleted by `ingest`, so history outlives the transcripts.

## Data source

| Item | Value |
|------|-------|
| Sessions root | `$GEMAGENT_STATE_DIR/sessions`, else `~/.local/state/gem-agent/sessions` |
| Files | `**/*.jsonl` (one per session; a resume appends to the same file) |
| Records | `{"kind":"usage","data":{"source","model","prompt","output","thoughts","cached","total"}}` and the session header's `model` / `project` / `location` |

Records are keyed by file and byte offset, so re-running `ingest` never double
counts. A line still being written is left for the next run.

Transcripts from gem-agent before v0.57 (ADR-0057) carry only main-loop rounds
without `source` / `model`; those are filled from the header and flagged
`partial`. `verify` lists them.

## JSON

`report --json`, `report --summary --json`, `budget --json`, `sessions --json`,
`models --json` and `verify --json` are stable machine-readable outputs; the
GUI consumes the first three. Timestamps are RFC 3339 with whole seconds.

## Build

```bash
make build      # → dist/gem-usage-lens
make test
make vet        # host + linux + windows
```

## Documents

- [RFP (design)](docs/en/gem-usage-lens-rfp.md) · [日本語](docs/ja/gem-usage-lens-rfp.ja.md)
- [gem-agent ADR-0057 — accounting records](https://github.com/nlink-jp/gem-agent/blob/main/docs/en/adr/0057-usage-accounting-records.md)

## License

MIT
