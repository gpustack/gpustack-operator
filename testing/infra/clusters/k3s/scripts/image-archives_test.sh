#!/usr/bin/env bash
# Unit test for image-archives.sh: run directly, e.g.
#   bash testing/infra/clusters/k3s/scripts/image-archives_test.sh
#
# Entirely offline. curl and uname are stubbed on PATH: curl serves a fake release directory
# and records every URL it is asked for, so "a warm cache downloads nothing" is asserted by
# counting calls rather than by reading a log line. K3S_AGENT_IMAGES_DIR and GPUSTACK_K3S_BIN_DIR
# point the staging step and the binary install at scratch directories, so the test needs no root.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$script_dir/image-archives.sh"
release="v1.34.9+k3s1"
fail=0

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# --- fixture ----------------------------------------------------------------

# A scratch world per case: a fake release server, an empty cache root, an empty images dir,
# and stub curl/uname on PATH. Sets $world, $serve, $cache_root, $cache, $images, $calls.
new_world() {
  world="$(mktemp -d)"
  serve="$world/release"
  cache_root="$world/cache"
  cache="$cache_root/$release"
  images="$world/images"
  bindir="$world/sbin"
  calls="$world/curl.log"
  mkdir -p "$serve" "$cache_root" "$images" "$bindir" "$world/bin"
  : >"$calls"

  # The installer is fetched from https://get.k3s.io -- or, in cn mirror mode, from
  # https://rancher-mirror.rancher.cn/k3s/k3s-install.sh -- whose basename is what the stub
  # serves by. It carries the marker the script shape-checks for, since a 200 with the wrong
  # body is the failure that matters here.
  printf '#!/bin/sh\n# INSTALL_K3S_SKIP_DOWNLOAD\n' >"$serve/get.k3s.io"
  printf '#!/bin/sh\n# INSTALL_K3S_SKIP_DOWNLOAD\n' >"$serve/k3s-install.sh"

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
}

# Publishes files into the fake release and (re)writes the checksum file to match them.
# Every argument is "<name>:<content>".
publish() {
  local arch="$1" entry name bin
  shift
  # Every release publishes its own binary under an arch-specific name, and the script needs it.
  # Provided here rather than in each case, which are about the image archives; a case that is
  # about the binary overwrites or removes it afterwards.
  bin="$(bin_asset_for "$arch")"
  [ -f "$serve/$bin" ] || printf 'k3s-binary-%s' "$arch" >"$serve/$bin"
  for entry in "$@"; do
    name="${entry%%:*}"
    printf '%s' "${entry#*:}" >"$serve/$name"
  done
  (cd "$serve" && for f in k3s-airgap-images-* gpustack-* "$bin"; do
    [ -f "$f" ] && printf '%s  %s\n' "$(sha256_of "$f")" "$f"
  done) >"$serve/sha256sum-${arch}.txt" 2>/dev/null
}

# The binary's asset name per arch, mirroring the script: amd64 carries no suffix and 32-bit arm
# is spelled armhf, so it cannot be derived from the archive names.
bin_asset_for() {
  case "$1" in
  amd64) echo k3s ;;
  arm64) echo k3s-arm64 ;;
  arm) echo k3s-armhf ;;
  *) echo "unknown-arch-$1" ;;
  esac
}

