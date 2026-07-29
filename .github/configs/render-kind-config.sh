#!/usr/bin/env bash

# Renders kind-config.yaml.tmpl for a given kindest/node image into a real file that
# `kind create cluster --config` can consume directly. ci-chart.yml's matrix calls this
# same script (into $RUNNER_TEMP) so a developer reproducing a matrix leg locally gets an
# identically rendered cluster, not a re-implementation of the substitution.
#
# Usage: .github/configs/render-kind-config.sh <node-image> [output-file]
#   .github/configs/render-kind-config.sh kindest/node:v1.31.14 /tmp/kind-config.yaml
#   kind create cluster --config /tmp/kind-config.yaml

set -o errexit
set -o nounset
set -o pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

node_image="${1:?usage: render-kind-config.sh <node-image> [output-file]}"
output="${2:-/dev/stdout}"

sed "s#__NODE_IMAGE__#${node_image}#g" "${script_dir}/kind-config.yaml.tmpl" >"${output}"
