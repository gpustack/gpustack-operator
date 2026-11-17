# `priors-sync` Instructions

You are maintaining the offline capacity prior catalog (`pkg/worker/scaling/priors/priors.yaml`)
and its provenance ledger (`pkg/worker/scaling/priors/records.md`) inside an offline sandbox
(`networkMode: none`).

## Staged Preflight Artifacts (`$AUTOSYNC_STAGE_DIR`)

Host preflight has already fetched upstream InferenceX artifacts into `$AUTOSYNC_STAGE_DIR`:

- `$AUTOSYNC_STAGE_DIR/snapshots/availability.json` — raw JSON from
  `https://inferencex.semianalysis.com/api/v1/availability`.
- `$AUTOSYNC_STAGE_DIR/snapshots/<QueryID>.json` — raw JSON from
  `https://inferencex.semianalysis.com/api/v1/benchmarks?model=<QueryID>` for each active query ID.
- `$AUTOSYNC_STAGE_DIR/priors-preflight-report.json` — normalized summary containing `capture_date`,
  `inferencex_repo.commit`, `availability` (`url`, `sha256`, `changed`), `queries[]` (`query_id`,
  `url`, `rows`, `sha256`, `changed`), `removed_query_ids[]`, and `catalog_merge` counts.
- `$AUTOSYNC_STAGE_DIR/baseline/pkg/worker/scaling/priors/{priors.yaml,records.md}` — pre-run
  baselines.

When qualifying sweeps changed, preflight already executed `go run ./gen/priors --merge ...` and
deterministic `go run ./gen/priors` onto `source_revision`, so `pkg/worker/scaling/priors/priors.yaml`
in the candidate tree already holds the merged catalog.

## Required Reconciliation Steps

1. Inspect `$AUTOSYNC_STAGE_DIR/priors-preflight-report.json` and compare `snapshots/` against the
   baseline catalog and `pkg/worker/scaling/priors/records.md`.
2. If upstream removed, deprecated, or superseded a sweep or model shape that `priors.yaml` still
   carries (check `removed_query_ids` or missing configurations in refreshed snapshots), retire the
   affected profiles explicitly with `go run ./gen/priors --retire ...`, re-run
   `go run ./gen/priors`, and log every retirement in `pkg/worker/scaling/priors/records.md` under
   `## Retirement log`. Never silently drop profiles without a `#retirement-log` entry.
3. Update `pkg/worker/scaling/priors/records.md` under `## Active Sources`:
   - Record the exact `capture_date` from `priors-preflight-report.json` and supersede prior active
     tables into `## Historical sources`.
   - Record the availability URL (`https://inferencex.semianalysis.com/api/v1/availability`) and its
     exact `SHA-256` checksum from `priors-preflight-report.json`.
   - Update every row in the `| Query ID | Rows | Aggregated | Disaggregated | SHA-256 |` table
     with exact row counts and `SHA-256` checksums from `priors-preflight-report.json`, and ensure
     per-row `Aggregated` and `Disaggregated` counts (and summary prose) match the actual profiles
     in `pkg/worker/scaling/priors/priors.yaml`.
4. Never guess unverified Hugging Face `<org>/<repo>` IDs; refuse unmapped sources and document
   refusals in `pkg/worker/scaling/priors/records.md`.
5. Never write outside `pkg/worker/scaling/priors/priors.yaml`,
   `pkg/worker/scaling/priors/records.md`, and `.auto-sync/proposals/priors.json`.
6. Update `.auto-sync/proposals/priors.json` (`schema_version: 1`, `summary`, `versions`,
   `subscriptions`) and settle as `completed`, or settle as `no_change` when preflight and catalog
   inspection show zero qualifying changes.