run() {
  local machine="${1:-x86_64}"
  shift 2>/dev/null || true
  env PATH="$world/bin:$PATH" \
    CURL_LOG="$calls" SERVE_DIR="$serve" FAKE_MACHINE="$machine" \
    K3S_AGENT_IMAGES_DIR="$images" GPUSTACK_K3S_BIN_DIR="$bindir" \
    bash "$script" --release "$release" --cache-dir "$cache_root" "$@" >"$world/out" 2>"$world/err"
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
# Directory contents are asserted through the directory itself, not through `ls` output: a
# failing `ls` yields an empty string, which would make "nothing was staged" pass for the wrong
# reason -- the exact shape of false pass this suite exists to prevent.
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
assert_contains() {
  if printf '%s' "$2" | grep -qF -- "$3"; then ok "$1"; else no "$1" "[$3] not found in: $2"; fi
}
assert_not_contains() {
  if printf '%s' "$2" | grep -qF -- "$3"; then no "$1" "[$3] unexpectedly found in: $2"; else ok "$1"; fi
}
# Takes the machine type, runs, and expects a non-zero exit -- a refusal is as much of a
# contract here as a success, since the alternative is a cluster that silently pulls everything.
assert_run_fails() {
  local name="$1" machine="$2"
  if run "$machine"; then no "$name" "exited 0; stdout: $(cat "$world/out")"; else ok "$name"; fi
}
# The call log holds full URLs; asset-level assertions still count by basename.
calls_for() { awk -F/ '{print $NF}' "$calls" | grep -cxF "$1"; }
# BSD and GNU stat disagree on the flag for a file's mode, and this suite runs on both.
mode_of() {
  if stat -f '%Lp' "$1" 2>/dev/null; then return 0; fi
  stat -c '%a' "$1"
}

# --- cases ------------------------------------------------------------------

# 1. Cold cache: the checksum file comes first (it is the trust anchor), the archive is
#    downloaded, verified under a name nothing stages, renamed, then staged.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:zst-images" "k3s-airgap-images-amd64.tar.gz:gz-images"
run x86_64
assert_eq "cold cache -> exits 0" "$?" "0"
assert_file "cold cache -> checksum file cached" "$cache/sha256sum-amd64.txt"
assert_file "cold cache -> archive cached" "$cache/k3s-airgap-images-amd64.tar.zst"
assert_no_file "cold cache -> no partial left behind" "$cache/partial.k3s-airgap-images-amd64.tar.zst"
assert_file "cold cache -> archive staged" "$images/k3s-airgap-images-amd64.tar.zst"
assert_dir_holds "cold cache -> zst preferred over gz, and nothing else staged" "$images" "k3s-airgap-images-amd64.tar.zst"
assert_eq "cold cache -> archive downloaded once" "$(calls_for k3s-airgap-images-amd64.tar.zst)" "1"

# 2. Warm cache: the same world, run again. Nothing is fetched at all -- asserted by the call
#    log, since this is the whole point of the feature.
: >"$calls"
run x86_64
assert_eq "warm cache -> exits 0" "$?" "0"
assert_eq "warm cache -> zero downloads" "$(wc -l <"$calls" | tr -d ' ')" "0"
assert_contains "warm cache -> reports the archive already present" "$(cat "$world/out")" "1 already present"
drop_world

# 3. A truncated cached archive self-heals in exactly one re-download.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:the-real-thing"
mkdir -p "$cache"
cp "$serve/sha256sum-amd64.txt" "$cache/"
printf 'the-real' >"$cache/k3s-airgap-images-amd64.tar.zst" # interrupted mid-write
run x86_64
assert_eq "truncated archive -> exits 0" "$?" "0"
assert_eq "truncated archive -> re-downloaded exactly once" "$(calls_for k3s-airgap-images-amd64.tar.zst)" "1"
assert_eq "truncated archive -> cache now holds the real content" "$(cat "$cache/k3s-airgap-images-amd64.tar.zst")" "the-real-thing"
assert_eq "truncated archive -> staged copy is the real content" "$(cat "$images/k3s-airgap-images-amd64.tar.zst")" "the-real-thing"
drop_world

# 4. A stale checksum file is refreshed BEFORE the artifact is blamed. The archive on disk is
#    what the release now publishes; only the cached anchor is out of date. Re-downloading
#    against a stale anchor would mismatch forever, so the anchor is refreshed first and the
#    archive is then accepted untouched.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:republished-images"
mkdir -p "$cache"
cp "$serve/k3s-airgap-images-amd64.tar.zst" "$cache/"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "k3s-airgap-images-amd64.tar.zst" >"$cache/sha256sum-amd64.txt"
run x86_64
assert_eq "stale checksum -> exits 0" "$?" "0"
assert_eq "stale checksum -> anchor refreshed once" "$(calls_for sha256sum-amd64.txt)" "1"
assert_eq "stale checksum -> archive NOT re-downloaded" "$(calls_for k3s-airgap-images-amd64.tar.zst)" "0"
assert_contains "stale checksum -> says why it refreshed" "$(cat "$world/out")" "refreshing sha256sum-amd64.txt"
drop_world

# 4a. The same stale anchor, but with nothing cached to have triggered the refresh: an operator
#     who copied another release's checksum file into this version's directory. The archive that
#     downloads is the right one, so the anchor -- not the download -- is what must be refreshed,
#     and one refresh plus one download has to be enough. Blaming the bytes here would leave a
#     first run permanently unable to warm the cache.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:current-images"
mkdir -p "$cache"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "k3s-airgap-images-amd64.tar.zst" >"$cache/sha256sum-amd64.txt"
run x86_64
assert_eq "stale anchor, empty cache -> exits 0" "$?" "0"
assert_eq "stale anchor, empty cache -> anchor refreshed once" "$(calls_for sha256sum-amd64.txt)" "1"
assert_eq "stale anchor, empty cache -> archive downloaded once" "$(calls_for k3s-airgap-images-amd64.tar.zst)" "1"
assert_eq "stale anchor, empty cache -> cache holds the real content" "$(cat "$cache/k3s-airgap-images-amd64.tar.zst")" "current-images"
assert_dir_holds "stale anchor, empty cache -> staged" "$images" "k3s-airgap-images-amd64.tar.zst"
drop_world

# 4b. A download that succeeds having written nothing is the one case the checksum file cannot
#     catch for itself, since it IS the anchor. It must not reach the final name: everything
#     downstream would then be judged against an empty list of digests.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
: >"$serve/sha256sum-amd64.txt" # the server answers 200 with an empty body
assert_run_fails "empty checksum response -> fails" x86_64
assert_contains "empty checksum response -> says what it did not get" "$(cat "$world/err")" "did not return a sha256 checksum list"
assert_no_file "empty checksum response -> anchor never promoted" "$cache/sha256sum-amd64.txt"
assert_dir_holds "empty checksum response -> nothing staged" "$images" ""
drop_world

# 5. A cache holding only the checksum file, on a node that cannot reach the release assets,
#    fails loudly naming the file and the cache -- it never proceeds to report success with an
#    empty images directory.
new_world
mkdir -p "$cache"
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
cp "$serve/sha256sum-amd64.txt" "$cache/"
rm -f "$serve/k3s-airgap-images-amd64.tar.zst" # the assets are unreachable now
assert_run_fails "half-warm cache offline -> fails" x86_64
assert_contains "half-warm cache offline -> names the cache" "$(cat "$world/err")" "$cache"
assert_dir_holds "half-warm cache offline -> nothing staged" "$images" ""
drop_world

# 6. An operator's own bundle, absent from the checksum file, is staged and logged as
#    explicitly unverified. Demanding a digest would break the extra-bundle path; calling it
#    verified would be a lie.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
mkdir -p "$cache"
cp "$serve/sha256sum-amd64.txt" "$serve/k3s-airgap-images-amd64.tar.zst" "$cache/"
printf 'our-own-images' >"$cache/gpustack-images.tar.zst"
run x86_64
assert_eq "operator extra -> exits 0" "$?" "0"
assert_file "operator extra -> staged" "$images/gpustack-images.tar.zst"
assert_contains "operator extra -> logged unverified" "$(cat "$world/out")" "gpustack-images.tar.zst -- unverified"
assert_contains "operator extra -> published archive still logged verified" "$(cat "$world/out")" "k3s-airgap-images-amd64.tar.zst -- verified"
drop_world

# 7. A leftover partial from an interrupted run is neither staged nor mistaken for an
#    artifact. This is why downloads are named partial.<name> and not <name>.partial: the
#    latter would be swept up by any *.tar* glob.
# 8. A .txt in the cache is never staged: there it is metadata, but in the images directory it
#    means "pull every image named in me".
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
mkdir -p "$cache"
cp "$serve/sha256sum-amd64.txt" "$serve/k3s-airgap-images-amd64.tar.zst" "$cache/"
printf 'half-a-download' >"$cache/partial.k3s-airgap-images-amd64.tar.gz"
printf 'docker.io/library/busybox:latest\n' >"$cache/k3s-images.txt"
run x86_64
assert_eq "leftover partial -> exits 0" "$?" "0"
assert_no_file "leftover partial -> not staged" "$images/partial.k3s-airgap-images-amd64.tar.gz"
assert_no_file "leftover partial -> not renamed into place" "$cache/k3s-airgap-images-amd64.tar.gz"
assert_no_file "cache .txt -> never staged" "$images/k3s-images.txt"
assert_no_file "cache checksum file -> never staged" "$images/sha256sum-amd64.txt"
drop_world

# 9. .tar.zst absent from the checksum file: the suffix is chosen from what the release
#    actually publishes, not hardcoded.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.gz:gz-only-release"
run x86_64
assert_eq "zst absent -> exits 0" "$?" "0"
assert_file "zst absent -> falls back to .tar.gz" "$cache/k3s-airgap-images-amd64.tar.gz"
assert_dir_holds "zst absent -> staged the gz" "$images" "k3s-airgap-images-amd64.tar.gz"
drop_world

# 10. Both spellings of every machine type k3s publishes for, and an unknown one. uname
#     reports x86_64/aarch64 on most distributions and amd64/arm64 on some, and guessing at an
#     unknown value would download a 404 page and cache it.
for pair in "x86_64:amd64" "amd64:amd64" "aarch64:arm64" "arm64:arm64" "armv7l:arm" "arm:arm"; do
  machine="${pair%%:*}"
  arch="${pair#*:}"
  new_world
  publish "$arch" "k3s-airgap-images-${arch}.tar.zst:images-${arch}"
  run "$machine"
  assert_dir_holds "uname -m '$machine' -> ${arch} archive" "$images" "k3s-airgap-images-${arch}.tar.zst"
  drop_world
done

new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
assert_run_fails "unknown machine type -> fails" s390x
assert_contains "unknown machine type -> names the value" "$(cat "$world/err")" "'s390x'"
assert_eq "unknown machine type -> downloads nothing" "$(wc -l <"$calls" | tr -d ' ')" "0"
drop_world

# 11. A relative cache directory is refused rather than resolved against whatever the
#     provisioner's working directory happens to be on the node.
world="$(mktemp -d)"
if out="$(bash "$script" --release "$release" --cache-dir relative/path 2>&1)"; then
  no "relative --cache-dir -> refused" "exited 0"
else
  assert_contains "relative --cache-dir -> refused" "$out" "must be absolute"
fi
rm -rf "$world"

# 12. A cache reached through a SYMLINK into k3s' data directory is refused -- and refused on the
#     FIRST run, when the release directory does not exist yet. That is the case a guard gated on
#     "the path already exists" skipped entirely, leaving a cache the uninstall deletes before it can
#     ever be warm. Two shapes: the symlink at the cache root, and at the release directory itself.
for shape in root release; do
  new_world
  publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
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
    K3S_AGENT_IMAGES_DIR="$images" GPUSTACK_K3S_DATA_DIR="$datadir" \
    bash "$script" --release "$release" --cache-dir "$given" 2>&1)" &&
    no "symlinked cache ($shape) -> refused" "exited 0" ||
    assert_contains "symlinked cache ($shape) -> refused, naming the resolved path" "$out" "$datadir"
  assert_eq "symlinked cache ($shape) -> downloads nothing" "$(wc -l <"$calls" | tr -d ' ')" "0"
  drop_world
