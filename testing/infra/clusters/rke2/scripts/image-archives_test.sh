#!/usr/bin/env bash
# Unit test for image-archives.sh: run directly, e.g.
#   bash testing/infra/clusters/rke2/scripts/image-archives_test.sh
#
# Entirely offline. curl and uname are stubbed on PATH: curl serves a fake release directory and
# records every URL it is asked for, so "a warm cache downloads nothing" is asserted by counting
# calls rather than by reading a log line. RKE2_AGENT_IMAGES_DIR points the staging step at a
# scratch directory, so the test needs no root.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$script_dir/image-archives.sh"
release="v1.34.9+rke2r1"
fail=0

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# --- fixture ----------------------------------------------------------------

# A scratch world per case: a fake release server, an empty cache root, an empty images dir, a
# staging directory path, and stub curl/uname on PATH.
new_world() {
  world="$(mktemp -d)"
  serve="$world/release"
  cache_root="$world/cache"
  cache="$cache_root/$release"
  images="$world/images"
  staging="$world/staging"
  calls="$world/curl.log"
  mkdir -p "$serve" "$cache_root" "$images" "$world/bin"
  : >"$calls"

  cat >"$world/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Serves $SERVE_DIR by URL basename and logs every full URL it is asked for -- a mirror mode
# serves the same basenames from a different host, so the basename alone cannot tell them apart.
# Exits like curl -f on a miss.
dest=""; url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) dest="$2"; shift 2 ;;
    https://*) url="$1"; shift ;;
    *) shift ;;
  esac
done
echo "$url" >> "$CURL_LOG"
src="$SERVE_DIR/$(basename "$url")"
[ -f "$src" ] || exit 22
cp "$src" "$dest"
STUB

  cat >"$world/bin/uname" <<'STUB'
#!/usr/bin/env bash
[ "${1:-}" = "-m" ] && { echo "$FAKE_MACHINE"; exit 0; }
exec /usr/bin/uname "$@"
STUB

  chmod +x "$world/bin/curl" "$world/bin/uname"
  # https://get.rke2.io is not a release asset; the stub resolves it by basename like any other.
  # The cn mirror's installer copy is https://rancher-mirror.rancher.cn/rke2/install.sh, served
  # here by its own basename.
  printf '#!/bin/sh\n# INSTALL_RKE2_ARTIFACT_PATH is read here\n' >"$serve/get.rke2.io"
  printf '#!/bin/sh\n# INSTALL_RKE2_ARTIFACT_PATH is read here\n' >"$serve/install.sh"
}

# Publishes the release assets for one arch and (re)writes the checksum file to match them. Every
# extra argument is "<name>:<content>"; the four standard assets are published by default.
publish() {
  local arch="$1" entry name
  shift
  printf 'binary-%s' "$arch" >"$serve/rke2.linux-${arch}.tar.gz"
  printf 'core-images-%s' "$arch" >"$serve/rke2-images.linux-${arch}.tar.zst"
  printf 'calico-images-%s' "$arch" >"$serve/rke2-images-calico.linux-${arch}.tar.zst"
  printf 'docker.io/rancher/hardened-calico:v3.32.0-build20260604\n' >"$serve/rke2-images.linux-${arch}.txt"
  for entry in "$@"; do
    name="${entry%%:*}"
    printf '%s' "${entry#*:}" >"$serve/$name"
  done
  (cd "$serve" && for f in rke2*; do
    [ -f "$f" ] && printf '%s  %s\n' "$(sha256_of "$f")" "$f"
  done) >"$serve/sha256sum-${arch}.txt" 2>/dev/null
}

# run <subcommand> [machine] [extra args...]
run() {
  local sub="$1" machine="${2:-x86_64}"
  shift 2 || shift || true
  env PATH="$world/bin:$PATH" \
    CURL_LOG="$calls" SERVE_DIR="$serve" FAKE_MACHINE="$machine" \
    RKE2_AGENT_IMAGES_DIR="$images" \
    bash "$script" "$sub" --release "$release" --cache-dir "$cache_root" "$@" \
    >"$world/out" 2>"$world/err"
}

drop_world() { rm -rf "$world"; }

# --- assertions -------------------------------------------------------------

