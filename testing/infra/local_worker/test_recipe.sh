#!/usr/bin/env bash
#
# Safety and validation cases for the local cluster recipe.
#
# The cases run in two groups. The fast group calls the pure predicates of
# lib.sh with declarative inputs and needs no cluster. The live group runs the
# recipe end to end: preflight, prepare, network, cleanup, and a second
# cleanup that has to succeed without touching anything.
#
# The live group is skipped when LOCAL_WORKER_TEST_FAST_ONLY is set to true.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=lib.sh
source "${SCRIPT_DIR}/lib.sh"

PASS_COUNT=0
FAIL_COUNT=0
FAILED_CASES=()
TMP_DIR=""
CLUSTER_WAS_PREPARED="false"

cleanup_tmp() {
  if [[ -n "${TMP_DIR}" && -d "${TMP_DIR}" ]]; then
    rm -rf "${TMP_DIR}"
  fi
}

cleanup_cluster() {
  if [[ "${CLUSTER_WAS_PREPARED}" == "true" ]]; then
    # A run that stops between a tamper with the ownership record and its
    # restore leaves the damaged record on disk, and a cleanup that reads it
    # refuses to remove the cluster, so the backup goes back first.
    if [[ -f "${OWNERSHIP_BACKUP}" && -f "$(lw_state_file)" ]] \
      && ! cmp -s "${OWNERSHIP_BACKUP}" "$(lw_state_file)"; then
      cp "${OWNERSHIP_BACKUP}" "$(lw_state_file)"
    fi
    bash "${SCRIPT_DIR}/run.sh" cleanup >/dev/null 2>&1 || true
  fi
}

trap 'cleanup_cluster; cleanup_tmp' EXIT

# Each case names one contract, the value it expects, and an expression that
# prints the observed value. The expression holds the data, not the logic.
run_case() {
  local name="$1" expected="$2" actual="$3"
  if [[ "${actual}" == "${expected}" ]]; then
    printf 'PASS  %s\n' "${name}"
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    printf 'FAIL  %s: expected %s, got %s\n' "${name}" "${expected}" "${actual}"
    FAIL_COUNT=$((FAIL_COUNT + 1))
    FAILED_CASES+=("${name}")
  fi
}

# The predicate calls below report a word, so a case states a word too and a
# difference is readable without decoding an exit status. The standard input
# is closed so a predicate cannot read the case table.
verdict() {
  if "$@" >/dev/null 2>&1 </dev/null; then
    printf 'yes\n'
  else
    printf 'no\n'
  fi
}

TMP_DIR="$(mktemp -d)"
OWNERSHIP_BACKUP="${TMP_DIR}/ownership.backup"

cat >"${TMP_DIR}/map-shaped.yaml" <<'FIXTURE'
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    "enable-aggregator-routing": "true"
FIXTURE

cat >"${TMP_DIR}/nameless-value.yaml" <<'FIXTURE'
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - value: "true"
FIXTURE

cat >"${TMP_DIR}/name-without-value.yaml" <<'FIXTURE'
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: "enable-aggregator-routing"
controllerManager:
  extraArgs:
    - name: controllers
      value: "*,-endpointslicemirroring"
FIXTURE

cat >"${TMP_DIR}/name-without-value-at-end.yaml" <<'FIXTURE'
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: "enable-aggregator-routing"
FIXTURE

cat >"${TMP_DIR}/list-shaped.yaml" <<'FIXTURE'
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: "enable-aggregator-routing"
      value: "true"
FIXTURE

# Fast cases

while IFS='|' read -r name expected expression; do
  [[ -z "${name}" ]] && continue
  run_case "${name}" "${expected}" "$(eval "${expression}")"
