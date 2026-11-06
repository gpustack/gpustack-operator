.SILENT:
.DEFAULT_GOAL := ci
.PHONY: deps lint test clean build ci

SHELL := /bin/bash

SRCDIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))

deps:
	@echo "+++ $@ +++"
	uv sync --all-packages
	uv lock
	@echo "--- $@ ---"

clean:
	@echo "+++ $@ +++"
	uv run pyclean -v $(SRCDIR)
	rm -rf dist
	@echo "--- $@ ---"

lint:
	@echo "+++ $@ +++"
	uv run ruff check $(SRCDIR)
	uv run ruff format --check $(SRCDIR)
	@echo "--- $@ ---"

test:
	@echo "+++ $@ +++"
	uv run pytest
	@echo "--- $@ ---"

build:
	@echo "+++ $@ +++"
	rm -rf dist
	uv build
	@echo "--- $@ ---"

ci: deps lint test clean build
