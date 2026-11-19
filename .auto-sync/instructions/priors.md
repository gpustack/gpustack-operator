# `priors-sync` Instructions

You are maintaining the offline capacity prior catalog (`pkg/worker/scaling/priors/priors.yaml`)
and its provenance ledger (`pkg/worker/scaling/priors/records.md`) inside an offline sandbox
(`networkMode: none`).

## Staged Preflight Artifacts (`.auto-sync/.stage/priors/`)

Inside the offline `linux-bwrap` sandbox, host toolchains (`go`, `hugo`, etc.) and external host
staging directories are NOT mounted. Always inspect `.auto-sync/.stage/priors/` FIRST (falling back
to `$AUTOSYNC_STAGE_DIR` only if `.auto-sync/.stage/priors/` is absent, e.g. manual local runs):

- `.auto-sync/.stage/priors/priors-preflight-report.json` — normalized summary containing
  `capture_date`, `inferencex_repo.commit`, `availability` (`url`, `sha256`, `changed`),
  `queries[]` (`query_id`, `model`, `url`, `rows`, `aggregated`, `disaggregated`, `sha256`,
  `changed`), `removed_query_ids[]`, `catalog_models_by_mode`, and `catalog_merge` counts.
- `.auto-sync/.stage/priors/upstream-records.json` — normalized `TaskpackUpstreamRecord` entries.

**Read-only stage rule:** `.auto-sync/.stage/**` is read-only host-staged context outside
`scope.writable`. **NEVER** edit, move, or delete any file under `.auto-sync/.stage/**` (doing so
fails `scope-guard`). Loopwright automatically strips `.auto-sync/.stage/**` when running the
host verification gate (`validate_priors.sh`, which runs `go run ./gen/priors`, `go test`, and
`make lint docs`) and when publishing the Pull Request.

When qualifying sweeps changed, host preflight already executed `go run ./gen/priors --merge ...`
and deterministic `go run ./gen/priors` onto `source_revision`, so
`pkg/worker/scaling/priors/priors.yaml` in your workspace already holds the merged and
deterministically formatted catalog.

## Required Reconciliation Steps

1. Read `.auto-sync/.stage/priors/priors-preflight-report.json` (and `upstream-records.json`)
   before editing any file.
   - Leave `pkg/worker/scaling/priors/priors.yaml` untouched unless retiring obsolete shapes.
   - If `digest_refresh_only` is `true`, upstream snapshot SHA-256 digests changed without altering
     `priors.yaml` profiles; you MUST still refresh the SHA-256 digests and capture date in
     `pkg/worker/scaling/priors/records.md` (and `.auto-sync/proposals/priors.json`) and settle as
     `completed` — never `no_change`.
2. If upstream removed, deprecated, or superseded a sweep or model shape that `priors.yaml` still
   carries (check `removed_query_ids` or missing configurations), retire the affected profiles
   explicitly and log every retirement in `pkg/worker/scaling/priors/records.md` under
   `## Retirement log`. Never silently drop profiles without a `#retirement-log` entry.
3. Update `pkg/worker/scaling/priors/records.md` under `## Active Sources`:
   - Record the exact `capture_date` from `priors-preflight-report.json` and archive the
     superseded active table under `## Historical sources` with plain clear sentences (no
     broken anchor links or unverified claims).
   - Record the availability URL (`https://inferencex.semianalysis.com/api/v1/availability`) and its
     exact `SHA-256` checksum from `priors-preflight-report.json`.
   - Update every row in the `| Query ID | Rows | Aggregated | Disaggregated | SHA-256 |` table
     with the exact `rows`, `aggregated`, `disaggregated`, and `sha256` values from `queries[]` in
     `priors-preflight-report.json`, and ensure summary counts match `catalog_merge`
     (`aggregated_profiles` / `disaggregated_profiles` / `total_profiles`).
4. Never guess unverified Hugging Face `<org>/<repo>` IDs; refuse unmapped sources and document
   refusals in `pkg/worker/scaling/priors/records.md`.
5. Never write outside `pkg/worker/scaling/priors/priors.yaml`,
   `pkg/worker/scaling/priors/records.md`, and `.auto-sync/proposals/priors.json`.
6. Update `.auto-sync/proposals/priors.json` (`schema_version: 1`, `summary`, `versions`,
   `subscriptions`) and settle as `completed`, or settle as `no_change` when preflight and catalog
   inspection show zero qualifying changes.
