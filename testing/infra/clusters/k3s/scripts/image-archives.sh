#!/usr/bin/env bash
# Keeps a release-partitioned airgap-image cache on a k3s node and stages it where k3s
# imports images at startup. Runs ON the node, as root -- it writes under /var/lib and never
# invokes sudo itself, so the caller decides. Run once per node, after the reclaim step and
# before the installer.
#
#   image-archives.sh --release <tag> --cache-dir <dir> [--mirror cn]
#
# With --mirror cn every download comes from rancher-mirror.rancher.cn instead: the release
# assets from the mirror's copy of the release directory (same asset names, byte-identical
# checksum files), and the installer from the mirror's own copy. This is for nodes that cannot
# reach github.com or get.k3s.io. The installer's own INSTALL_K3S_MIRROR parameter is
# deliberately not used: the upstream and CN-hosted install.sh variants differ, and a node may
# already hold a cached script of either variant, so mirror downloads are done here instead.
#
# The cache is <cache-dir>/<release>: one directory per release tag, which is what makes a
# version-skewed install unreachable rather than merely detectable -- what gets staged for a
# release can only come from that release's own directory, and what gets downloaded comes from
# that release's own published assets. It is find-or-fetch, so a warm cache downloads nothing
# and a node with the files pre-placed (the checksum file included) needs no network at all.
# It is never pruned and never rewritten -- only added to.
#
# k3s imports every archive under /var/lib/rancher/k3s/agent/images/ at startup, which is what
# this stages into. The cache also holds the release's own k3s binary and a pinned copy of the
# installer, and this script puts the binary in place -- so the install that follows can run with
# INSTALL_K3S_SKIP_DOWNLOAD=true and reach the network for nothing at all. The binary is verified
# against the same published checksum file as the archives, which is what makes --release
# authoritative for it too.
set -euo pipefail

PROG="$(basename "$0")"
NODE="$(hostname 2>/dev/null || echo unknown-node)"
readonly PROG NODE
# Where k3s imports images from. Overridable so the test suite can run against a scratch
# directory without root; the module never sets it.
readonly IMAGES_DIR="${K3S_AGENT_IMAGES_DIR:-/var/lib/rancher/k3s/agent/images}"
# The tree the uninstall removes wholesale. Deliberately NOT k3s' own K3S_DATA_DIR, which is a real
# k3s variable the installer honours: `sudo` strips it before this script sees it, so reading it here
# would look like it tracked a relocated data directory while silently guarding the default one. A
# relocated data directory is out of this module's scope -- the images directory is fixed too. This
# name exists only so the test suite can exercise the guard below; the module never sets it.
readonly DATA_DIR="${GPUSTACK_K3S_DATA_DIR:-/var/lib/rancher/k3s}"
# Where the k3s binary is installed, matching the installer's own INSTALL_K3S_BIN_DIR default.
# Overridable for the same reason as the two above: so the test suite can run without root.
readonly BIN_DIR="${GPUSTACK_K3S_BIN_DIR:-/usr/local/bin}"

log() { echo "[$PROG] $*"; }
die() {
  echo "[$PROG] ${NODE}: $*" >&2
  exit 1
}

release=""
cache_root=""
mirror=""
while [ "$#" -gt 0 ]; do
  case "$1" in
  --release)
    release="${2:-}"
    [ -n "$release" ] || die "--release needs a value"
    shift 2
    ;;
  --cache-dir)
    cache_root="${2:-}"
    [ -n "$cache_root" ] || die "--cache-dir needs a value"
    shift 2
    ;;
  --mirror)
    mirror="${2:-}"
    [ -n "$mirror" ] || die "--mirror needs a value"
    shift 2
    ;;
  *)
    echo "usage: $PROG --release <tag> --cache-dir <dir> [--mirror cn]" >&2
    exit 2
    ;;
  esac
