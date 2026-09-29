# ADR-0001: Effective-dated prices — each record is priced at the rate in force when it was made

| Field | Value |
|-------|-------|
| Status | Accepted |
| Date | 2026-09-29 |
| Binds | gem-usage-lens |
| Decision makers | nlink-jp maintainers |
| Triggered by | The Vertex AI pricing page lists a second price for Gemini 3.8 / 3.7 / 3.6 Flash starting 2027-01-01; one price per model cannot represent both, and adding the new price later would let `reprice` rewrite 2026 |

## Context

The rate table (`core/pricing`) holds **one** set of prices per model, and
every pricing path applies it to every record regardless of when the call was
made: `ingest` prices new records with it, and `reprice` recomputes **every
stored record** from its token columns with it.

The Vertex AI pricing page (Gemini 3 section, checked 2026-09-29) splits three
models in use into two rows:

| Model | through 2026-12-31 | starting 2027-01-01 |
|---|---|---|
| gemini-3.8-flash / 3.7-flash / 3.6-flash | $0.75 in / $3.75 out / cached $0.075 | $1.50 in / $7.50 out / cached $0.15 |

Non-global rows are 1.1× of each; the cache-read ratio stays 0.1×. The page
calls the current price "introductory pricing … through December 31, 2026",
after which "standard pricing … will apply".

With one price per model, the table can be right for 2026 or for 2027, never
both:

- **Leave the table alone** → every call from 2027-01-01 is recorded at half
  its list price, and the monthly budget undercounts from January.
- **Update the table in January** → new calls are right, but the next
  `reprice` recomputes the whole of 2026 at the new price and doubles it.
  `reprice` is the documented fix for every other pricing error (it is how the
  grounding charge was corrected from $35 to $14 per 1,000 queries), so this
  is the normal path, not an edge case.

**A recorded lesson says not to write scheduled prices, and it stands.** knowledge
`llm-integration.md` ("When syncing a price table…") says: do not write future
schedules into the table; record the verification date and confirm at the
next sync that the schedule happened. It came from a dated introductory price
whose scheduled increase was then cancelled (Claude Sonnet 5: the $3 / $15
scheduled for 2026-09-01 never took effect). The 2027 rows here are the same
class — a dated end to an introductory price. The lesson is right about what
to write, but it assumed a flat table: when the scheduled change does happen,
the only way to record it today is to overwrite the old price, and the next
`reprice` then rewrites history. This ADR gives the lesson a table that can
follow it.

The start date has no time zone on the pricing page. Cloud Billing's Reports
documentation says: "A 24-hour time period in the Cloud Billing report starts
at midnight US and Canadian Pacific Time (UTC-8), and observes daylight saving
time shifts in the United States." That describes how reports group days, not
when a price change takes effect; it is the nearest day boundary Google
documents for billing.

## Decision

1. **A model's rates are a list of periods, each with an effective instant.**
   Each model maps to periods ordered by `From`; the first period is open from
   the beginning (a table test enforces this for every model). `Lookup(model,
   at)` returns the last period whose `From` is at or before `at` — a record
   exactly at `From` takes the new price. `Known(model)` answers "is this
   model priced at all" without a time, for the unknown-model warnings and
   `unpriced_*`. Every `From` must be a whole second, because the store keeps
   `ts` in whole seconds: ingest (full-precision timestamp) and `reprice`
   (stored seconds) then always choose the same period.

2. **Every pricing path uses the record's own timestamp.** `ingest` and
   `reprice` price each record at its time; `repriceSelect` reads
   `COALESCE(ts, 0)`. A record with no timestamp (none in the author's store:
   2,830 rows, 0 missing) falls in the first period, so the result is
   deterministic and never depends on when `reprice` runs.

3. **The built-in table keeps writing only prices in force; a change is
   added as a new period once it has happened.** The recorded lesson stands:
   a scheduled price is not pre-written, because it can be cancelled or made
   permanent. At the first sync after the start date that confirms the change,
   the new price is **appended as a period starting at its effective instant**
   — never written over the old one — and `reprice` then corrects the records
   ingested since the boundary while leaving earlier ones as they were. A
   table test pins the rule: no built-in period starts after `VerifiedOn`.
   The lesson in knowledge is extended in the same change with this second
   half — how to record the change once it happens (Decision 8).

4. **A period starts at midnight US Pacific Time** on its effective date,
   written as a literal RFC 3339 instant with its offset — for the 2027 change,
   `2027-01-01T00:00:00-08:00` (`08:00Z`, 17:00 JST) — never computed with
   `time.LoadLocation`, which the Windows and Linux builds cannot rely on
   without embedded zoneinfo. January is PST (−08:00); a
   summer boundary would be −07:00, so each instant is written out, not
   derived.

5. **Config overrides apply to every period of their model, field by field.**
   `[pricing.models."<id>"]` stays flat: an overridden field replaces that
   field in each period (a negotiated rate replaces the list price at all
   dates); an omitted field keeps each period's own value. Config cannot
   express a date. Because an override of `input_per_mtok` /
   `output_per_mtok` on a model with a schedule flattens the scheduled change,
   `models` and `doctor` say so by name. `config.example.toml`'s "after a
   price revision" example (which would stamp the 2027 price on 2026) is
   rewritten. A config key that is a snapshot alias (`gemini-3.7-flash-001`)
   is its own single-period entry; it does not inherit the base model's
   schedule — documented.

