#!/usr/bin/env bash
# Reads a Docker daemon.json (stdin) and writes the k3s containerd
# config.toml.tmpl stanzas for its non-nvidia runtimes to stdout, one
# [plugins...runtimes.<name>] / [...runtimes.<name>.options] pair per runtime.
# Writes nothing (and always exits 0) when daemon.json is missing, malformed,
# has no .runtimes, or only has nvidia -- nvidia is always dropped since k3s
# auto-detects and wires it. Never invokes jq (or anything else) remotely --
# the caller pipes in content already fetched from the remote host.
set -uo pipefail

daemon_json="$(cat)"

jq -r '
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
            "\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes." + .key + "]\n  runtime_type = \"io.containerd.runc.v2\"\n\n[plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes." + .key + ".options]\n  BinaryName = " + (.value.path | @json)
          ))
      )
      | join("\n")
    end
' <<<"$daemon_json" 2>/dev/null

exit 0
