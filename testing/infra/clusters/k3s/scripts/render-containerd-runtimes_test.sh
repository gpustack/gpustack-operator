#!/usr/bin/env bash
# Unit test for render-containerd-runtimes.sh: run directly, e.g.
#   bash testing/infra/clusters/k3s/scripts/render-containerd-runtimes_test.sh
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
render="$script_dir/render-containerd-runtimes.sh"
fail=0

assert_output() {
  local name="$1" config_version="$2" input="$3" expected="$4"
  # The exit code is asserted alongside the output: the renderer's contract is to print the runtimes
  # AND exit 0, and comparing only stdout leaves a case that cannot fail on a nonzero exit.
  local actual rc=0
  actual="$(printf '%s' "$input" | bash "$render" "$config_version")" || rc=$?
  if [ "$rc" != 0 ]; then
    echo "FAIL: $name (the renderer exited $rc)"
    fail=1
  elif [ "$actual" != "$expected" ]; then
    echo "FAIL: $name"
    echo "--- expected ---"
    printf '%s\n' "$expected"
    echo "--- actual ---"
    printf '%s\n' "$actual"
    fail=1
  else
    echo "PASS: $name"
  fi
}

# ascend + nvidia: nvidia is dropped, ascend is kept.
ascend_and_nvidia='{
  "default-runtime": "ascend",
  "runtimes": {
    "ascend": {
      "path": "/usr/local/Ascend/Ascend-Docker-Runtime/ascend-docker-runtime",
      "runtimeArgs": []
    },
    "nvidia": {
      "path": "/usr/bin/nvidia-container-runtime",
      "runtimeArgs": []
    }
  }
}'
expected_ascend_v2='{{ template "base" . }}

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.ascend]
  runtime_type = "io.containerd.runc.v2"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.ascend.options]
  BinaryName = "/usr/local/Ascend/Ascend-Docker-Runtime/ascend-docker-runtime"'
assert_output "config v2 -> v2 CRI plugin path, nvidia dropped, ascend kept" 2 "$ascend_and_nvidia" "$expected_ascend_v2"

# The same runtimes for containerd 2's config version 3, whose CRI plugin is named
# io.containerd.cri.v1.runtime. containerd ignores (without complaint) a runtime declared
# under the other version's plugin path, so config-v3.toml.tmpl must carry this one.
expected_ascend_v3='{{ template "base" . }}

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.ascend]
  runtime_type = "io.containerd.runc.v2"

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.ascend.options]
  BinaryName = "/usr/local/Ascend/Ascend-Docker-Runtime/ascend-docker-runtime"'
assert_output "config v3 -> v3 CRI plugin path, nvidia dropped, ascend kept" 3 "$ascend_and_nvidia" "$expected_ascend_v3"

# nvidia-only: emits nothing.
nvidia_only='{"runtimes": {"nvidia": {"path": "/usr/bin/nvidia-container-runtime"}}}'
assert_output "nvidia-only -> empty" 3 "$nvidia_only" ""

# no .runtimes key at all.
no_runtimes='{"data-root": "/data/docker"}'
assert_output "no .runtimes key -> empty" 3 "$no_runtimes" ""

# missing daemon.json (empty input, e.g. the remote file didn't exist).
assert_output "missing daemon.json -> empty" 3 "" ""

# malformed JSON.
assert_output "malformed JSON -> empty" 3 '{not-json' ""

# An absent or unknown config version is a caller mistake, not an input to tolerate: guessing
# one would silently write a template the other containerd version discards. It must fail
# loudly and emit nothing.
for bad_version in "" 1 v3; do
  if out="$(printf '%s' "$ascend_and_nvidia" | bash "$render" $bad_version 2>/dev/null)" || [ -n "$out" ]; then
    echo "FAIL: config version '$bad_version' -> must exit non-zero and emit nothing"
    fail=1
  else
    echo "PASS: config version '$bad_version' -> rejected"
  fi
done

# This script only ever transforms stdin -> stdout; it must never shell out to
# ssh (or anything remote), since the whole point is that parsing happens locally.
if grep -q '\bssh\b' "$render"; then
  echo "FAIL: render script must not invoke ssh (parsing must stay local, no remote jq)"
  fail=1
else
  echo "PASS: render script never invokes ssh"
fi

if [ "$fail" -ne 0 ]; then
  echo "one or more assertions failed"
  exit 1
fi
echo "all assertions passed"