done <<'CASES'
a well formed cluster name is accepted|yes|verdict lw_cluster_name_valid gpustack-local-worker
an uppercase cluster name is refused|no|verdict lw_cluster_name_valid GPustack-Local
a cluster name with a space is refused|no|verdict lw_cluster_name_valid "gpustack local"
a cluster name another cluster holds is not free|no|verdict lw_cluster_name_is_free $'kind-alpha\nkind-beta' kind-alpha
a cluster name no cluster holds is free|yes|verdict lw_cluster_name_is_free "kind-alpha" gpustack-local-worker
the same container identifier matches|yes|verdict lw_ownership_matches sha256:aaa sha256:aaa
a different container identifier does not match|no|verdict lw_ownership_matches sha256:aaa sha256:bbb
an empty recorded identifier does not match|no|verdict lw_ownership_matches "" sha256:bbb
a kubeconfig outside the state directory is not isolated|no|verdict lw_kubeconfig_isolated /tmp/other/kubeconfig
an unset kubeconfig is isolated|yes|verdict lw_kubeconfig_isolated ""
a kubeconfig inside the state directory is isolated|yes|verdict lw_kubeconfig_isolated "$(lw_state_dir)/kubeconfig"
the recipe configuration uses name and value lists|yes|verdict lw_extra_args_use_name_value_lists "${SCRIPT_DIR}/kind-config.yaml"
a map shaped configuration is refused|no|verdict lw_extra_args_use_name_value_lists "${TMP_DIR}/map-shaped.yaml"
a value without a name is refused|no|verdict lw_extra_args_use_name_value_lists "${TMP_DIR}/nameless-value.yaml"
a name without a value is refused|no|verdict lw_extra_args_use_name_value_lists "${TMP_DIR}/name-without-value.yaml"
a name without a value at the end of the file is refused|no|verdict lw_extra_args_use_name_value_lists "${TMP_DIR}/name-without-value-at-end.yaml"
a name and value list is accepted|yes|verdict lw_extra_args_use_name_value_lists "${TMP_DIR}/list-shaped.yaml"
a selectorless service with an address and a port is valid|yes|verdict lw_mirror_fixture_is_valid "" "" "" "10.0.0.1" https
a service with a selector is not valid|no|verdict lw_mirror_fixture_is_valid "app=x" "" "" "10.0.0.1" https
endpoints carrying the skip-mirror label are not valid|no|verdict lw_mirror_fixture_is_valid "" "true" "" "10.0.0.1" https
endpoints carrying the leader annotation are not valid|no|verdict lw_mirror_fixture_is_valid "" "" "x" "10.0.0.1" https
endpoints without an address are not valid|no|verdict lw_mirror_fixture_is_valid "" "" "" "" https
endpoints without a named port are not valid|no|verdict lw_mirror_fixture_is_valid "" "" "" "10.0.0.1" ""
the aggregator routing flag is found in a rendered command|yes|verdict lw_has_arg "--enable-aggregator-routing=true --v=2" enable-aggregator-routing true
the mirroring exclusion token is found in a rendered command|yes|verdict lw_has_arg "--controllers=*,-endpointslicemirroring --v=2" controllers "*,-endpointslicemirroring"
a flag with another value is not found|no|verdict lw_has_arg "--enable-aggregator-routing=false" enable-aggregator-routing true
a quoted flag value is found|yes|verdict lw_has_arg '--controllers="*,-endpointslicemirroring"' controllers "*,-endpointslicemirroring"
a flag in a rendered command list is found|yes|verdict lw_has_arg '["kube-apiserver","--enable-aggregator-routing=true","--v=2"]' enable-aggregator-routing true
a flag with a comma in its value is found in a rendered command list|yes|verdict lw_has_arg '["kube-controller-manager","--controllers=*,-endpointslicemirroring"]' controllers "*,-endpointslicemirroring"
a flag in a rendered argument list is found|yes|verdict lw_has_arg '["--enable-aggregator-routing=true","--v=2"]' enable-aggregator-routing true
CASES

# The EndpointSlice observation cases

# The name a slice carries when it is there.
STUB_SLICE_NAME="endpointslices.discovery.k8s.io/mirror-check-abcde"
# The outcomes the stub of the read serves, in order.
STUB_SEQUENCE=""
# The file the stub records the reads it served in.
STUB_READ_LOG="${TMP_DIR}/stub-reads.log"