ok() { echo "PASS: $1"; }
no() {
  echo "FAIL: $1"
  [ -n "${2:-}" ] && printf '  %s\n' "$2"
  fail=1
}

assert_file() {
  if [ -f "$2" ]; then ok "$1"; else no "$1" "missing: $2"; fi
}
assert_no_file() {
  if [ -f "$2" ]; then no "$1" "present but should not be: $2"; else ok "$1"; fi
}
assert_eq() {
  if [ "$2" = "$3" ]; then ok "$1"; else no "$1" "expected [$3], got [$2]"; fi
}
assert_contains() {
  if printf '%s' "$2" | grep -qF -- "$3"; then ok "$1"; else no "$1" "[$3] not found in: $2"; fi
}
assert_not_contains() {
  if printf '%s' "$2" | grep -qF -- "$3"; then no "$1" "[$3] unexpectedly found in: $2"; else ok "$1"; fi
}
# Directory contents are asserted through the directory itself, not through `ls` output: a failing
# `ls` yields an empty string, which would make "nothing was staged" pass for the wrong reason.
dir_entries() { find "$1" -mindepth 1 -maxdepth 1 -exec basename {} \; | sort | tr '\n' ' '; }
assert_dir_holds() {
  local name="$1" dir="$2" want="$3" got
  if [ ! -d "$dir" ]; then
    no "$name" "not a directory: $dir"
    return
  fi
  got="$(dir_entries "$dir")"
  got="${got% }"
  if [ "$got" = "$want" ]; then ok "$name"; else no "$name" "expected [$want], got [$got]"; fi
}
assert_run_fails() {
  local name="$1"
  shift
  if run "$@"; then no "$name" "exited 0; stdout: $(cat "$world/out")"; else ok "$name"; fi
}
# The call log holds full URLs; asset-level assertions still count by basename.
calls_for() { awk -F/ '{print $NF}' "$calls" | grep -cxF "$1"; }

# --- fetch ------------------------------------------------------------------

# 1. Cold cache with cni=calico: the anchor first, then the binary, the core image set, the CNI
#    extra, and the installer. The image list is metadata and is NOT fetched unless asked for.
new_world
publish amd64
run fetch x86_64 --cni calico
assert_eq "cold fetch -> exits 0" "$?" "0"
assert_dir_holds "cold fetch -> exactly the wanted files, no partials" "$cache" \
  "install.sh rke2-images-calico.linux-amd64.tar.zst rke2-images.linux-amd64.tar.zst rke2.linux-amd64.tar.gz sha256sum-amd64.txt"
assert_eq "cold fetch -> installer taken from get.rke2.io" "$(calls_for get.rke2.io)" "1"
assert_contains "cold fetch -> says the installer carries no published digest" "$(cat "$world/out")" "covered by no published digest"

# 2. Warm cache: nothing is fetched at all.
: >"$calls"
run fetch x86_64 --cni calico
assert_eq "warm fetch -> exits 0" "$?" "0"
assert_eq "warm fetch -> zero downloads" "$(wc -l <"$calls" | tr -d ' ')" "0"

# 3. artifacts: an allowlist, into a directory this module owns. The image list, the installer and
#    an operator's own bundle must not be there -- the installer copies every file matching
#    rke2-images-*.linux-<arch>* without looking at the extension.
printf 'our-own-images' >"$cache/gpustack-images.tar.zst"
printf 'half-a-download' >"$cache/partial.rke2-images.linux-amd64.tar.gz"
run artifacts x86_64 --cni calico --staging-dir "$staging"
assert_eq "artifacts -> exits 0" "$?" "0"
assert_dir_holds "artifacts -> exactly the four files the installer needs" "$staging" \
  "rke2-images-calico.linux-amd64.tar.zst rke2-images.linux-amd64.tar.zst rke2.linux-amd64.tar.gz sha256sum-amd64.txt"

# 4. stage: after the install. The binary tarball is never staged as an image archive, a .txt never
#    is, a leftover partial never is, and an operator's own bundle is staged and logged unverified.
run stage x86_64
assert_eq "stage -> exits 0" "$?" "0"
assert_dir_holds "stage -> archives only, no binary tarball, no .txt, no partial" "$images" \
  "gpustack-images.tar.zst rke2-images-calico.linux-amd64.tar.zst rke2-images.linux-amd64.tar.zst"
