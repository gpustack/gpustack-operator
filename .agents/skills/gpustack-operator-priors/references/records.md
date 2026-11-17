# Capacity prior provenance records

The canonical capacity prior provenance ledger lives at
[pkg/worker/scaling/priors/records.md](../../../../pkg/worker/scaling/priors/records.md).

It was relocated out of `.agents/` so automated `priors-sync` runs can update `priors.yaml`
and its SHA-256 provenance ledger atomically without modifying protected skill paths.
