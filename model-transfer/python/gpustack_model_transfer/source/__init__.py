"""The source runtime: workers that serve one verified generation each.

The package speaks the broker control wire that pkg/modeltransfer/broker serves,
and nothing else. The native path lives beside the reference path:
`worker.py` carries the LocalFileReader reference worker (never a
production fallback), `native_worker.py` the production native holder
that fills real pinned host memory and registers it in this process,
`supervisor.py` runs many generations in one process, and
`__main__.py` is the production entry with real vendor factories only.
"""
