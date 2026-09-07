#!/usr/bin/env bash
# check-toolpins-selftest.sh - proves the hack/lib validators reject a binary that is not the pin,
# and pins the four that deliberately do not look at a version at all.
#
# A validator in hack/lib/ decides whether a binary already in .sbin may be reused. Accepting one
# by existence alone reuses whatever a pin bump replaced, and the generated tree is then compared
# against a baseline produced by a different tool. The comparison was added for the four tools on
# the `make generate` path, and a check that has only ever been seen to pass cannot be told apart
# from one that cannot fail, so each validator here is handed a binary that is not its pin and its
# verdict is asserted.
#
# Half the rows assert the opposite, and they are the more important half:
#
#   - helm, helm-docs, helm-schema and protoc-gen-validate accept by existence on purpose. They are
#     not on the `make generate` path, so no generated artifact carries their output. Their rows say
#     ACCEPT. A row flipping to REJECT means someone added a version check, which is a deliberate
#     change and belongs in this table, not a surprise.
#   - those same four rows are this script's own liveness check. Every validator enters its accept
#     branch only when `command -v` resolves the planted file, so a plant that silently failed would
#     turn all fifteen rows REJECT - and the four ACCEPT rows are what goes red when it does. A run
#     where every row rejects is not a strict result; it is a broken harness.
#
# The count is part of the assertion: eleven of the fifteen validators compare a version, four do
# not. That split is what the api.yml toolbox-cache comment states, and it is checkable here rather
# than only readable there.
#
# NEVER point ROOT_DIR at the real checkout: hack/lib/ is copied into a throwaway tree so the
# planted files land in that tree's .sbin. Nothing in the real .sbin is read or written.
#
# Usage: bash hack/check-toolpins-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

MINI="$(mktemp -d)"
trap 'rm -rf "$MINI"' EXIT
mkdir -p "$MINI/hack"
cp -R "$ROOT/hack/lib" "$MINI/hack/lib"

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# verdict <validate-fn> <install-fn> <sbin-relative-path>
#
# Plants a file that is not any tool's pinned build - not a Go binary, and silent under every
# version flag - then runs the real validator with its install stubbed out. What is measured is the
# decision, not the network: a validator that reaches install has rejected what it was handed.
function verdict() {
  local validate_fn="$1" install_fn="$2" rel="$3"
  local out

  rm -rf "${MINI:?}/.sbin"
  mkdir -p "$(dirname "$MINI/.sbin/$rel")"
  printf '#!/bin/sh\nexit 0\n' > "$MINI/.sbin/$rel"
  chmod +x "$MINI/.sbin/$rel"

  out="$(
    # shellcheck disable=SC1090,SC1091
    source "$MINI/hack/lib/init.sh"
    eval "${install_fn}() { echo 'REACHED-INSTALL'; return 0; }"
    "${validate_fn}" 2>&1
  )" || true

  case "$out" in
    *REACHED-INSTALL*) echo "reject" ;;
    *) echo "accept" ;;
  esac
}

# One row per validate function in hack/lib/. Every function `grep -n '::validate()' hack/lib/`
# reports is here; a new tool without a row is a tool nothing asserts anything about.
#
#      <validate fn>|<install fn>|<path under .sbin>|<want>|<how it compares>
VALIDATORS=(
  "gpustack::cgo::c_for_go::validate|gpustack::cgo::c_for_go::install|c-for-go|reject|go version -m"
  "gpustack::cgo::c_for_go_c99::validate|gpustack::cgo::c_for_go_c99::install|c-for-go-c99|reject|go version -m"
  "gpustack::helm::helm::validate|gpustack::helm::helm::install|helm|accept|existence, on purpose"
  "gpustack::helm::docs::validate|gpustack::helm::docs::install|helm-docs|accept|existence, on purpose"
  "gpustack::helm::schema::validate|gpustack::helm::schema::install|helm-schema|accept|existence, on purpose"
  "gpustack::protoc::protoc::validate|gpustack::protoc::protoc::install|protoc/bin/protoc|reject|--version"
  "gpustack::protoc::protoc_gen_go::validate|gpustack::protoc::protoc_gen_go::install|protoc/bin/protoc-gen-go|reject|--version"
  "gpustack::protoc::protoc_gen_gogo::validate|gpustack::protoc::protoc_gen_gogo::install|protoc/bin/protoc-gen-gogo|reject|go version -m"
  "gpustack::protoc::protoc_gen_go_grpc::validate|gpustack::protoc::protoc_gen_go_grpc::install|protoc/bin/protoc-gen-go-grpc|reject|--version"
  "gpustack::protoc::protoc_gen_validate::validate|gpustack::protoc::protoc_gen_validate::install|protoc/bin/protoc-gen-validate|accept|existence, on purpose"
  "gpustack::protoc::protoc_gen_grpc_gateway::validate|gpustack::protoc::protoc_gen_grpc_gateway::install|protoc/bin/protoc-gen-grpc-gateway|reject|--version"
  "gpustack::lint::golangci_lint::validate|gpustack::lint::golangci_lint::install|golangci-lint|reject|--version"
  "gpustack::lint::goimports_reviser::validate|gpustack::lint::goimports_reviser::install|goimports-reviser|reject|-version"
  "gpustack::commit::commitsar::validate|gpustack::commit::commitsar::install|commitsar|reject|version"
  "gpustack::lint::goimports::validate|gpustack::lint::goimports::install|goimports|reject|go version -m"
)

