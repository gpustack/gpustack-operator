"""Fail-closed acceptance runner for the model-transfer capability.

Invoked as `python3 -m gpustack_model_transfer.acceptance --config <path> --backend
<b> [--engine <e>] --suite <s>`. The configuration is validated strictly
before anything executes; an incomplete, stale, or unknown configuration
refuses the run with an explicit reason and a non-zero exit. The runner
performs no cloud or cluster allocation of any kind.

Exit codes: 0 every capability PASS; 1 the suite ran and reported NO-GO;
2 refused (unknown or invalid input); 3 backend/suite known but not
implemented yet.
"""