6. **Surfaces.**
   - `models` shows one row per period with a `FROM` column (`—` for the open
     first period) and marks the period in force now.
   - `models --json` keeps `models` as today — model → the rates **in force
     now**, same fields — and adds `schedule`: model → every period as
     `{"from": "<RFC 3339>" | "", …rates}`, always written (no `omitempty`;
     `""` for the open first period, the repository's convention for an
     absent instant). A test pins both keys. "Now" is injected for tests.
   - `ingest` / `reprice` unknown-model warnings and `unpriced_*` use
     `Known` and do not depend on time.
   - `budget` and `report` sum stored `cost_usd`; no change.
   - `verify` does no pricing; no change.

7. **Nothing is repriced now, and the mechanism ships before the change.**
   No period is added now, so no stored cost moves. This release (CLI, and a
   GUI bundling it) exists so that the January sync is a one-period table
   change followed by `reprice`. From 2027-01-01 until that sync ships,
   records on the three models are priced at the 2026 rate — an accepted
   under-count (see Consequences) that the post-sync `reprice` corrects
   exactly. The January release must include a GUI release too: the GUI
   ingests every minute with its **bundled** CLI and its Reprice button runs
   that CLI, and the `daemon` launchd job must point at the new binary.

8. **Say how a price change is recorded, everywhere the rule is read**, in
   the same change: the `VerifiedOn` comment in `core/pricing/table.go`,
   AGENTS.md's "no scheduled future prices" gotcha (kept, with the append-a-
   period step added) and its Structure line, README / README.ja ("Pricing":
   the per-date rule and the `FROM` column), `config.example.toml`, knowledge
   `llm-integration.md` (ja + en), and the project memory, which also records
   the pending 2027 sync.

## Consequences

- One period table replaces one flat entry per model; single-price models
  read as before. `Table`'s shape changes, but it is internal to this module;
  the GUI reads only `report` / `budget` JSON (confirmed by grep of its
  sources), and the `models` JSON key keeps its shape.
- Tests that must exist (multi-period behaviour on a synthetic table, since
  the built-in one has none yet): `Lookup` at `07:59:59Z`, `08:00:00Z` and
  `08:00:01Z` on a `-08:00` boundary, and the same on a `-07:00` one; no
  built-in period starts after `VerifiedOn`; the first period is
  open for every model; every `From` is a whole second; a config override
  lands in every period and an omitted field keeps each period's value;
  `reprice` of a mixed-date store leaves 2026 rows unchanged and prices 2027
  rows at the new rate; ingest followed by `reprice` changes 0 records on
  either side of the boundary; the `models --json` shape, both keys.
- **Accepted: January is under-counted until the sync ships** (Decision 3).
  From 2027-01-01 the three models are recorded at half their list price, and
  the monthly budget reads low, until the new period is released in both the
  CLI and the GUI; `reprice` then corrects every affected record. The pending
  sync is recorded in the project memory so it is not left to chance.
- **Accepted: the boundary instant is inferred.** If Vertex applies the new
  price at another instant on 2027-01-01, calls in that window (at most 8
  hours from UTC midnight, 17 hours from JST midnight) are priced on the
  wrong side. At the author's current volume that is under a dollar.
- **Accepted: an old binary can undo the correction.** A GUI or CLI still on
  a build without the 2027 period that ingests or runs `reprice` after the
  boundary writes the 2026 price again. A later `reprice` with the new build
  restores it.
- `claude-usage-lens` has the same one-price structure but no scheduled
  change today; this ADR binds only gem-usage-lens.

## Alternatives considered

- **A. Keep one price; update the table on the day and never `reprice`.**
  Correct only as long as nobody runs the command the tool tells users to run
  after any pricing fix. Installs that are not updated also undercount.
- **B. Freeze stored costs older than some date.** Removes `reprice` as the
  fix path for pricing errors, which it has already been needed for.
- **C. Store a rate snapshot on every record.** A schema change that still
  needs dated rates to price correctly, and blocks applying a corrected rate
  to history.
- **D. Dated periods in config only (built-in stays flat).** Puts a published,
  dated fact on every user to transcribe. Config periods can be added later
  if a need appears; nothing here prevents it.
- **E. UTC or local-time boundary.** Neither is documented by Google for
  billing; the Pacific boundary is the nearest documented one.
- **G. Write the published 2027 price into the built-in table now.** January
  would switch over by itself, and for a budget monitor over-counting (if the
  change were cancelled) is the safer direction than under-counting. Rejected
  by the maintainer's decision to keep the recorded lesson: a dated
  introductory price has been cancelled before, and a table that states only
  prices in force is the one a reader can trust without re-checking the page.
  The under-count this leaves in January is bounded by the sync and undone by
  `reprice`.

## References

- Vertex AI pricing, Gemini 3 section:
  <https://cloud.google.com/vertex-ai/generative-ai/pricing?hl=en> (checked
  2026-09-29; redirects to the Agent Platform pricing page).
- Cloud Billing › Analyze billing data and cost trends with Reports:
  <https://docs.cloud.google.com/billing/docs/how-to/reports> (checked
  2026-09-29).
- knowledge `docs/en/llm-integration.md` › "When syncing a price table, check
  every multiplier column and the footnotes" — the lesson this ADR keeps and
  extends.
