#!/usr/bin/env bash

##
# Inspired by github.com/kubernetes/kubernetes/hack/lib/version.sh
##

# -----------------------------------------------------------------------------
# Version management helpers. These functions help to set the
# following variables:
#
#    GIT_TREE_STATE  -  "clean" indicates no changes since the git commit id.
#                       "dirty" indicates source code changes after the git commit id.
#                       "archive" indicates the tree was produced by 'git archive'.
#                       "unknown" indicates cannot find out the git tree.
#        GIT_COMMIT  -  The git commit id corresponding to this
#                       source code.
#       GIT_VERSION  -  "vX.Y" used to indicate the last release version,
#                       it can be specified via "VERSION".
#        BUILD_DATE  -  The build date of the version.

function gpustack::version::get_version_vars() {
  local version_override="${VERSION:-${GIT_VERSION:-}}"
  local commit_override="${COMMIT:-${GIT_COMMIT:-}}"
  local git_status git_tag
  # shellcheck disable=SC2034 # These variables are consumed by the sourcing build scripts.
  BUILD_DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  GIT_TREE_STATE="unknown"
  GIT_COMMIT="unknown"
  GIT_VERSION="unknown"

  # get the git tree state if the source was exported through git archive.
  # shellcheck disable=SC2016,SC2050
  if [[ '$Format:%%$' == "%" ]]; then
    GIT_TREE_STATE="archive"
    GIT_COMMIT='$Format:%H$'
    # when a 'git archive' is exported, the '$Format:%D$' below will look
    # something like 'HEAD -> release-1.8, tag: v1.8.3' where then 'tag: '
    # can be extracted from it.
    if [[ '$Format:%D$' =~ tag:\ (v[^ ,]+) ]]; then
      GIT_VERSION="${BASH_REMATCH[1]}"
    else
      GIT_VERSION="${GIT_COMMIT:0:7}"
    fi
    # respect specified version.
    GIT_VERSION="${version_override:-${GIT_VERSION}}"
    GIT_COMMIT="${commit_override:-${GIT_COMMIT}}"
    return
  fi

  # return directly if not found git client.
  if [[ -z "$(command -v git)" ]]; then
    # respect specified version.
    GIT_VERSION=${version_override:-${GIT_VERSION}}
    GIT_COMMIT=${commit_override:-${GIT_COMMIT}}
    return
  fi

  # find out git info via git client. The assignment below lands even when the command fails,
  # leaving GIT_COMMIT empty rather than the "unknown" it started as, so restore that before
  # anything reads it.
  if GIT_COMMIT=$(git -C "${ROOT_DIR:-.}" rev-parse "HEAD^{commit}" 2>/dev/null); then
    # specify as dirty if the tree is not clean.
    if git_status=$(git -C "${ROOT_DIR:-.}" status --porcelain 2>/dev/null); then
      if [[ -n ${git_status} ]]; then
        GIT_TREE_STATE="dirty"
      else
        GIT_TREE_STATE="clean"
      fi
    fi

    # specify with the tag if the head is tagged.
    if GIT_VERSION="$(git -C "${ROOT_DIR:-.}" rev-parse --abbrev-ref HEAD 2>/dev/null)"; then
      if git_tag=$(git -C "${ROOT_DIR:-.}" describe --tags --exact-match 2>/dev/null) && [[ -n ${git_tag} ]]; then
        GIT_VERSION="${git_tag}"
      fi
    fi

    # specify to dev if the tree is dirty.
    if [[ "${GIT_TREE_STATE:-dirty}" == "dirty" ]]; then
      GIT_VERSION="dev"
    elif ! [[ "${GIT_VERSION}" =~ ^v([0-9]+)\.([0-9]+)(\.[0-9]+)?(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
      GIT_VERSION="dev"
    fi
  fi

  # Build arguments take precedence even in archives and inaccessible worktrees.
  GIT_VERSION=${version_override:-${GIT_VERSION:-unknown}}
  GIT_COMMIT=${commit_override:-${GIT_COMMIT:-unknown}}
}