done

# 13. An option given without a value says so, instead of dying inside `shift 2`.
world="$(mktemp -d)"
if out="$(bash "$script" --release 2>&1)"; then
  no "--release with no value -> refused" "exited 0"
else
  assert_contains "--release with no value -> says which option" "$out" "--release needs a value"
fi
if out="$(bash "$script" --release "$release" --cache-dir 2>&1)"; then
  no "--cache-dir with no value -> refused" "exited 0"
else
  assert_contains "--cache-dir with no value -> says which option" "$out" "--cache-dir needs a value"
fi
rm -rf "$world"

# 14. The binary and the installer: both land in the cache, the binary is installed executable
#     where the installer looks for one, and neither is staged into the images directory -- there
#     they would be handed to k3s as an image archive.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
run x86_64
assert_eq "binary+installer -> exits 0" "$?" "0"
assert_file "binary -> cached" "$cache/k3s"
assert_file "installer -> cached" "$cache/install.sh"
assert_file "binary -> installed" "$bindir/k3s"
assert_eq "binary -> installed 0755" "$(mode_of "$bindir/k3s")" "755"
assert_eq "binary -> installed content is the cached content" "$(cat "$bindir/k3s")" "$(cat "$cache/k3s")"
assert_no_file "binary -> not staged as an image archive" "$images/k3s"
assert_no_file "installer -> not staged as an image archive" "$images/install.sh"
assert_no_file "binary -> no staging temp left behind" "$bindir/.k3s.staged"
assert_contains "binary -> says where it installed" "$(cat "$world/out")" "installed $bindir/k3s from k3s"

