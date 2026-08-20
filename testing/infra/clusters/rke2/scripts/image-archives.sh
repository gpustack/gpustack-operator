#!/usr/bin/env bash
# Keeps a release-partitioned artifact cache on an RKE2 node, hands the installer an allowlisted
# copy of it, and stages any remaining image archives where RKE2 imports them. Runs ON the node,
# as root -- it writes under /var/lib and never invokes sudo itself, so the caller decides.
#
#   image-archives.sh fetch     --release <tag> --cache-dir <dir> [--cni <name>]
#   image-archives.sh artifacts --release <tag> --cache-dir <dir> --staging-dir <dir> [--cni <name>]
#   image-archives.sh stage     --release <tag> --cache-dir <dir>
#
# The cache is <cache-dir>/<release>: one directory per release tag, which is what makes a
# version-skewed install unreachable rather than merely detectable -- what the installer is given
# for a release can only come from that release's own directory, and what gets downloaded comes
# from that release's own published assets. It is find-or-fetch, so a warm cache downloads nothing
# and a node with the files pre-placed (the checksum file included) needs no network at all. It is
# never pruned and never rewritten -- only added to.
#
# Three phases rather than one, because the installer sits between them:
#
#   fetch     -- bring every wanted file into the cache, verified.
#   artifacts -- fill a directory this module owns with an ALLOWLIST from the cache, to be handed
#                to the installer as INSTALL_RKE2_ARTIFACT_PATH and removed afterwards. The
#                installer copies every regular file matching rke2-images-*.linux-<arch>* into the
#                images directory without looking at its extension, so it must never be pointed at
#                a directory an operator also writes to: a stray .txt there means "pull every
#                image named in me", which is the opposite of the intent.
#   stage     -- after the install, copy any remaining archives (an operator's own bundles) into
#                the images directory, which RKE2 reads at startup.
set -euo pipefail

PROG="$(basename "$0")"
NODE="$(hostname 2>/dev/null || echo unknown-node)"
readonly PROG NODE
# Where RKE2 imports images from. Overridable so the test suite can run against a scratch
# directory without root; the module never sets it.
readonly IMAGES_DIR="${RKE2_AGENT_IMAGES_DIR:-/var/lib/rancher/rke2/agent/images}"
# The tree the uninstall removes wholesale. Deliberately not named RKE2_DATA_DIR, which RKE2's own
# scripts use: `sudo` strips such a variable before this script sees it, so reading it here would look
# like it tracked a relocated data directory while silently guarding the default one. A relocated data
# directory is out of this module's scope -- the images directory is fixed too. This name exists only
# so the test suite can exercise the guard below; the module never sets it.
readonly DATA_DIR="${GPUSTACK_RKE2_DATA_DIR:-/var/lib/rancher/rke2}"

log() { echo "[$PROG] $*"; }
die() {
  echo "[$PROG] ${NODE}: $*" >&2
  exit 1
}

command="${1:-}"
shift || true

release=""
cache_root=""
staging_dir=""
cni=""
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
  --staging-dir)
    staging_dir="${2:-}"
    [ -n "$staging_dir" ] || die "--staging-dir needs a value"
    shift 2
    ;;
  --cni)
    cni="${2:-}"
    [ -n "$cni" ] || die "--cni needs a value"
    shift 2
    ;;
  *)
    echo "usage: $PROG <fetch|artifacts|stage> --release <tag> --cache-dir <dir> [--cni <name>] [--staging-dir <dir>]" >&2
    exit 2
    ;;
  esac
done
# The subcommand is checked here rather than only at the dispatch below, because everything between
# the two has side effects -- an invocation this script is going to refuse must create nothing.
case "$command" in
fetch | artifacts | stage) ;;
*)
  echo "usage: $PROG <fetch|artifacts|stage> --release <tag> --cache-dir <dir> [--cni <name>] [--staging-dir <dir>]" >&2
  exit 2
  ;;
