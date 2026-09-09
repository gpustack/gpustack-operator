#!/usr/bin/env bash
# check-toolpins-selftest.sh - proves the hack/lib validators reject a binary that is not the pin,
# over the whole set of them rather than over a list someone remembered to extend.
#
# A validator in hack/lib/ decides whether a binary already in .sbin may be reused. Accepting one by
# existence where a version is meant to be compared reuses whatever a pin bump replaced, and a
# committed artifact is then produced by a generator nobody chose.
#
# Four layers, because four different things can be wrong and each hides the others:
#
#   COVERAGE      the set of validators found in the tree and the set this file declares must be
#                 equal, IN BOTH DIRECTIONS. A new tool with no row is the failure this layer
#                 exists for: without it the row assertions below stay green while saying nothing
#                 about the new tool. A row naming a function that no longer exists is the other
#                 direction, and it reads as coverage until someone checks.
#   VERDICT       each validator is handed a binary that is not its pin. Four outcomes are
#                 distinguished, not two: accept, reject, harness-broken and unattributed. Folding
#                 the last two into accept is how a validator that fails outright reads as one that
#                 approved something.
#   ATTRIBUTION   an accept means nothing unless it was caused by the planted file. Every bin()
#                 in hack/lib/ falls back to the bare tool name when .sbin has nothing, so on a
#                 machine that carries the tool a plant that never landed still resolves - to the
#                 host's copy. Each case therefore asserts the resolved path IS the planted one.
#   WIRING        a validator can compare diligently against the wrong pin. One that is declared to
#                 compare is required to name the same pin variables its own installer names, so a
#                 neighbour's pin fails here even though every verdict above stays unchanged; one
#                 declared existence-only is required to name none, so the day it grows a
#                 comparison its row has to say so.
#
# NEVER point ROOT_DIR at the real checkout: hack/lib/ is copied into a throwaway tree so the
# planted files land in that tree's .sbin. Nothing in the real .sbin is read or written.
#
# Usage: bash hack/check-toolpins-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# REQUIRED: the physical path. hack/lib/init.sh derives ROOT_DIR with `pwd -P`, so every path a
# bin() hands back is physical, while `mktemp -d` on macOS answers under /var, a symlink to
# /private/var. The attribution assertion below compares paths, and two spellings of one file are
# not equal as strings.
MINI="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$MINI"' EXIT
mkdir -p "$MINI/hack"
cp -R "$ROOT/hack/lib" "$MINI/hack/lib"
ALL="$MINI/all-lib.sh"
cat "$MINI"/hack/lib/*.sh > "$ALL"

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# One row per validator, and the coverage layer below rejects any disagreement with the tree.
#
# The `want` column is a declaration of intent, and intent is the one thing that cannot be derived:
# a validator that accepts by existence looks identical whether that is a decision or an oversight.
# So every accept row carries the reason it is one, and the same reason sits beside the validator.
#
#      <validate fn>|<path under .sbin>|<want>|<why>
CASES=(
  "gpustack::cgo::c_for_go::validate|c-for-go|reject|go version -m"
  "gpustack::cgo::c_for_go_c99::validate|c-for-go-c99|reject|go version -m"
  "gpustack::helm::helm::validate|helm|accept|existence, deliberately: writes no committed artifact"
  "gpustack::helm::docs::validate|helm-docs|reject|go version -m; writes the committed chart README"
  "gpustack::helm::schema::validate|helm-schema|reject|go version -m; writes the committed values.schema.json"
  "gpustack::protoc::protoc::validate|protoc/bin/protoc|reject|--version"
  "gpustack::protoc::protoc_gen_go::validate|protoc/bin/protoc-gen-go|reject|--version"
  "gpustack::protoc::protoc_gen_gogo::validate|protoc/bin/protoc-gen-gogo|reject|go version -m"
  "gpustack::protoc::protoc_gen_go_grpc::validate|protoc/bin/protoc-gen-go-grpc|reject|--version"
  "gpustack::protoc::protoc_gen_validate::validate|protoc/bin/protoc-gen-validate|accept|existence, deliberately: its only caller has none"
  "gpustack::protoc::protoc_gen_grpc_gateway::validate|protoc/bin/protoc-gen-grpc-gateway|reject|--version"
  "gpustack::lint::golangci_lint::validate|golangci-lint|reject|--version"
  "gpustack::lint::goimports_reviser::validate|goimports-reviser|reject|-version"
  "gpustack::commit::commitsar::validate|commitsar|reject|version"
  "gpustack::lint::goimports::validate|goimports|reject|go version -m"
)

# ---------------------------------------------------------------------------- coverage

echo "== the set of validators, both directions =="
discovered="$(grep -h -oE '^function [A-Za-z0-9_:]+::validate\(\)' "$MINI"/hack/lib/*.sh |
  sed 's/^function //; s/()$//' | sort)"
declared="$(printf '%s\n' "${CASES[@]}" | cut -d'|' -f1 | sort)"

untabled="$(comm -23 <(printf '%s\n' "$discovered") <(printf '%s\n' "$declared"))"
unfound="$(comm -13 <(printf '%s\n' "$discovered") <(printf '%s\n' "$declared"))"

if [ -n "$untabled" ]; then
  while read -r fn; do fail "in hack/lib but not in this table: $fn"; done <<<"$untabled"
else
  pass "every validator in hack/lib has a row"
fi
if [ -n "$unfound" ]; then
  while read -r fn; do fail "in this table but not in hack/lib: $fn"; done <<<"$unfound"
else
  pass "every row names a validator that exists"
fi

# ---------------------------------------------------------------------------- verdicts

# verdict <validate-fn> <sbin-relative-path> <plant-shape>
#
# Plants a file that is not any tool's pinned build, then runs the real validator with its own
# installer stubbed out. What is measured is the decision, not the network: a validator that
# reaches its installer has rejected what it was handed.
#
# Two shapes, and the second is the one with teeth. A SILENT plant answers every version flag with
# nothing, which a validator comparing against its pin rejects - and so does a validator that has
# regressed to accepting any non-empty output, because nothing is not non-empty. A SPEAKING plant
# is a Go executable with a non-pin module version and prints a non-pin `tag` version. Those are the
# two formats the seven parsers that cannot read a generic shell response need. The assertion below
# confirms each parser reads its intended non-pin value before its row is judged.
#
# Prints exactly one of:
#   accept        returned 0 without reaching the installer
#   reject        reached the installer
#   unattributed  its bin() resolved to something other than the planted file, so whatever it
#                 decided, it did not decide it about this plant
#   broken        anything else, which is the default on purpose: a validator that was renamed,
#                 deleted or died, or a speaking fixture/parser precondition failed, must not read
#                 as one that approved something
function prepare_speaking_fixture() {
  if [ -x "$MINI/speaking-bin" ]; then
    return 0
  fi
  if ! command -v go > /dev/null; then
    echo "SPEAKING-FIXTURE-FAILED:go is unavailable" >&2
    return 1
  fi

  mkdir -p "$MINI/cmd/speaking"
  printf 'module example.com/speaking\n' > "$MINI/go.mod"
  printf '%s\n' \
    'package main' \
    '' \
    'import (' \
    '  "fmt"' \
    '  "os"' \
    ')' \
    '' \
    'func main() {' \
    '  for _, arg := range os.Args[1:] {' \
    '    if arg == "-version" {' \
    '      fmt.Println("tag v0.0.0-not-any-pin")' \
    '      return' \
    '    }' \
    '  }' \
    '  fmt.Println("stale 0.0.0-not-any-pin built from 0000000 on 1970-01-01T00:00:00Z")' \
    '}' > "$MINI/cmd/speaking/main.go"
  if ! (cd "$MINI" && go build -o "$MINI/speaking-bin" ./cmd/speaking); then
    echo "SPEAKING-FIXTURE-FAILED:go build" >&2
    return 1
  fi
}

function verdict() {
  local validate_fn="$1" rel="$2" shape="$3" why="$4"
  local base="${validate_fn%::validate}"
  local out

  rm -rf "${MINI:?}/.sbin"
  mkdir -p "$(dirname "$MINI/.sbin/$rel")"
  if [ "$shape" = speaking ]; then
    if ! prepare_speaking_fixture; then
      echo "broken"
      return 0
    fi
    if ! cp "$MINI/speaking-bin" "$MINI/.sbin/$rel"; then
      echo "SPEAKING-FIXTURE-FAILED:copy" >&2
      echo "broken"
      return 0
    fi
  else
    printf '#!/bin/sh\nexit 0\n' > "$MINI/.sbin/$rel"
    chmod +x "$MINI/.sbin/$rel"
  fi

  out="$(
    # shellcheck disable=SC1090,SC1091
    source "$MINI/hack/lib/init.sh"

    # Stubbing an installer that is not the one this validator calls would let the real one run, so
    # the convention this derivation relies on is asserted rather than assumed.
    declare -F "${base}::install" > /dev/null || { echo "NO-INSTALL-FN"; exit 0; }
    declare -F "${base}::bin" > /dev/null || { echo "NO-BIN-FN"; exit 0; }

    resolved="$(command -v "$("${base}"::bin)" || true)"
    if [ "$resolved" != "$MINI/.sbin/$rel" ]; then
      echo "UNATTRIBUTED:${resolved:-<nothing>}"
      exit 0
    fi

    if [ "$shape" = speaking ]; then
      case "$why" in
        "go version -m"*)
          parsed="$(gpustack::util::go_module_version "$("${base}"::bin)")"
          [ "$parsed" = "(devel)" ] || { echo "SPEAKING-PARSE-FAILED:${parsed:-<empty>}"; exit 0; }
          ;;
        "-version")
          # REQUIRED: mirror the parser in gpustack::lint::goimports_reviser::validate so this
          # assertion proves the validator receives the speaking value it will compare.
          parsed="$("$("${base}"::bin)" -version | grep tag | cut -d " " -f 2 2>&1 | head -n 1)"
          [ "$parsed" = "v0.0.0-not-any-pin" ] || { echo "SPEAKING-PARSE-FAILED:${parsed:-<empty>}"; exit 0; }
          ;;
      esac
    fi

    eval "${base}::install() { echo 'REACHED-INSTALL'; return 0; }"
    "${validate_fn}" 2>&1
    echo "VALIDATOR-RC:$?"
  )" || true

  case "$out" in
    *UNATTRIBUTED:*|*NO-INSTALL-FN*|*NO-BIN-FN*) echo "unattributed" ;;
    *REACHED-INSTALL*) echo "reject" ;;
    *VALIDATOR-RC:0*) echo "accept" ;;
    *) echo "broken" ;;
  esac
}

echo
echo "== a binary that is not the pin =="
rejects=0
accepts=0
for shape in silent speaking; do
  for row in "${CASES[@]}"; do
    IFS='|' read -r validate_fn rel want why <<<"$row"
    got="$(verdict "$validate_fn" "$rel" "$shape" "$why")"
    if [ "$got" = "$want" ]; then
      pass "$rel [$shape]: $got ($why)"
    else
      fail "$rel [$shape]: want $want, got $got ($why)"
    fi
    # The split is counted once, off the silent pass; the speaking pass asserts the same want and
    # would double every number.
    if [ "$shape" = silent ]; then
      case "$got" in
        reject) rejects=$((rejects + 1)) ;;
        accept) accepts=$((accepts + 1)) ;;
      esac
    fi
  done
done

# ---------------------------------------------------------------------------- wiring

# A validator that compares against a neighbour's pin rejects its own cached binary and accepts the
# other one, and every verdict above is unchanged by that - the planted file matches neither pin, so
# it is rejected either way. What separates the two is which pin the comparison names, and the
# answer has to agree with the pin the installer installs.
#
# Log messages are skipped, and that exclusion is what makes this layer say anything at all about
# the existence-only validators. They name their pin once, in `installing X ${x_version}`, so a
# rule that counted every mention passed them by accident and would have called the deletion of
# that one message a wiring error. Outside the log messages the two classes are actually different:
# a validator that compares names its pin, and one that does not name none.
function pins_of() {
  awk -v fn="function $1() {" '
    index($0, fn) == 1 { inside = 1 }
    inside {
      if ($0 ~ /gpustack::log::/) next
      s = $0
      while (match(s, /\$\{[a-z_][a-z_0-9]*_version[#}]/)) {
        print substr(s, RSTART + 2, RLENGTH - 3)
        s = substr(s, RSTART + RLENGTH)
      }
    }
    inside && /^}/ { exit }
  ' "$ALL" | sort -u | tr '\n' ' '
}

# want_of <validate-fn>: the want column of that validator's row, empty when it has no row. A
# validator with no row is already a coverage failure named above, and reporting it a second time
# here would describe it as a wiring problem, which it is not.
function want_of() {
  local fn="$1" row
  for row in "${CASES[@]}"; do
    case "$row" in
      "${fn}|"*) IFS='|' read -r _ _ w _ <<<"$row"; printf '%s' "$w"; return 0 ;;
    esac
  done
}

echo
echo "== the pin each validator names is the pin its installer installs =="
while read -r fn; do
  base="${fn%::validate}"
  want="$(want_of "$fn")"
  [ -n "$want" ] || continue
  vp="$(pins_of "${base}::validate")"
  ip="$(pins_of "${base}::install")"
  if [ "$want" = accept ]; then
    # Declared existence-only. Both values carry information: no pin outside the log messages is
    # the declaration holding, and a pin appearing there means the validator changed class and the
    # row has to follow it.
    if [ -n "$vp" ]; then
      fail "${base}: declared existence-only, yet compares [${vp% }] -- its row needs to change with it"
    elif [ -z "$ip" ]; then
      fail "${base}: its installer names no pin, so there is no version for it to install"
    else
      pass "${base}: existence-only, and names no pin outside its log messages"
    fi
  elif [ -z "$vp" ]; then
    fail "${base}: declared to compare a pin, yet names none outside its log messages"
  elif [ "$vp" = "$ip" ]; then
    pass "${base}: compares ${vp% }"
  else
    fail "${base}: compares [${vp% }] while its installer installs [${ip% }]"
  fi
done <<<"$discovered"

# ---------------------------------------------------------------------------- the comparison

# The accept side of the go-installed family. Those validators cannot be handed a matching binary
# without installing one, so what is pinned here is the comparison they delegate to.
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
echo "== the comparison the go-installed validators delegate to =="
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
# The split is read off the measured verdicts rather than written down: a second copy of a number
# drifts from the first, and the coverage layer above is what keeps the denominator honest.
echo "$(printf '%s\n' "$discovered" | wc -l | tr -d ' ') validators: ${rejects} compare a version, ${accepts} accept by existence"
# What a green run establishes: every validator in hack/lib has a row; each one's verdict on a
# binary that is not its pin, silent and speaking, came from the file this script planted; and each
# compares the pin its own installer installs, or compares none and is declared that way.
#
# What it does NOT establish, both turning on the want column being a declaration and not a
# measurement:
#
#   - that a validator which OUGHT to compare a version does. One declared existence-only that does
#     not compare passes every layer here. Only the other direction is checked: declared
#     existence-only while actually comparing fails the wiring layer.
#   - that a flag-comparing validator ACCEPTS a matching binary. Asserting it needs a fixture per
#     output format, and a fixture is a copy of a format - it stays green after the tool changes
#     its own. That direction runs on every `make lint` and `make generate`, which is not the same
#     as being gated by them: measured, with one validator regressed to accept any non-empty output
#     and a stand-in reporting the wrong version, `make lint` exits 0 and reports nothing.
echo "SELFTEST PASSED"
