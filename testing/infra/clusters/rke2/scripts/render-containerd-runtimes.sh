#!/usr/bin/env bash
# Reads a Docker daemon.json (stdin) and writes the RKE2 containerd config
# template stanzas for its non-nvidia runtimes to stdout, one
# [plugins...runtimes.<name>] / [...runtimes.<name>.options] pair per runtime.
# Takes the containerd config version -- 2 or 3 -- as its only argument, since
# the CRI plugin is named io.containerd.grpc.v1.cri in version 2 and
# io.containerd.cri.v1.runtime in version 3: containerd silently ignores a
# runtime declared under the other version's path, so config.toml.tmpl (v2) and
# config-v3.toml.tmpl (v3) need separate renderings, and an unknown version is
# rejected rather than guessed.
# Writes nothing (and always exits 0) when daemon.json is missing, malformed,
# has no .runtimes, or only has nvidia -- nvidia is always dropped since RKE2
# auto-detects and wires it. Never invokes jq (or anything else) remotely --
# the caller pipes in content already fetched from the remote host.
set -uo pipefail

case "${1:-}" in
2) cri_plugin="io.containerd.grpc.v1.cri" ;;
3) cri_plugin="io.containerd.cri.v1.runtime" ;;
*)
  echo "usage: $(basename "$0") <containerd config version: 2|3> < daemon.json" >&2
  exit 2
  ;;
esac

daemon_json="$(cat)"

jq -r --arg cri "$cri_plugin" '
  try (
    (.runtimes // {})
    | to_entries
    | map(select(.key != "nvidia" and (.key | test("^[A-Za-z0-9_-]+$"))))
    | map(select(try ((.value | type) == "object" and (.value.path | type) == "string" and (.value.path | length) > 0) catch false))
  ) catch []
  | if length == 0 then
      empty
    else
      (
        ["{{ template \"base\" . }}"]
        + (map(
            "\n[plugins.\"" + $cri + "\".containerd.runtimes." + .key + "]\n  runtime_type = \"io.containerd.runc.v2\"\n\n[plugins.\"" + $cri + "\".containerd.runtimes." + .key + ".options]\n  BinaryName = " + (.value.path | @json)
          ))
      )
      | join("\n")
    end
' <<<"$daemon_json" 2>/dev/null

exit 0
