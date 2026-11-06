"""Transport backends.

A backend adapts one vendor transport to the reader contract. Backends are
optional: importing this package imports no vendor runtime, and each
backend's vendor imports happen lazily inside its runtime module, so the
wheel installs and runs on a CPU-only host with zero hard dependencies.
"""