esac
[ -n "$release" ] || die "--release is required"
[ -n "$cache_root" ] || die "--cache-dir is required"
case "$cache_root" in
/*) ;;
*) die "--cache-dir must be absolute, got '${cache_root}'" ;;
esac

readonly cache="${cache_root%/}/${release}"

# Terraform can only string-check this path; the node is where the truth is, and a symlink is
# invisible from there. A cache that resolves inside RKE2's data directory is removed by the
# uninstall that runs before every install, so it would never be warm and every apply would
# download again -- the feature reporting success while doing nothing.
#
# Created first, then checked. Gating the check on the path already existing skipped it on exactly
# the first run -- and a symlinked PARENT whose leaf does not exist yet is the case that then slipped
# through. The release directory is what is resolved, because a symlink at either level puts the
# cache in the same place.
mkdir -p "$cache" || die "cannot create ${cache}"
resolved="$(cd "$cache" && pwd -P)" || die "cannot resolve ${cache}"
# Both sides are resolved before they are compared. On a host where /var is itself a symlink, a fully
# resolved cache path never matches an unresolved data directory, and the guard would pass on exactly
# the layout it exists to catch.
data_dir_resolved="$DATA_DIR"
if [ -d "$DATA_DIR" ]; then
  data_dir_resolved="$(cd "$DATA_DIR" && pwd -P)" || data_dir_resolved="$DATA_DIR"
fi
case "$resolved" in
"$data_dir_resolved" | "$data_dir_resolved"/*)
  die "the cache directory resolves to ${resolved}, inside RKE2's data directory (${data_dir_resolved}); the uninstall run before every install removes that tree"
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

# RKE2 publishes amd64 and arm64 only, and uname reports the same machine under either spelling
# depending on the distribution. Guessing at anything else would download a 404 page and cache it.
case "$(uname -m)" in
x86_64 | amd64) readonly arch=amd64 ;;
aarch64 | arm64) readonly arch=arm64 ;;
*) die "unsupported machine type '$(uname -m)'; RKE2 publishes artifacts for amd64 and arm64 only" ;;
esac

readonly checksums="sha256sum-${arch}.txt"
readonly binary_tarball="rke2.linux-${arch}.tar.gz"
readonly installer="install.sh"
# GitHub download paths take the tag percent-encoded; the '+' in every RKE2 tag is otherwise read
# as a space and the asset is not found.
readonly base_url="https://github.com/rancher/rke2/releases/download/${release//+/%2B}"

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

# The checksum file is the trust anchor for everything else, so it is fetched first and refreshed
# -- once per run -- whenever an artifact disagrees with it. Re-downloading an artifact against a
# stale anchor would mismatch forever, with no recovery path.
checksums_refreshed=no
refresh_checksums() {
  [ "$checksums_refreshed" = no ] || return 0
  checksums_refreshed=yes
  log "refreshing ${checksums}: an artifact disagrees with it"
  fetch_checksums
}

# The digest the release publishes for a name, or empty when the name is not listed -- which is how
# an operator's own bundle is told apart from a published artifact.
checksum_lookup() {
  awk -v want="$1" '$2 == want { print $1; exit }' "${cache}/${checksums}" 2>/dev/null || true
}

# Both compressions of every image archive are published, and either may be missing from an older
# release branch. The checksum file decides, rather than a hardcoded suffix; the installer itself
# probes .tar.zst then .tar.gz, so this follows the same preference.
pick_archive() {
  local base="$1" suffix
  for suffix in .tar.zst .tar.gz; do
    if [ -n "$(checksum_lookup "${base}${suffix}")" ]; then
      echo "${base}${suffix}"
      return 0
    fi
  done
  # Neither listed: the anchor itself may be truncated, or from another release. Refresh it once
  # before concluding the release has no such asset.
  refresh_checksums
  for suffix in .tar.zst .tar.gz; do
    if [ -n "$(checksum_lookup "${base}${suffix}")" ]; then
      echo "${base}${suffix}"
      return 0
    fi
  done
  die "release ${release} lists neither ${base}.tar.zst nor ${base}.tar.gz in ${checksums}"
}

# The names this node wants, derived once so every phase agrees on them. The CNI extra is not an
# optimisation: the default image set carries hardened-calico and hardened-flannel, which is what
# canal needs, while calico/cilium/flannel each need a whole stack of their own images that live
# only in their own archive. Caching the default set alone with cni=calico means the apply
# succeeds and the entire Calico stack is pulled from a registry.
resolve_names() {
  [ -s "${cache}/${checksums}" ] || fetch_checksums
  core_archive="$(pick_archive "rke2-images.linux-${arch}")"
  cni_archive=""
  case "$cni" in
  calico | cilium | flannel) cni_archive="$(pick_archive "rke2-images-${cni}.linux-${arch}")" ;;
  esac
}

# --- fetch ------------------------------------------------------------------

# Brings one wanted published artifact into the cache, verified. The order is the correctness
# argument: a download is verified while it still carries a name nothing stages, and only then
# takes its real one. Renaming first would leave a truncated file occupying the final name, and
# every later run would "find" it and hand a corrupt archive to the installer forever.
ensure_verified() {
  local name="$1" expected actual part
  expected="$(checksum_lookup "$name")"
  [ -n "$expected" ] || die "${name} is not listed in ${checksums} for release ${release}"

  if [ -f "${cache}/${name}" ]; then
    actual="$(sha256_of "${cache}/${name}")"
    if [ "$actual" = "$expected" ]; then
      log "cached ${name} (verified)"
      return 0
    fi
    refresh_checksums
    expected="$(checksum_lookup "$name")"
    # A refreshed anchor that no longer lists this name means the release dropped the asset.
    # Without this the re-download below would compare against an empty digest.
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
  log "downloading ${installer} from https://get.rke2.io"
  download "https://get.rke2.io" "$part"
  grep -q 'INSTALL_RKE2_ARTIFACT_PATH' "$part" ||
    die "https://get.rke2.io did not return the RKE2 installer"
  mv -f "$part" "${cache}/${installer}"
  log "downloaded ${installer} (covered by no published digest, so trusted only as far as TLS)"
}

cmd_fetch() {
  mkdir -p "$cache"
  resolve_names

  local name
  for name in "$binary_tarball" "$core_archive" "$cni_archive"; do
    [ -n "$name" ] || continue
    ensure_verified "$name"
  done
  ensure_installer

  log "cache ${cache} ready for ${release} (${arch}, cni=${cni:-none})"
}

# --- artifacts --------------------------------------------------------------

link_or_copy() {
  # A hardlink keeps a ~1 GB archive from being duplicated; it fails across filesystems, and a
  # copy is then the only option.
  ln "$1" "$2" 2>/dev/null || cp -f "$1" "$2"
}

cmd_artifacts() {
  [ -n "$staging_dir" ] || die "--staging-dir is required"
  case "$staging_dir" in
  /) die "--staging-dir must not be /" ;;
  "${cache_root%/}" | "${cache_root%/}"/*)
    die "--staging-dir must be outside --cache-dir; it is wiped and refilled on every run"
    ;;
  /*) ;;
  *) die "--staging-dir must be absolute, got '${staging_dir}'" ;;
  esac
  [ -d "$cache" ] || die "no cache at ${cache}; run fetch first"
  resolve_names

  rm -rf "$staging_dir"
  mkdir -p "$staging_dir"

  # Exactly these, and nothing else. The installer needs the checksum file (it reads the expected
  # digests from it), the binary tarball, and the core image archive -- without that last one its
  # airgap step returns silently and NO images are installed, extras included. The CNI extra is
  # picked up by its own glob. Everything else in the cache stays out by construction: the image
  # list (.txt), the installer script, leftover partials, and an operator's own bundles, which are
  # staged after the install instead.
  local name
  for name in "$checksums" "$binary_tarball" "$core_archive" "$cni_archive"; do
    [ -n "$name" ] || continue
    [ -f "${cache}/${name}" ] || die "${name} is missing from ${cache}; run fetch first"
    link_or_copy "${cache}/${name}" "${staging_dir}/${name}"
  done

  log "prepared ${staging_dir} for the installer:"
  find "$staging_dir" -mindepth 1 -maxdepth 1 -exec basename {} \; | sort | sed "s/^/[$PROG]   /"
}

# --- stage ------------------------------------------------------------------

# Copies the cache's remaining image archives into the directory RKE2 imports from, so an operator
# who drops an extra bundle (their own images) into the release directory gets it imported too,
# with no second variable. A copy, never a move: the copy under the data directory is removed by
# the next uninstall, and the cache has to survive that.
cmd_stage() {
  [ -d "$cache" ] || die "no cache at ${cache}"
  mkdir -p "$IMAGES_DIR"

  local path name dest verified staged=0 present=0
  # *.txt is deliberately absent from these globs. In the cache a .txt is metadata (checksums,
  # image lists); in the images directory it means "pull every image named in me through the CRI
  # API" -- the exact opposite of importing locally.
  for path in "$cache"/*.tar "$cache"/*.tar.gz "$cache"/*.tar.zst; do
    [ -f "$path" ] || continue
    name="$(basename "$path")"
    case "$name" in
    partial.*)
      log "skipping ${name}: an interrupted download, not an artifact"
      continue
      ;;
    rke2.linux-*.tar.gz)
      # Matched for every arch, not just this node's: containerd would try to import a
      # foreign-arch binary tarball as images and fail at every start.
      log "skipping ${name}: the RKE2 binary tarball is not an image archive"
      continue
      ;;
    esac
    dest="${IMAGES_DIR}/${name}"
    if [ -f "$dest" ] && [ "$(size_of "$dest")" = "$(size_of "$path")" ]; then
      present=$((present + 1))
      continue
    fi
    if [ -n "$(checksum_lookup "$name")" ]; then
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
  # every image is still pulled from a registry -- and the installer's own airgap step returns
  # silently in exactly that case, so it cannot be relied on to have said anything.
  [ "$((staged + present))" -gt 0 ] ||
    die "no image archive landed in ${IMAGES_DIR}: neither the installer nor ${cache} put one there, so every image would be pulled from a registry"

  log "${cache} staged into ${IMAGES_DIR}: ${staged} archive(s) copied, ${present} already present"
}

case "$command" in
fetch) cmd_fetch ;;
artifacts) cmd_artifacts ;;
stage) cmd_stage ;;
*)
  echo "usage: $PROG <fetch|artifacts|stage> --release <tag> --cache-dir <dir> [--cni <name>] [--staging-dir <dir>]" >&2
  exit 2
  ;;
esac