assert_contains "stage -> operator bundle logged unverified" "$(cat "$world/out")" "gpustack-images.tar.zst -- unverified"
assert_contains "stage -> says why the binary tarball is skipped" "$(cat "$world/out")" "not an image archive"

# 5. Re-staging copies nothing: the installer's own copies are already there at the same size.
run stage x86_64
assert_eq "re-stage -> exits 0" "$?" "0"
assert_contains "re-stage -> nothing copied again" "$(cat "$world/out")" "0 archive(s) copied, 3 already present"
drop_world

# 6. A foreign-arch binary tarball in a mixed-arch cache is not staged either: containerd would
#    try to import it as images and fail at every start.
new_world
publish arm64
run fetch aarch64
printf 'binary-amd64' >"$cache/rke2.linux-amd64.tar.gz"
run stage aarch64
assert_eq "foreign-arch binary tarball -> exits 0" "$?" "0"
assert_dir_holds "foreign-arch binary tarball -> not staged" "$images" "rke2-images.linux-arm64.tar.zst"
drop_world

# 7. The CNI extra is derived from --cni, not remembered: canal and none need nothing extra (the
#    default set already carries hardened-calico and hardened-flannel), and cilium needs its own.
for pair in "canal:" "none:" "cilium:rke2-images-cilium.linux-amd64.tar.zst " "flannel:rke2-images-flannel.linux-amd64.tar.zst "; do
  cni="${pair%%:*}"
  extra="${pair#*:}"
  new_world
  publish amd64 "rke2-images-cilium.linux-amd64.tar.zst:cilium-images" \
    "rke2-images-flannel.linux-amd64.tar.zst:flannel-images"
  run fetch x86_64 --cni "$cni"
  assert_dir_holds "cni=${cni} -> ${extra:-no extra} fetched" "$cache" \
    "install.sh ${extra}rke2-images.linux-amd64.tar.zst rke2.linux-amd64.tar.gz sha256sum-amd64.txt"
  drop_world
done

# 8. The release's image list is never fetched, requested, or staged. Nothing reads a tag out of it
#    any more -- the multi-NIC fix takes its image from the running calico-node -- and a .txt in the
#    images directory means "pull every image named in me", the opposite of the intent.
new_world
publish amd64
run fetch x86_64 --cni calico
assert_no_file "the image list is never cached" "$cache/rke2-images.linux-amd64.txt"
assert_eq "the image list is never even requested" \
  "$(grep -c 'rke2-images.linux-amd64.txt' "$calls")" "0"
run artifacts x86_64 --cni calico --staging-dir "$staging"
run stage x86_64
assert_no_file "the image list is never staged as an image" "$images/rke2-images.linux-amd64.txt"
run fetch x86_64 --with-image-list
assert_eq "--with-image-list is no longer an option" "$?" "2"
drop_world

# --- the cache protocol -----------------------------------------------------

# 9. A truncated cached archive self-heals in exactly one re-download.
new_world
publish amd64
mkdir -p "$cache"
cp "$serve/sha256sum-amd64.txt" "$cache/"
printf 'core-imag' >"$cache/rke2-images.linux-amd64.tar.zst"
run fetch x86_64
assert_eq "truncated archive -> exits 0" "$?" "0"
assert_eq "truncated archive -> re-downloaded exactly once" "$(calls_for rke2-images.linux-amd64.tar.zst)" "1"
assert_eq "truncated archive -> cache holds the real content" "$(cat "$cache/rke2-images.linux-amd64.tar.zst")" "core-images-amd64"
drop_world