# The stub of the slice read. It serves the outcomes of the sequence in order
# and records every read it served in the log file. The record is the state of
# the stub, because the library reads the slice inside a command substitution
# and a shell variable would not survive it. The stub is installed inside the
# subshell of a case, so the live group keeps the real read.
stub_slice_read() {
  lw_read_slice_names() {
    local -a outcomes=()
    local index outcome
    IFS=',' read -r -a outcomes <<<"${STUB_SEQUENCE}"
    index="$(wc -l <"${STUB_READ_LOG}" | tr -d ' ')"
    if (( index >= ${#outcomes[@]} )); then
      printf 'the stub has no read left to serve\n' >&2
      return 1
    fi
    outcome="${outcomes[index]}"
    printf '%s\n' "${outcome}" >>"${STUB_READ_LOG}"
    case "${outcome}" in
      empty)
        return 0
        ;;
      listed)
        printf '%s\n' "${STUB_SLICE_NAME}"
        return 0
        ;;
      failed)
        printf 'the API server refused the read\n' >&2
        return 1
        ;;
      *)
        printf 'the stub does not know the outcome %s\n' "${outcome}" >&2
        return 1
        ;;
    esac
  }
}

# The status one slice read returns. The stub of the API call lives in the
# subshell of the case, so the live group keeps the real call.
slice_read_status() {
  local api_status="$1"
  (
    lw_kubectl() {
      if (( api_status != 0 )); then
        printf 'the API server refused the read\n' >&2
        return "${api_status}"
      fi
      printf '%s\n' "${STUB_SLICE_NAME}"
      return 0
    }
    local status=0
    lw_read_slice_names mirror-check >/dev/null 2>&1 || status=$?
    printf '%s\n' "${status}"
  )
}

# The names one slice read printed.
slice_read_names() {
  (
    lw_kubectl() {
      printf '%s\n' "${STUB_SLICE_NAME}"
      return 0
    }
    lw_read_slice_names mirror-check 2>/dev/null
  )
}

# The outcome of one observation over a sequence of read outcomes.
slice_observation() {
  local sequence="$1" timeout="$2"
  (
    lw_sleep() {
      :
    }
    STUB_SEQUENCE="${sequence}"
    : >"${STUB_READ_LOG}"
    stub_slice_read
    local status=0
    lw_wait_for_no_slice mirror-check "${timeout}" >/dev/null 2>&1 || status=$?
    case "${status}" in
      0)
        printf 'absent\n'
        ;;
      1)
        printf 'present\n'
        ;;
      2)
        printf 'read-failed\n'
        ;;
      *)
        printf 'unexpected-%s\n' "${status}"
        ;;
    esac
  )
}

# The number of reads one observation made.
slice_observation_reads() {
  local sequence="$1" timeout="$2"
  (
    lw_sleep() {
      :
    }
    STUB_SEQUENCE="${sequence}"
    : >"${STUB_READ_LOG}"
    stub_slice_read
    lw_wait_for_no_slice mirror-check "${timeout}" >/dev/null 2>&1 || true
  )
  wc -l <"${STUB_READ_LOG}" | tr -d ' '
}

while IFS='|' read -r name expected expression; do
  [[ -z "${name}" ]] && continue
  run_case "${name}" "${expected}" "$(eval "${expression}")"
done <<'CASES'
a successful read returns the status of the call|0|slice_read_status 0
a failed read returns the status of the call|1|slice_read_status 1
a successful read prints the names it read|endpointslices.discovery.k8s.io/mirror-check-abcde|slice_read_names
a successful absence is reported as absent|absent|slice_observation empty,empty 4
a nonempty slice is reported as present|present|slice_observation listed 4
a failed read is reported as a failed read|read-failed|slice_observation failed 4
a failure after an empty observation is reported as a failed read|read-failed|slice_observation empty,failed 4
the observation reads no more than the bound allows|2|slice_observation_reads empty,empty,empty 4
a window without room for the interval still performs its first read|1|slice_observation_reads empty,empty 0
CASES