# 15. Warm cache, and the binary gone from the bin directory -- which is the real shape of every
#     re-apply, since the reclaim's uninstall removes it. It has to be put back with no download.
rm -f "$bindir/k3s"
: >"$calls"
run x86_64
assert_eq "warm, binary removed -> exits 0" "$?" "0"
assert_eq "warm, binary removed -> zero downloads" "$(wc -l <"$calls" | tr -d ' ')" "0"
assert_file "warm, binary removed -> reinstalled from cache" "$bindir/k3s"
drop_world

# 16. A corrupt cached binary self-heals in exactly one re-download, and what reaches the bin
#     directory is the real content -- not the corrupt copy that was already there.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
mkdir -p "$cache"
cp "$serve/sha256sum-amd64.txt" "$serve/k3s-airgap-images-amd64.tar.zst" "$serve/get.k3s.io" "$cache/"
mv "$cache/get.k3s.io" "$cache/install.sh"
printf 'truncated' >"$cache/k3s"
run x86_64
assert_eq "corrupt binary -> exits 0" "$?" "0"
assert_eq "corrupt binary -> re-downloaded exactly once" "$(calls_for k3s)" "1"
assert_eq "corrupt binary -> cache holds the real content" "$(cat "$cache/k3s")" "k3s-binary-amd64"
assert_eq "corrupt binary -> installed the real content" "$(cat "$bindir/k3s")" "k3s-binary-amd64"
drop_world