echo "== a binary that is not the pin =="
rejects=0
accepts=0
for row in "${VALIDATORS[@]}"; do
  IFS='|' read -r validate_fn install_fn rel want how <<<"$row"
  got="$(verdict "$validate_fn" "$install_fn" "$rel")"
  if [ "$got" = "$want" ]; then
    pass "$rel: $got ($how)"
  else
    fail "$rel: want $want, got $got ($how)"
  fi
  if [ "$got" = "reject" ]; then rejects=$((rejects + 1)); else accepts=$((accepts + 1)); fi
done

# The split itself, asserted rather than counted by a reader. Both halves are named: a drop in
# rejects is a version check that went away, and a drop in accepts is one that appeared.
if [ "$rejects" -eq 11 ] && [ "$accepts" -eq 4 ]; then
  pass "split: 11 compare a version, 4 accept by existence"
else
  fail "split: want 11 comparing and 4 by existence, got $rejects and $accepts"
fi

# The accept side of the `go version -m` family. Its four validators cannot be handed a matching
# binary without installing one, so what is pinned here is the comparison they delegate to.
#
# The subtlety it exists for: a commit pin is written out in full in hack/lib/cgo.sh while go stamps
# twelve characters, so neither equality nor a suffix test can ever hold for those two tools. A
# comparison that got this wrong would reinstall on every run - slower, still green, and invisible.
#
#      <version stamped in the binary>|<pin as hack/lib writes it>|<want>|<what the row is>
COMPARISONS=(
  "v0.49.0|v0.49.0|match|a tag, stamped verbatim"
  "v0.43.0|v0.49.0|differ|the stale goimports a .sbin keeps across a pin bump"
  "v1.3.1-0.20260305101747-6dd1c017f91e|6dd1c017f91eea5364499f7926d94583eaddaadb|match|12 stamped against a 40-character pin"
  "v0.0.0-20220810182948-cef5ec7833f3|cef5ec7833f3274488b3edd519f46ddfb57d5735|match|the same, for c-for-go"
  "v1.3.3-0.20221024144010-f67b8970b736|f67b8970b736|match|12 against 12"
  "v0.0.0-20220810182948-cef5ec7833f3|6dd1c017f91eea5364499f7926d94583eaddaadb|differ|two commits of one module, which existence cannot tell apart"
  "|v0.49.0|differ|not a Go binary, or one carrying no build info"
  "v0.49.0||differ|no pin to compare against"
  "v0.0.0-20220810182948-cef5ec7833f3|cef5ec|differ|a pin under git's floor, which would otherwise prefix-match every commit"
)

echo
echo "== the comparison the four delegate to =="
for row in "${COMPARISONS[@]}"; do
  IFS='|' read -r installed pin want what <<<"$row"
  got="differ"
  if (
    # shellcheck disable=SC1090,SC1091
    source "$MINI/hack/lib/init.sh"
    gpustack::util::go_module_version_is "$installed" "$pin"
  ); then
    got="match"
  fi
  if [ "$got" = "$want" ]; then
    pass "${installed:-<empty>} vs ${pin:-<empty>}: $got ($what)"
  else
    fail "${installed:-<empty>} vs ${pin:-<empty>}: want $want, got $got ($what)"
  fi
done

echo
if [ "$fails" -gt 0 ]; then
  echo "SELFTEST FAILED: $fails case(s)"
  exit 1
fi
# What a green run establishes: each validator's verdict on a binary that is not its pin, and the
# comparison the four go-installed tools delegate to. It does NOT establish that the seven --version
# validators accept a matching binary - that path runs on every `make lint` and `make generate`, and
# a break there stops the build rather than passing quietly.
echo "SELFTEST PASSED"