# 10. A stale anchor is refreshed before the artifact is blamed -- both when the archive is already
#     cached, and (4b) when nothing is cached at all, which is where an operator who copied another
#     release's checksum file into this directory lands on the very first run.
new_world
publish amd64
mkdir -p "$cache"
cp "$serve/rke2-images.linux-amd64.tar.zst" "$serve/rke2.linux-amd64.tar.gz" "$cache/"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "rke2-images.linux-amd64.tar.zst" >"$cache/sha256sum-amd64.txt"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "rke2.linux-amd64.tar.gz" >>"$cache/sha256sum-amd64.txt"
run fetch x86_64
assert_eq "stale anchor, archives cached -> exits 0" "$?" "0"
assert_eq "stale anchor, archives cached -> anchor refreshed once" "$(calls_for sha256sum-amd64.txt)" "1"
assert_eq "stale anchor, archives cached -> archive not re-downloaded" "$(calls_for rke2-images.linux-amd64.tar.zst)" "0"
drop_world

new_world
publish amd64
mkdir -p "$cache"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "rke2.linux-amd64.tar.gz" >"$cache/sha256sum-amd64.txt"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "rke2-images.linux-amd64.tar.zst" >>"$cache/sha256sum-amd64.txt"
run fetch x86_64
assert_eq "stale anchor, empty cache -> exits 0" "$?" "0"
assert_eq "stale anchor, empty cache -> anchor refreshed once" "$(calls_for sha256sum-amd64.txt)" "1"
assert_eq "stale anchor, empty cache -> binary downloaded once" "$(calls_for rke2.linux-amd64.tar.gz)" "1"
assert_eq "stale anchor, empty cache -> cache holds the real binary" "$(cat "$cache/rke2.linux-amd64.tar.gz")" "binary-amd64"
drop_world

# 11. A download of the anchor that succeeds having written nothing must not reach the final name:
#     everything downstream would be judged against an empty list of digests.
new_world
publish amd64
: >"$serve/sha256sum-amd64.txt"
assert_run_fails "empty checksum response -> fails" fetch x86_64
assert_contains "empty checksum response -> says what it did not get" "$(cat "$world/err")" "did not return a sha256 checksum list"
assert_no_file "empty checksum response -> anchor never promoted" "$cache/sha256sum-amd64.txt"
drop_world

# 12. A cache with only the anchor, on a node that cannot reach the release assets, fails naming the
#     cache rather than installing a cluster that pulls every image.
new_world
publish amd64
mkdir -p "$cache"
cp "$serve/sha256sum-amd64.txt" "$cache/"
rm -f "$serve"/rke2*
assert_run_fails "offline with only the anchor -> fails" fetch x86_64
assert_contains "offline with only the anchor -> names the cache" "$(cat "$world/err")" "$cache"
drop_world

# 13. .tar.zst absent from the anchor: the suffix comes from what the release actually publishes.
new_world
publish amd64
rm -f "$serve/rke2-images.linux-amd64.tar.zst"
publish amd64 "rke2-images.linux-amd64.tar.gz:gz-only-core"
rm -f "$serve/rke2-images.linux-amd64.tar.zst"
(cd "$serve" && for f in rke2*; do
  [ -f "$f" ] && printf '%s  %s\n' "$(sha256_of "$f")" "$f"
done) >"$serve/sha256sum-amd64.txt"
run fetch x86_64
assert_eq "zst absent -> exits 0" "$?" "0"
assert_file "zst absent -> falls back to .tar.gz" "$cache/rke2-images.linux-amd64.tar.gz"
run artifacts x86_64 --staging-dir "$staging"
assert_file "zst absent -> the gz is what the installer is given" "$staging/rke2-images.linux-amd64.tar.gz"
drop_world

# 14. Both spellings of both machine types RKE2 publishes for, and an unknown one. RKE2 has no
#     32-bit arm build, so armv7l is a refusal rather than a guess.
for pair in "x86_64:amd64" "amd64:amd64" "aarch64:arm64" "arm64:arm64"; do
  machine="${pair%%:*}"
  arch="${pair#*:}"
  new_world
  publish "$arch"
  run fetch "$machine"
  assert_file "uname -m '$machine' -> ${arch} artifacts" "$cache/rke2.linux-${arch}.tar.gz"
  drop_world
done

for machine in armv7l s390x; do
  new_world
  publish amd64
  assert_run_fails "machine type '$machine' -> refused" fetch "$machine"
  assert_contains "machine type '$machine' -> names the value" "$(cat "$world/err")" "'$machine'"
  assert_eq "machine type '$machine' -> downloads nothing" "$(wc -l <"$calls" | tr -d ' ')" "0"
  drop_world