# 17. https://get.k3s.io answering 200 with something that is not the installer must not reach the
#     final name: the next run would "find" it and hand it to sh as the installer.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
printf '<html>proxy login page</html>' >"$serve/get.k3s.io"
assert_run_fails "wrong installer body -> fails" x86_64
assert_contains "wrong installer body -> says what it did not get" "$(cat "$world/err")" "did not return the k3s installer"
assert_no_file "wrong installer body -> never promoted" "$cache/install.sh"
drop_world

# 18. A release that lists no binary for this architecture fails naming it, rather than
#     downloading a 404 page and installing it as k3s.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
# Removed from the release AND from the anchor by hand: calling publish() again would put the
# binary straight back, since it provides one for every case that does not care.
rm -f "$serve/k3s"
grep -v ' k3s$' "$serve/sha256sum-amd64.txt" >"$serve/anchor" && mv "$serve/anchor" "$serve/sha256sum-amd64.txt"
assert_run_fails "binary absent from release -> fails" x86_64
assert_contains "binary absent from release -> names the asset" "$(cat "$world/err")" "lists no k3s in"
assert_no_file "binary absent from release -> nothing installed" "$bindir/k3s"
drop_world

# 19. Default mode downloads from the places it always has: github.com for the release assets
#     (tag percent-encoded) and get.k3s.io for the installer. Pinned explicitly because the cn
#     mode below changes where both come from, and a regression here is silent.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
run x86_64
assert_eq "default urls -> exits 0" "$?" "0"
assert_contains "default urls -> anchor from github, tag percent-encoded" "$(cat "$calls")" \
  "https://github.com/k3s-io/k3s/releases/download/v1.34.9%2Bk3s1/sha256sum-amd64.txt"