# The fixture cleanup cases

# The file the stub of the namespace delete records the calls in.
STUB_CLEANUP_LOG="${TMP_DIR}/stub-cleanups.log"

# The file the cleanup scope reports its exit status in.
STUB_CLEANUP_STATUS_FILE="${TMP_DIR}/stub-cleanup-status"

# A command that fails, for the cleanup scope to run.
stub_failing_command() {
  exit 1
}

# A command that succeeds, for the cleanup scope to run.
stub_succeeding_command() {
  return 0
}

# Run the cleanup scope with the creation and the delete stubbed. The case
# states whether the namespace was created, whether the delete fails, and the
# command the scope runs.
scope_run() {
  local create_status="$1" cleanup_status="$2" body="$3"
  : >"${STUB_CLEANUP_LOG}"
  (
    # shellcheck disable=SC2317  # the scope calls it; the checker cannot see the call
    lw_create_fixture_namespace() {
      printf 'the namespace already exists\n' >&2
      return "${create_status}"
    }
    # shellcheck disable=SC2317  # the trap keeps it reachable; the checker cannot see the EXIT
    lw_delete_fixture_namespace() {
      printf 'deleted\n' >>"${STUB_CLEANUP_LOG}"
      return "${cleanup_status}"
    }
    local status=0
    lw_with_fixture_cleanup "${body}" >/dev/null 2>&1 || status=$?
    printf '%s\n' "${status}" >"${STUB_CLEANUP_STATUS_FILE}"
  )
}

# The exit status the cleanup scope reported.
scope_status() {
  local create_status="$1" cleanup_status="$2" body="$3"
  scope_run "${create_status}" "${cleanup_status}" "${body}"
  cat "${STUB_CLEANUP_STATUS_FILE}"
}

# The number of times the cleanup scope removed the fixture namespace.
scope_cleanups() {
  local create_status="$1" cleanup_status="$2" body="$3"
  scope_run "${create_status}" "${cleanup_status}" "${body}"
  wc -l <"${STUB_CLEANUP_LOG}" | tr -d ' '
}

# Whether the wrapper of a failed cluster creation deleted anything. kind is
# stubbed to fail the creation and to record its calls: kind itself removes
# the resources it created on failure, so the wrapper must not delete by
# name, because a foreign cluster that took the name meanwhile would be
# removed by that delete. The wrapper is read out of run.sh, so the case
# tests the source the recipe runs.
create_failure_deletes() {
  local fragment="${TMP_DIR}/create-cluster.fn" calls="${TMP_DIR}/kind-calls.log"
  sed -n '/^lw_create_cluster() {/,/^}/p' "${SCRIPT_DIR}/run.sh" >"${fragment}"
  : >"${calls}"
  (
    # shellcheck disable=SC1090  # the fragment holds the wrapper under test
    source "${fragment}"
    # shellcheck disable=SC2317  # the wrapper calls it; the checker cannot see the call
    kind() {
      printf '%s\n' "$*" >>"${calls}"
      [[ "$1" == "create" ]] && return 1
      return 0
    }
    lw_create_cluster >/dev/null 2>&1 || true
  )
  if grep -q '^delete cluster' "${calls}"; then
    printf 'deleted\n'
  else
    printf 'none\n'
  fi
}

# The exit trap of the shell that runs the cleanup scope. The stubs are
# defined in this shell, not in a nested subshell, because the case must
# compare the trap table of the very shell that calls the scope: a scope that
# armed its trap in the caller would change this table, and a scope that
# isolates itself in a subshell leaves it alone. The stub definitions stay
# inside the case because the case runs in its own command substitution.
scope_trap() {
  local before after
  before="$(trap -p EXIT)"
  # shellcheck disable=SC2317  # the scope calls it; the checker cannot see the call
  lw_create_fixture_namespace() {
    return 0
  }
  # shellcheck disable=SC2317  # the trap keeps it reachable; the checker cannot see the EXIT
  lw_delete_fixture_namespace() {
    return 0
  }
  lw_with_fixture_cleanup stub_failing_command >/dev/null 2>&1 || true
  after="$(trap -p EXIT)"
  if [[ "${before}" == "${after}" ]]; then
    printf 'unchanged\n'
  else
    printf 'changed\n'
  fi
}