done

# --- argument handling ------------------------------------------------------

# 15. A staging directory inside the cache would be wiped along with the operator's own files.
new_world
publish amd64
run fetch x86_64
assert_run_fails "staging dir inside the cache -> refused" artifacts x86_64 --staging-dir "$cache_root/inside"
assert_contains "staging dir inside the cache -> says why" "$(cat "$world/err")" "must be outside --cache-dir"
assert_run_fails "artifacts without a staging dir -> refused" artifacts x86_64
drop_world

# 16. A relative cache directory is refused rather than resolved against whatever the provisioner's
#     working directory happens to be on the node.
if out="$(bash "$script" fetch --release "$release" --cache-dir relative/path 2>&1)"; then
  no "relative --cache-dir -> refused" "exited 0"
else
  assert_contains "relative --cache-dir -> refused" "$out" "must be absolute"
fi
if out="$(bash "$script" frobnicate --release "$release" --cache-dir "$(mktemp -d)" 2>&1)"; then
  no "unknown subcommand -> refused" "exited 0"
else
  assert_contains "unknown subcommand -> refused" "$out" "usage:"
fi

# 17. A cache reached through a SYMLINK into RKE2's data directory is refused -- and refused on the
#     FIRST run, when the release directory does not exist yet. That is the case a guard gated on
#     "the path already exists" skipped entirely, leaving a cache the uninstall deletes before it can
#     ever be warm. Two shapes: the symlink at the cache root, and at the release directory itself.
for shape in root release; do
  new_world
  publish amd64
  datadir="$world/datadir"
  mkdir -p "$datadir"
  if [ "$shape" = root ]; then
    ln -s "$datadir" "$cache_root/link"
    given="$cache_root/link"
  else
    mkdir -p "$cache_root/plain"
    ln -s "$datadir" "$cache_root/plain/$release"
    given="$cache_root/plain"
  fi
  out="$(env PATH="$world/bin:$PATH" CURL_LOG="$calls" SERVE_DIR="$serve" FAKE_MACHINE=x86_64 \
    RKE2_AGENT_IMAGES_DIR="$images" GPUSTACK_RKE2_DATA_DIR="$datadir" \
    bash "$script" fetch --release "$release" --cache-dir "$given" 2>&1)" &&
    no "symlinked cache ($shape) -> refused" "exited 0" ||
    assert_contains "symlinked cache ($shape) -> refused, naming the resolved path" "$out" "$datadir"
  assert_eq "symlinked cache ($shape) -> downloads nothing" "$(wc -l <"$calls" | tr -d ' ')" "0"
  drop_world
done

# 18. An option given without a value says so, instead of dying inside `shift 2`.
scratch="$(mktemp -d)"
for flag in --release --cache-dir --cni --staging-dir; do
  if out="$(bash "$script" fetch --release "$release" --cache-dir "$scratch" "$flag" 2>&1)"; then
    no "$flag with no value -> refused" "exited 0"
  else
    assert_contains "$flag with no value -> says which option" "$out" "$flag needs a value"
  fi
done
assert_dir_holds "an invocation that is refused creates nothing" "$scratch" ""
if out="$(bash "$script" frobnicate --release "$release" --cache-dir "$scratch" 2>&1)"; then
  no "unknown subcommand -> refused before any side effect" "exited 0"
else
  assert_dir_holds "unknown subcommand -> creates nothing" "$scratch" ""
fi
rm -rf "$scratch"

# 19. Default mode downloads from the places it always has: github.com for the release assets
#     (tag percent-encoded) and get.rke2.io for the installer. Pinned explicitly because the cn
#     mode below changes where both come from, and a regression here is silent.
new_world
publish amd64
run fetch x86_64 --cni calico
assert_eq "default urls -> exits 0" "$?" "0"
assert_contains "default urls -> anchor from github, tag percent-encoded" "$(cat "$calls")" \
  "https://github.com/rancher/rke2/releases/download/v1.34.9%2Brke2r1/sha256sum-amd64.txt"
assert_contains "default urls -> core archive from github" "$(cat "$calls")" \
  "https://github.com/rancher/rke2/releases/download/v1.34.9%2Brke2r1/rke2-images.linux-amd64.tar.zst"
