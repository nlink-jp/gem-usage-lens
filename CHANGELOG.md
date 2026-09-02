# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-09-03

Initial release — the gem-agent counterpart of claude-usage-lens.

### Added

- `ingest`: incremental, idempotent load of gem-agent transcripts
  (`~/.local/state/gem-agent/sessions`, or `$GEMAGENT_STATE_DIR/sessions`)
  into a SQLite store. Records are keyed by file + byte offset; only complete
  lines advance the offset; the session header is re-read every pass.
- Accounting per gem-agent ADR-0057: thinking tokens bill at the output price,
  cached tokens are a discounted share of the prompt, and
  `prompt + output + thoughts == total` is checked on every record.
- Legacy transcripts (before ADR-0057) are ingested with source/model filled
  from the header and flagged `partial`; every report, `budget` and the JSON
  contract carry the count.
- `report`: group by hour / day / week / month / session / project / model /
  source, with `--since` (`month` included), `--dense`, `--sort`, `--top`,
  `--summary` (with `unpriced_records` / `unpriced_models` /
  `partial_records` / `checksum_mismatches`), `--compare`, `--tz`, `--json`.
- `budget`: calendar-month budget on both bases (USD and billed tokens) with
  warning / critical thresholds and a linear pace forecast; `--json` is the
  GUI contract.
- Built-in Vertex AI rate table (global endpoint, verified 2026-09-02): Gemini
  3.7 / 3.6 / 3.5 Flash, 3.5 / 3.1 Flash-Lite, 3 Flash preview. Per-model
  cache-read multiplier, per-request grounding charge on `web_search` calls,
  and a non-global region multiplier from the session header's `location`.
- `reprice`, `models` (with the table's verification date), `verify`
  (checksum + partial-file report; exits 1 on any mismatch), `doctor`, `watch`,
  `daemon` (launchd), `sessions`.
- `config.toml` searched at XDG → `~/.config` → Application Support:
  `[sources]`, `[pricing.models]` (partial overrides inherit), `[budget]`.