assert_contains "default urls -> archive from github" "$(cat "$calls")" \
  "https://github.com/k3s-io/k3s/releases/download/v1.34.9%2Bk3s1/k3s-airgap-images-amd64.tar.zst"
assert_contains "default urls -> installer from get.k3s.io" "$(cat "$calls")" "https://get.k3s.io"
assert_not_contains "default urls -> nothing from the cn mirror" "$(cat "$calls")" "rancher-mirror.rancher.cn"
drop_world

# 20. --mirror cn: the same assets (same names, hence the same cache layout) come from
#     rancher-mirror.rancher.cn instead -- under the tag with '+' spelled '-', and the installer
#     from the mirror's own copy. Nothing at all may be fetched from github.com or get.k3s.io,
#     which are exactly the hosts a CN node cannot reach.
new_world
publish amd64 "k3s-airgap-images-amd64.tar.zst:images"
run x86_64 --mirror cn
assert_eq "cn mirror -> exits 0" "$?" "0"
assert_contains "cn mirror -> anchor from mirror, tag '+'->'-'" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/k3s/v1.34.9-k3s1/sha256sum-amd64.txt"
assert_contains "cn mirror -> archive from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/k3s/v1.34.9-k3s1/k3s-airgap-images-amd64.tar.zst"
assert_contains "cn mirror -> binary from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/k3s/v1.34.9-k3s1/k3s"
assert_contains "cn mirror -> installer from mirror" "$(cat "$calls")" \
  "https://rancher-mirror.rancher.cn/k3s/k3s-install.sh"
assert_not_contains "cn mirror -> nothing from github" "$(cat "$calls")" "github.com"
assert_not_contains "cn mirror -> nothing from get.k3s.io" "$(cat "$calls")" "get.k3s.io"
assert_file "cn mirror -> archive cached under the same name" "$cache/k3s-airgap-images-amd64.tar.zst"

# 21. A warm cache downloads nothing in cn mode either -- the mirror's checksums are the same
#     bytes as github's, so a cache warmed in one mode verifies cleanly in the other.
: >"$calls"
run x86_64 --mirror cn
assert_eq "cn warm cache -> exits 0" "$?" "0"
assert_eq "cn warm cache -> zero downloads" "$(wc -l <"$calls" | tr -d ' ')" "0"
drop_world

# 22. An unknown mirror is refused up front, before anything is created or fetched.
world="$(mktemp -d)"
if out="$(bash "$script" --release "$release" --cache-dir /tmp/cache --mirror mars 2>&1)"; then
  no "unknown --mirror -> refused" "exited 0"
else
  assert_contains "unknown --mirror -> refused, naming the value" "$out" "'mars'"
fi
if out="$(bash "$script" --release "$release" --cache-dir /tmp/cache --mirror 2>&1)"; then
  no "--mirror with no value -> refused" "exited 0"
else
  assert_contains "--mirror with no value -> says which option" "$out" "--mirror needs a value"
fi
rm -rf "$world"

if [ "$fail" -ne 0 ]; then
  echo "one or more assertions failed"
  exit 1
fi
echo "all assertions passed"