assert_contains "default urls -> installer from get.rke2.io" "$(cat "$calls")" "https://get.rke2.io"
assert_not_contains "default urls -> nothing from the cn mirror" "$(cat "$calls")" "rancher-mirror.rancher.cn"
drop_world

# 20. --mirror cn: the same assets (same names, hence the same cache layout) come from
#     rancher-mirror.rancher.cn instead -- under the tag with '+' percent-encoded as %2D -- the
#     CNI extra included, and the installer from the mirror's own copy. Nothing at all may be
#     fetched from github.com or get.rke2.io, which are exactly the hosts a CN node cannot reach.
new_world
publish amd64
run fetch x86_64 --cni calico --mirror cn
assert_eq "cn mirror -> exits 0" "$?" "0"
assert_contains "cn mirror -> anchor from mirror, tag '+'->'%2D'" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/rke2/releases/download/v1.34.9%2Drke2r1/sha256sum-amd64.txt"
assert_contains "cn mirror -> core archive from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/rke2/releases/download/v1.34.9%2Drke2r1/rke2-images.linux-amd64.tar.zst"
assert_contains "cn mirror -> cni extra from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/rke2/releases/download/v1.34.9%2Drke2r1/rke2-images-calico.linux-amd64.tar.zst"
assert_contains "cn mirror -> binary tarball from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/rke2/releases/download/v1.34.9%2Drke2r1/rke2.linux-amd64.tar.gz"
assert_contains "cn mirror -> installer from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/rke2/install.sh"
assert_not_contains "cn mirror -> nothing from github" "$(cat "$calls")" "github.com"
assert_not_contains "cn mirror -> nothing from get.rke2.io" "$(cat "$calls")" "get.rke2.io"
assert_dir_holds "cn mirror -> cached under the same names" "$cache" \
  "install.sh rke2-images-calico.linux-amd64.tar.zst rke2-images.linux-amd64.tar.zst rke2.linux-amd64.tar.gz sha256sum-amd64.txt"

# 21. The artifacts and stage phases run off the mirror-filled cache unchanged, and a warm cache
#     downloads nothing in cn mode either -- the mirror's checksums are the same bytes as
#     github's, so a cache warmed in one mode verifies cleanly in the other.
run artifacts x86_64 --cni calico --staging-dir "$staging" --mirror cn
assert_eq "cn artifacts -> exits 0" "$?" "0"
assert_dir_holds "cn artifacts -> the installer gets the same allowlist" "$staging" \
  "rke2-images-calico.linux-amd64.tar.zst rke2-images.linux-amd64.tar.zst rke2.linux-amd64.tar.gz sha256sum-amd64.txt"
run stage x86_64 --mirror cn
assert_eq "cn stage -> exits 0" "$?" "0"
assert_dir_holds "cn stage -> archives staged" "$images" \
  "rke2-images-calico.linux-amd64.tar.zst rke2-images.linux-amd64.tar.zst"
: >"$calls"
run fetch x86_64 --cni calico --mirror cn
assert_eq "cn warm cache -> exits 0" "$?" "0"
assert_eq "cn warm cache -> zero downloads" "$(wc -l <"$calls" | tr -d ' ')" "0"
drop_world

# 22. An unknown mirror is refused up front, before anything is created or fetched.
scratch="$(mktemp -d)"
if out="$(bash "$script" fetch --release "$release" --cache-dir "$scratch" --mirror mars 2>&1)"; then
  no "unknown --mirror -> refused" "exited 0"
else
  assert_contains "unknown --mirror -> refused, naming the value" "$out" "'mars'"
fi
assert_dir_holds "unknown --mirror -> creates nothing" "$scratch" ""
if out="$(bash "$script" fetch --release "$release" --cache-dir "$scratch" --mirror 2>&1)"; then
  no "--mirror with no value -> refused" "exited 0"
else
  assert_contains "--mirror with no value -> says which option" "$out" "--mirror needs a value"
fi
rm -rf "$scratch"

if [ "$fail" -ne 0 ]; then
  echo "one or more assertions failed"
  exit 1
fi
echo "all assertions passed"