done
[ -n "$release" ] || die "--release is required"
[ -n "$cache_root" ] || die "--cache-dir is required"
case "$cache_root" in
/*) ;;
*) die "--cache-dir must be absolute, got '${cache_root}'" ;;
esac
case "$mirror" in
"" | cn) ;;
*) die "unsupported --mirror '${mirror}'; the only mirror this script knows is 'cn'" ;;
esac

readonly cache="${cache_root%/}/${release}"

# Terraform can only string-check this path; the node is where the truth is, and a symlink is
# invisible from there. A cache that resolves inside k3s' data directory is removed by the
# uninstall that runs before every install, so it would never be warm and every apply would
# download again -- the feature reporting success while doing nothing.
#
# Created first, then checked. Gating the check on the path already existing skipped it on exactly
# the first run -- and a symlinked PARENT whose leaf does not exist yet is the case that then slipped
# through. The release directory is what is resolved, because a symlink at either level puts the
# cache in the same place.
mkdir -p "$cache" || die "cannot create ${cache}"
resolved="$(cd "$cache" && pwd -P)" || die "cannot resolve ${cache}"
# Both sides are resolved before they are compared. On a host where /var is itself a symlink,
# a fully resolved cache path never matches an unresolved data directory, and the guard would
# pass on exactly the layout it exists to catch.
data_dir_resolved="$DATA_DIR"
if [ -d "$DATA_DIR" ]; then
  data_dir_resolved="$(cd "$DATA_DIR" && pwd -P)" || data_dir_resolved="$DATA_DIR"
fi
case "$resolved" in
"$data_dir_resolved" | "$data_dir_resolved"/*)
  die "the cache directory resolves to ${resolved}, inside k3s' data directory (${data_dir_resolved}); the uninstall run before every install removes that tree"
  ;;
esac

# sha256sum is universal on the Linux nodes this runs on; the shasum fallback is what lets the
# test suite run on a maintainer's workstation.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

size_of() { echo $(($(wc -c <"$1"))); }

# k3s publishes airgap images for amd64, arm64 and (32-bit) arm, and uname reports the same
# machine under either spelling depending on the distribution. The binary's asset name does not
# follow the image archives': amd64's carries no suffix at all and 32-bit arm is spelled armhf,
# so the two are resolved side by side rather than derived from one another.
case "$(uname -m)" in
x86_64 | amd64) readonly arch=amd64 bin_asset=k3s ;;
aarch64 | arm64) readonly arch=arm64 bin_asset=k3s-arm64 ;;
armv7l | arm) readonly arch=arm bin_asset=k3s-armhf ;;
*) die "unsupported machine type '$(uname -m)'; k3s publishes airgap images for amd64, arm64 and arm" ;;
esac

readonly checksums="sha256sum-${arch}.txt"
readonly installer="install.sh"
# GitHub download paths take the tag percent-encoded; the '+' in every k3s tag is otherwise
# read as a space and the asset is not found. The cn mirror serves the same assets -- same
# names, byte-identical checksum files -- under the tag with '+' spelled '-' instead, and hosts
# its own copy of the installer next to them.
if [ "$mirror" = cn ]; then
  readonly base_url="https://rancher-mirror.rancher.cn/k3s/${release//+/-}"
  readonly installer_url="https://rancher-mirror.rancher.cn/k3s/k3s-install.sh"
else
  readonly base_url="https://github.com/k3s-io/k3s/releases/download/${release//+/%2B}"
  readonly installer_url="https://get.k3s.io"
fi

download() {
  curl -fL --retry 3 --retry-delay 2 -sS -o "$2" "$1" ||
    die "cannot download $1 (a node without access to the release assets needs the files pre-placed in ${cache})"
}

fetch_checksums() {
  local part="${cache}/partial.${checksums}"
  rm -f "$part"
  download "${base_url}/${checksums}" "$part"
  # This is the one file with no digest to check it against, so it is checked for shape instead:
  # a download that succeeds having written nothing, or an error page, must not be promoted to
  # the name every other artifact is then judged by.
  grep -Eq '^[0-9a-f]{64}[[:space:]]' "$part" ||
    die "${base_url}/${checksums} did not return a sha256 checksum list"
  mv -f "$part" "${cache}/${checksums}"
}

# The checksum file is the trust anchor for everything else, so it is fetched first and
# refreshed -- once per run -- whenever a cached artifact disagrees with it. Re-downloading an
# artifact against a stale anchor would mismatch forever, with no recovery path.
checksums_refreshed=no
refresh_checksums() {
  [ "$checksums_refreshed" = no ] || return 0
  checksums_refreshed=yes
  log "refreshing ${checksums}: an artifact disagrees with it"
  fetch_checksums
}

# The digest the release publishes for a name, or empty when the name is not listed -- which is
# how an operator's own bundle is told apart from a published artifact.
checksum_lookup() {
  awk -v want="$1" '$2 == want { print $1; exit }' "${cache}/${checksums}" 2>/dev/null || true
}

# Bring one published asset into the cache, verified. The order is the correctness argument: a
# download is verified while it still carries a name nothing reads, and only then takes its real
# one. Renaming first would leave a truncated file occupying the final name, and every later run
# would "find" it and use a corrupt artifact forever.
ensure_cached() {
  local name="$1" expected actual part
  expected="$(checksum_lookup "$name")"
  if [ -z "$expected" ]; then
    # Not listed: the anchor itself may be truncated, or from another release. Refresh it once
    # before concluding the release has no such asset.
    refresh_checksums
    expected="$(checksum_lookup "$name")"
    [ -n "$expected" ] || die "release ${release} lists no ${name} in ${checksums}"
  fi
  if [ -f "${cache}/${name}" ]; then
    actual="$(sha256_of "${cache}/${name}")"
    if [ "$actual" = "$expected" ]; then
      log "cached ${name} (verified)"
      return 0
    fi
    refresh_checksums
    expected="$(checksum_lookup "$name")"
    # A refreshed anchor that no longer lists this name means the release dropped the asset.
    # Without this the re-download below would compare against an empty digest and report a
    # mismatch against nothing.
    [ -n "$expected" ] || die "release ${release} no longer lists ${name} in ${checksums}"
    if [ "$actual" = "$expected" ]; then
      log "cached ${name} (verified against the refreshed ${checksums})"
      return 0
    fi
    log "re-downloading ${name}: cached copy is ${actual}, release publishes ${expected}"
    rm -f "${cache}/${name}"
  fi
  part="${cache}/partial.${name}"
  rm -f "$part"
  log "downloading ${name}"
  download "${base_url}/${name}" "$part"
  actual="$(sha256_of "$part")"
  if [ "$actual" != "$expected" ] && [ "$checksums_refreshed" = no ]; then
    # The anchor, not the download, may be what is out of date -- an operator who copied another
    # release's checksum file into this directory lands here on the very first run, with nothing
    # cached to have triggered the refresh above. Judge the same bytes again against a fresh
    # anchor before blaming them.
    refresh_checksums
    expected="$(checksum_lookup "$name")"
    [ -n "$expected" ] || die "release ${release} does not list ${name} in ${checksums}"
  fi
  if [ "$actual" != "$expected" ]; then
    rm -f "$part"
    die "sha256 mismatch for ${name}: expected ${expected}, got ${actual}"
  fi
  mv -f "$part" "${cache}/${name}"
  log "downloaded ${name} ($(size_of "${cache}/${name}") bytes, verified)"
}

# The installer script is not a release asset, so it is covered by no published digest. Caching it
# per release still pins it: an upstream edit cannot change what a re-apply installs.
ensure_installer() {
  local part="${cache}/partial.${installer}"
  if [ -s "${cache}/${installer}" ]; then
    log "cached ${installer} (pinned for ${release}; covered by no published digest)"
    return 0
  fi
  rm -f "$part"
  log "downloading ${installer} from ${installer_url}"
  download "$installer_url" "$part"
  grep -q 'INSTALL_K3S_SKIP_DOWNLOAD' "$part" ||
    die "${installer_url} did not return the k3s installer"
  mv -f "$part" "${cache}/${installer}"
  log "downloaded ${installer} (covered by no published digest, so trusted only as far as TLS)"
}

# Put the cached binary where the installer expects to find one, so it can be run with
# INSTALL_K3S_SKIP_DOWNLOAD=true. Written under a temporary name and renamed: a rename over a
# running binary is fine on Linux, while writing through the final name is ETXTBSY. The installer
# checks only that the result is executable -- the digest checked above is what makes it the right
# binary, and the module's post-install version assertion is what catches a hand-placed cache.
install_binary() {
  local staged="${BIN_DIR}/.k3s.staged"
  mkdir -p "$BIN_DIR"
  cp -f "${cache}/${bin_asset}" "$staged" || die "cannot write ${staged}"
  chmod 0755 "$staged" || die "cannot make ${staged} executable"
  mv -f "$staged" "${BIN_DIR}/k3s" || die "cannot install ${BIN_DIR}/k3s"
  log "installed ${BIN_DIR}/k3s from ${bin_asset}"
}

mkdir -p "$cache"
[ -s "${cache}/${checksums}" ] || fetch_checksums

# k3s publishes .tar, .tar.gz and .tar.zst of the same images. The checksum file decides which
# one to take, rather than a hardcoded suffix that would reject a release publishing only
# another; zst first because it is the smallest. The cn mirror serves only the .tar.gz -- its
# copy of the checksum anchor still lists all three -- so in cn mode the gz is preferred, and
# the rest stay as fallbacks for a mirror that one day carries them.
archive_suffixes=".tar.zst .tar.gz .tar"
[ "$mirror" = cn ] && archive_suffixes=".tar.gz .tar.zst .tar"
# A cached archive whose digest already matches the anchor wins over the mode's download
# preference: the mirror's checksums are the same bytes as github's, so a cache warmed in one
# mode serves the other without re-downloading the same images in the other compression.
archive=""
for suffix in .tar.zst .tar.gz .tar; do
  name="k3s-airgap-images-${arch}${suffix}"
  [ -f "${cache}/${name}" ] || continue
  expected="$(checksum_lookup "$name")"
  if [ -n "$expected" ] && [ "$(sha256_of "${cache}/${name}")" = "$expected" ]; then
    archive="$name"
    break
  fi
done
if [ -z "$archive" ]; then
  for suffix in $archive_suffixes; do
    if [ -n "$(checksum_lookup "k3s-airgap-images-${arch}${suffix}")" ]; then
      archive="k3s-airgap-images-${arch}${suffix}"
      break
    fi
  done
fi
if [ -z "$archive" ]; then
  # Neither listed: the anchor itself may be truncated, or from another release. Refresh it
  # once before concluding the release has no such asset.
  refresh_checksums
  for suffix in $archive_suffixes; do
    if [ -n "$(checksum_lookup "k3s-airgap-images-${arch}${suffix}")" ]; then
      archive="k3s-airgap-images-${arch}${suffix}"
      break
    fi
  done
fi
[ -n "$archive" ] || die "release ${release} lists no k3s-airgap-images-${arch} archive in ${checksums}"

ensure_cached "$archive"
# The binary and the installer, so the install that follows needs no network. Both are per-release
# and neither is staged into the images directory -- they are read from the cache in place.
ensure_cached "$bin_asset"
ensure_installer
install_binary

# Stage every archive in the cache, not just the one above, so an operator who drops an extra
# bundle (their own images) into the release directory gets it imported too, with no second
# variable. A copy, never a move: the copy under the data directory is removed by the next
# uninstall, and the cache has to survive that.
#
# *.txt is deliberately absent from these globs. In the cache a .txt is metadata (checksums,
# image lists); in k3s' images directory it means "pull every image named in me through the CRI
# API" -- the exact opposite of importing locally.
mkdir -p "$IMAGES_DIR"
staged=0
present=0
for path in "$cache"/*.tar "$cache"/*.tar.gz "$cache"/*.tar.zst; do
  [ -f "$path" ] || continue
  name="$(basename "$path")"
  case "$name" in
  partial.*)
    log "skipping ${name}: an interrupted download, not an artifact"
    continue
    ;;
  esac
  dest="${IMAGES_DIR}/${name}"
  if [ -f "$dest" ] && [ "$(size_of "$dest")" = "$(size_of "$path")" ]; then
    present=$((present + 1))
    continue
  fi
  expected="$(checksum_lookup "$name")"
  if [ -n "$expected" ]; then
    # A published name is not proof of the bytes: the selection above hashes only the archive it
    # picks, so a cached official compression whose digest failed is still sitting here -- and
    # staging a corrupt archive breaks the import at every k3s start. The anchor is fresh by now
    # (it is refreshed whenever an artifact disagrees with it), so a mismatch is the file's fault.
    [ "$(sha256_of "$path")" = "$expected" ] ||
      die "${name} does not match its published sha256 in ${checksums}: refusing to stage a corrupt archive"
    verified=verified
  else
    # An operator's own bundle has no published digest. Demanding one would break the
    # extra-bundle path; calling it verified would be a lie.
    verified="unverified (not published in ${checksums})"
  fi
  cp -f "$path" "$dest"
  staged=$((staged + 1))
  log "staged ${name} -- ${verified}"
done

# An images directory left empty is the one state in which this feature reports success while
# every image is still pulled from a registry. Asserted rather than assumed, because that
# failure is invisible: the cluster comes up, only slowly and only with a registry reachable.
[ "$((staged + present))" -gt 0 ] ||
  die "no image archive landed in ${IMAGES_DIR}: ${cache} holds none, so every image would be pulled from a registry"

log "${cache} ready for ${release} (${arch}); ${staged} archive(s) staged into ${IMAGES_DIR}, ${present} already present; ${BIN_DIR}/k3s and ${installer} in place, so the install needs no network"
