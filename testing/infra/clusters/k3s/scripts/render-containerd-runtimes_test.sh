#!/usr/bin/env bash
# Unit test for render-containerd-runtimes.sh: run directly, e.g.
#   bash testing/infra/clusters/k3s/scripts/render-containerd-runtimes_test.sh
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
render="$script_dir/render-containerd-runtimes.sh"
fail=0

assert_output() {
  local name="$1" input="$2" expected="$3"
  local actual
  actual="$(printf '%s' "$input" | bash "$render")"
  if [ "$actual" != "$expected" ]; then
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
expected_ascend='{{ template "base" . }}

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.ascend]
  runtime_type = "io.containerd.runc.v2"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.ascend.options]
  BinaryName = "/usr/local/Ascend/Ascend-Docker-Runtime/ascend-docker-runtime"'
assert_output "ascend + nvidia -> nvidia dropped, ascend kept" "$ascend_and_nvidia" "$expected_ascend"

# nvidia-only: emits nothing.
nvidia_only='{"runtimes": {"nvidia": {"path": "/usr/bin/nvidia-container-runtime"}}}'
assert_output "nvidia-only -> empty" "$nvidia_only" ""

# no .runtimes key at all.
no_runtimes='{"data-root": "/data/docker"}'
assert_output "no .runtimes key -> empty" "$no_runtimes" ""

# missing daemon.json (empty input, e.g. the remote file didn't exist).
assert_output "missing daemon.json -> empty" "" ""

# malformed JSON.
assert_output "malformed JSON -> empty" '{not-json' ""

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