while IFS='|' read -r name expected expression; do
  [[ -z "${name}" ]] && continue
  run_case "${name}" "${expected}" "$(eval "${expression}")"
done <<'CASES'
the fixture namespace is removed when the proof fails|1|scope_cleanups 0 0 stub_failing_command
the failure status survives a failing cleanup|1|scope_status 0 3 stub_failing_command
the scope fails when the namespace was not created|1|scope_status 1 0 stub_succeeding_command
a namespace that was not created is not deleted|0|scope_cleanups 1 0 stub_succeeding_command
the cleanup scope leaves the trap of the calling shell alone|unchanged|scope_trap
a failed cluster creation deletes nothing by name|none|create_failure_deletes
CASES

# Live cases

# A live case names the contract, the exit status it expects, and the command
# that produces the observed status. The command output is printed only when
# the case fails.
run_live() {
  local name="$1" expected="$2"
  shift 2
  local status=0 output
  output="$("$@" 2>&1)" || status=$?
  if [[ "${status}" == "${expected}" ]]; then
    printf 'PASS  %s\n' "${name}"
    PASS_COUNT=$((PASS_COUNT + 1))
  else
    printf 'FAIL  %s: expected exit %s, got %s\n' "${name}" "${expected}" "${status}"
    printf '%s\n' "${output}" | tail -25
    FAIL_COUNT=$((FAIL_COUNT + 1))
    FAILED_CASES+=("${name}")
  fi
}

if [[ "${LOCAL_WORKER_TEST_FAST_ONLY:-false}" == "true" ]]; then
  printf '\nThe live group is skipped because LOCAL_WORKER_TEST_FAST_ONLY is true.\n'
else
  printf '\n--- live group ---\n'

  run_live "preflight succeeds" 0 bash "${SCRIPT_DIR}/run.sh" preflight

  # The flag goes up before prepare runs: a prepare that fails after the
  # cluster was created still has its cleanup taken by the exit trap, because
  # the trap is the last chance to remove a cluster this run created.
  CLUSTER_WAS_PREPARED="true"
  run_live "prepare succeeds" 0 bash "${SCRIPT_DIR}/run.sh" prepare

  # The expressions in this table are read by the shared loop and evaluated
  # there, so they expand at evaluation time only.
  while IFS='|' read -r name expected expression; do
    [[ -z "${name}" ]] && continue
    run_case "${name}" "${expected}" "$(eval "${expression}")"
  done <<'CASES'
the ownership record exists|yes|[[ -f "$(lw_state_file)" ]] && printf 'yes\n' || printf 'no\n'
the cluster exists|yes|lw_cluster_exists && printf 'yes\n' || printf 'no\n'
the task-local kubeconfig exists|yes|[[ -f "$(lw_kubeconfig_path)" ]] && printf 'yes\n' || printf 'no\n'
the default kubeconfig does not name the task context|no|grep -Fq -- "$(lw_context)" "$(lw_default_kubeconfig_path)" 2>/dev/null && printf 'yes\n' || printf 'no\n'
the default kubeconfig is unchanged|yes|lw_default_kubeconfig_unchanged && printf 'yes\n' || printf 'no\n'
the fixture namespace is gone after prepare|no|lw_kubectl get namespace "${LW_NAMESPACE}" >/dev/null 2>&1 && printf 'yes\n' || printf 'no\n'
CASES

  # The pinned versions are read from the configuration, so a run that
  # overrides them compares against what it asked for.
  run_case "the task-local kubectl is the pinned version" "${LW_KUBECTL_VERSION}" \
    "$("$(lw_kubectl_path)" version --client -o json | awk -F'"' '/"gitVersion"/ {print $4; exit}')"
  run_case "the cluster reports the pinned server version" "${LW_KUBERNETES_VERSION}" "$(lw_server_version)"

  # A namespace that already existed must survive the proof. The namespace is
  # one this run created for the check, so the collision is exercised without
  # touching a namespace the recipe did not create, and the teardown removes
  # only that namespace.
  foreign_namespace="gpustack-local-worker-foreign-$$"
  cat >"${TMP_DIR}/collision.sh" <<'COLLISION'
