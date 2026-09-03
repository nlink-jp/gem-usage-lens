# CLAUDE.md — gem-usage-lens

**Organization rules (mandatory): https://github.com/nlink-jp/.github/blob/main/CONVENTIONS.md**

## Project overview

util-series CLI that parses gem-agent session transcripts
(`~/.local/state/gem-agent/sessions/**/*.jsonl`, one `usage` record per model
call per gem-agent ADR-0057) to collect token usage and compute a Vertex AI
**list-price-equivalent** cost. Usage is accumulated in a durable SQLite store
and reported by day / session / project / model / call source, with a
calendar-month budget and pace forecast. The GUI (`gem-usage-lens-gui`) is a
thin front-end over this CLI's `--json`.

## Non-negotiable rules

- **Tests are mandatory** — write them with the implementation
- **Never `go build` directly** — always `make build` (outputs to `dist/`)
- **Docs in sync** — update `README.md` and `README.ja.md` together
- **Small, typed commits** — `feat:`, `fix:`, `test:`, `chore:`, `docs:`, `refactor:`
- **No PII / secrets committed** — never commit real transcripts or `usage.db`;
  test fixtures are synthetic

## Build & test

```sh
make build      # → dist/gem-usage-lens
make test       # or: go test ./...
make vet        # host + GOOS=windows + GOOS=linux
```

## Key decisions

- **Accounting = gem-agent ADR-0057**: thoughts bill as output, cached is a
  share of prompt, `prompt + output + thoughts + tool_prompt == total` is the checksum (tool_prompt is derived when gem-agent omits it).
- **Dedup by file + byte offset** (no message id exists); torn lines wait.
- **Store**: SQLite via `modernc.org/sqlite` (pure Go); rows outlive sources.
- **Prices per model, per field** (`core/pricing`), verified on a stated date,
  overridable in config, applied to history with `reprice`; unpriced models
  are surfaced everywhere.
- **Budget = calendar month**, computed only in `core/budget`; the GUI renders
  `budget --json`.

## Architecture

- `cmd/` — stdlib-flag dispatch (all commands implemented, tested)
- `core/model` — shared types · `core/collect` — discover / parse
- `core/pricing` + `core/cost` — rate table + pure cost engine
- `core/aggregate` — group-by / summary · `core/budget` — month window / forecast
- `core/store` — SQLite · `core/ingest` — orchestration
- `core/config` — strict TOML · `core/platform` — paths / launchd

## Design references

- [`docs/ja/gem-usage-lens-rfp.ja.md`](docs/ja/gem-usage-lens-rfp.ja.md) (primary)
- [`docs/en/gem-usage-lens-rfp.md`](docs/en/gem-usage-lens-rfp.md)
