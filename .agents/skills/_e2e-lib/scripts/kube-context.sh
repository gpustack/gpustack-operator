#!/usr/bin/env bash
#
# Point a run at a kube context that is NOT the current one, without switching it.
# READ-ONLY with respect to ~/.kube/config — it never writes that file.
#
#   kube-context.sh <context>
#
# It extracts the one context into a standalone, flattened kubeconfig (mode 600) and prints
# a single `export KUBECONFIG=<path>` line on stdout; everything else goes to stderr. Use it
# either way round:
#
#   eval "$(bash .claude/skills/_e2e-lib/scripts/kube-context.sh <ctx>)"   # one interactive shell
#   KUBECONFIG=<path> bash .claude/skills/.../case-1.sh <NS>               # per command, as a run does
#
# The copy names the target as its OWN current-context, which is what makes the rest of the
# suite work unchanged: no case has to pass --context, `helm` (which takes no --context) follows
# the same file, and `kubectl config current-context` — the way build-load.sh picks its image
# import path and preflight.sh reports the cluster to confirm — answers the targeted context
# rather than the user's.
#
# Why not `kubectl config use-context`: the user's kubeconfig is shared with whatever else they
# are doing (a chart mid-verification in another window), and a run that switches it owes a
# restore that a crash or a context compaction can lose. Why not a shell alias: an alias is not
# expanded in a non-interactive shell, so it would never reach a single kubectl inside a case.
#
# The copy is a SNAPSHOT. Re-run this after anything rewrites ~/.kube/config — a provision, a
# re-fetched credential. An exec-credential plugin is carried over as written, so a token still
# refreshes normally; only the context/cluster/user entries are frozen.
#
# Exit 0 = written, 1 = no such context / extraction failed.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

CTX="${1:?usage: kube-context.sh <context>   (list them with: kubectl config get-contexts -o name)}"

if ! kubectl config get-contexts -o name 2>/dev/null | grep -qxF "$CTX"; then
  echo "no context '${CTX}' in this kubeconfig. Available:" >&2
  kubectl config get-contexts -o name 2>/dev/null | sed 's/^/  /' >&2
  # The likeliest cause of a one-context list: this is being run again from a shell that
  # already eval'd an earlier copy, which holds that one context and nothing else.
  [ -n "${KUBECONFIG:-}" ] && echo "(reading KUBECONFIG=${KUBECONFIG} — unset it to see your whole kubeconfig)" >&2
  exit 1
fi

# Deterministic path, so every later command in the run can name it without this script
# having to be re-run and without the path being passed around.
TMPD="${TMPDIR:-/tmp}"
OUT="${TMPD%/}/e2e-kubeconfig-${CTX//[^a-zA-Z0-9._-]/-}.yaml"

# Via a temp file: a direct redirect would truncate an existing, working copy before kubectl
# had a chance to fail.
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
if ! kubectl config view --minify --flatten --context="$CTX" >"$TMP" || [ ! -s "$TMP" ]; then
  echo "could not extract context '${CTX}' from this kubeconfig" >&2
  exit 1
fi
# The path is predictable, so whatever sits there must be OURS before it is replaced. On a
# shared sticky /tmp another user's file cannot be renamed over (rename(2) gives EPERM), and
# on a plain leftover — say a root-owned copy from one sudo run — chmod fails the same way.
# Unchecked, either leaves a foreign kubeconfig at the path this script then hands the caller
# to export, and its exec-credential plugin would run as the caller. Refuse instead: a plain
# file we own is the only thing safe to overwrite (a symlink is not; rename replaces the link,
# not its target).
if [ -e "$OUT" ] || [ -L "$OUT" ]; then
  if [ -L "$OUT" ] || [ ! -f "$OUT" ] || [ ! -O "$OUT" ]; then
    echo "refusing to write ${OUT}: it already exists and is not a plain file you own" >&2
    echo "inspect it (ls -l) and remove it yourself if it is stale" >&2
    exit 1
  fi
fi
mv "$TMP" "$OUT" || { echo "could not write ${OUT}" >&2; exit 1; }
chmod 600 "$OUT" || { echo "could not restrict ${OUT} to mode 600" >&2; exit 1; }

echo "context '${CTX}' extracted to ${OUT} — your ~/.kube/config is untouched" >&2
echo "the copy's own current-context: $(KUBECONFIG="$OUT" kubectl config current-context 2>/dev/null)" >&2
echo "still current in ~/.kube/config: $(kubectl config current-context 2>/dev/null || echo '<none set>')" >&2
# %q, because this line is meant to be eval'd: an unquoted path with a space in it (a TMPDIR
# the caller chose) would split into a variable assignment plus a command to run.
printf 'export KUBECONFIG=%q\n' "$OUT"
