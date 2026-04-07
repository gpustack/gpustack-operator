SHELL := /bin/bash

# Borrowed from https://stackoverflow.com/questions/18136918/how-to-get-current-relative-directory-of-your-makefile
curr_dir := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))

# Borrowed from https://stackoverflow.com/questions/2214575/passing-arguments-to-make-run
rest_args := $(wordlist 2, $(words $(MAKECMDGOALS)), $(MAKECMDGOALS))
$(eval $(rest_args):;@:)

targets := $(shell ls $(curr_dir)/hack | grep '.sh' | sed 's/\.sh//g')
$(targets):
	@$(curr_dir)/hack/$@.sh $(rest_args)

help:
	#
	# Usage:
	#
	#   * [dev] `make deps`, get dependencies.
	#           - `make deps update`, update dependencies.
	#
	#   * [dev] `make generate`, generate something.
	#
	#   * [dev] `make lint`, check style.
	#           - `make lint dirty`, verify whether the code tree is dirty.
	#
	#   * [dev] `make package`, build container images, not supported on Windows.
	#
	#   * [dev] `make build`, execute cross building.
	#           - `VERSION=vX.y.z+l.m make build` build all targets with vX.y.z+l.m version.
	#
	@echo

.DEFAULT_GOAL := build
.PHONY: $(targets) api binding gen hack pack pkg staging