#!/usr/bin/env bash
set -o errexit
set -o nounset
set -o pipefail
source "${1}/lib.sh"
lw_with_fixture_cleanup true
COLLISION
  lw_kubectl create namespace "${foreign_namespace}"
  run_live "the proof refuses a namespace it did not create" 1 \
    env LOCAL_WORKER_NAMESPACE="${foreign_namespace}" bash "${TMP_DIR}/collision.sh" "${SCRIPT_DIR}"
  run_case "the namespace that was not created survived" "yes" \
    "$(lw_kubectl get namespace "${foreign_namespace}" >/dev/null 2>&1 && printf 'yes\n' || printf 'no\n')"
  lw_kubectl delete namespace "${foreign_namespace}"

  run_live "network succeeds" 0 bash "${SCRIPT_DIR}/run.sh" network

  # A cluster whose live node is not the one this recipe created must not be
  # removed, even when the name matches.
  cp "$(lw_state_file)" "${OWNERSHIP_BACKUP}"
  sed 's|^NODE_CONTAINER_ID=.*|NODE_CONTAINER_ID=sha256:0000000000000000000000000000000000000000000000000000000000000000|' \
    "${OWNERSHIP_BACKUP}" >"$(lw_state_file)"
  run_live "cleanup refuses a cluster it cannot prove it created" 1 \
    bash "${SCRIPT_DIR}/run.sh" cleanup
  run_case "the refused cleanup left the cluster alone" "yes" \
    "$(lw_cluster_exists && printf 'yes\n' || printf 'no\n')"

  # A damaged ownership record must not be read as an absent one.
  printf 'CLUSTER_NAME=%s\nNOT_A_RECORDED_KEY=value\n' "${LW_CLUSTER_NAME}" >"$(lw_state_file)"
  run_live "cleanup refuses a damaged ownership record" 1 \
    bash "${SCRIPT_DIR}/run.sh" cleanup
  run_case "the refused cleanup left the cluster alone" "yes" \
    "$(lw_cluster_exists && printf 'yes\n' || printf 'no\n')"

  cp "${OWNERSHIP_BACKUP}" "$(lw_state_file)"

  # A cleanup that names a cluster the record does not name must not delete
  # the record, because the cluster the record names may still be running.
  run_live "cleanup refuses a cluster name it did not record" 1 \
    env LOCAL_WORKER_CLUSTER_NAME="gpustack-local-worker-other" bash "${SCRIPT_DIR}/run.sh" cleanup
  run_case "the name-mismatched cleanup left the cluster alone" "yes" \
    "$(lw_cluster_exists && printf 'yes\n' || printf 'no\n')"

  run_live "cleanup succeeds" 0 bash "${SCRIPT_DIR}/run.sh" cleanup
  CLUSTER_WAS_PREPARED="false"
  run_case "the cluster is gone" "no" \
    "$(lw_cluster_exists && printf 'yes\n' || printf 'no\n')"
  run_case "the ownership record is gone" "no" \
    "$([[ -f "$(lw_state_file)" ]] && printf 'yes\n' || printf 'no\n')"

  # A second cleanup has to succeed without removing anything.
  run_live "cleanup is repeatable" 0 bash "${SCRIPT_DIR}/run.sh" cleanup
  run_case "the repeat left the cluster alone" "no" \
    "$(lw_cluster_exists && printf 'yes\n' || printf 'no\n')"
fi

printf '\n%s passed, %s failed\n' "${PASS_COUNT}" "${FAIL_COUNT}"
if (( FAIL_COUNT > 0 )); then
  printf 'failed cases:\n'
  printf '  %s\n' "${FAILED_CASES[@]}"
  exit 1
fi
