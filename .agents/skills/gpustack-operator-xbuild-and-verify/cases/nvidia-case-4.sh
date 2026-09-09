#!/usr/bin/env bash
#
# NVIDIA-CASE 4 — postinit lock recovery after owner death
#   (parts A/B need no GPU; part C needs a real NVIDIA GPU host)
#
#   nvidia-case-4.sh [TARGET]
#
# HAMi-core serializes postInit's host-PID detection. It used to do so with an
# unnamed semaphore living inside the shared region, which nothing releases when
# its holder dies: every later process then spent SEM_WAIT_TIME_POSTINIT(30) x
# SEM_WAIT_RETRY_TIMES_POSTINIT(10) = 300s before giving up and running without
# host-PID detection. It now takes a POSIX record lock on a byte of the shared
# cache FILE, at the fixed offset 0x40000000, which the kernel releases when the
# holder exits — SIGKILL included.
#
# Three parts, weakest evidence first:
#   A. the shipped libvgpu.so carries the record-lock path and NOT the semaphore
#      give-up path — a revert tripwire on the pinned commit (no GPU).
#   B. upstream's own GPU-free regression test, built from the pinned source in
#      the image, which kills a holder and a waiter and asserts the next waiter
#      acquires promptly (no GPU).
#   C. the black box: a real container with the real injected libvgpu.so. The
#      lock is observed in the HOST's /proc/locks, on the shared cache file's
#      inode at offset 0x40000000; the holder is then SIGKILLed *while that entry
#      is present*, and the next container must still complete (needs 1 GPU).
#
# The kill is triggered by the lock's appearance, never by a fixed delay. A first
# version of this case killed at staggered offsets across the postInit window and
# passed 5/5 against the OLD pin as well: no kill had landed inside the window, so
# five green rows attested to nothing. A round that cannot observe the lock before
# it fires is now reported unarmed and counted as evidence of nothing.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE), XB_STAGE (/opt/vgpu), XB_GPU (0),
#      XB_MEM (4096 MiB), XB_KILL_ROUNDS (3), XB_ARM_TIMEOUT (30s — how long a
#      round waits for the lock to appear before firing unarmed),
#      XB_RECOVER_TIMEOUT (60s — the bound the old 300s give-up would blow).
#      Prints STATUS|CHECK|DETAIL; non-zero on FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:-}"
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-}}"
[ -z "${IMG}" ] && [ -n "${TARGET}" ] && IMG="vgpu-build:${TARGET#xbuild-nvidia-cuda-}"
[ -n "${IMG}" ] || { echo "nvidia-case-4: pass a TARGET (e.g. xbuild-nvidia-cuda-13) or set XB_WORKLOAD_IMAGE"; exit 2; }

echo "# NVIDIA-CASE 4 — postinit owner-death recovery (image ${IMG}, gpu ${XB_GPU:-0}) on $(xtarget_desc)"

out="$(xsh \
  IMG="${IMG}" STAGE="${XB_STAGE:-/opt/vgpu}" \
  GPU="${XB_GPU:-0}" MEM="${XB_MEM:-4096}" \
  KILL_ROUNDS="${XB_KILL_ROUNDS:-3}" ARM_TIMEOUT="${XB_ARM_TIMEOUT:-30}" \
  RECOVER_TIMEOUT="${XB_RECOVER_TIMEOUT:-60}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
LOCK_OFFSET=1073741824   # 0x40000000, POSTINIT_FILE_LOCK_OFFSET

# ---------------------------------------------------------------- part A ----
# The shipped artifact must carry the record-lock path and not the semaphore
# give-up path. Every string below comes from a LOG_MSG/LOG_ERROR/LOG_WARN
# format literal, so it survives in .rodata at any log level.
echo "--- part A: shipped libvgpu.so carries the record-lock path (no GPU) ---"
a_out="$(docker run --rm --entrypoint bash "${IMG}" -c '
  LV=/out/libvgpu.so
  [ -f "$LV" ] || { echo "MISSING"; exit 0; }
  for s in \
    "Waiting for postinit file lock" \
    "Postinit lock cannot be acquired recursively" \
    "Skipped host PID detection because the postinit lock failed" ; do
    if strings -a "$LV" | grep -qF "$s"; then echo "PRESENT|$s"; else echo "ABSENT|$s"; fi
  done
  for s in \
    "Postinit lock timeout after" \
    "Skipping host PID detection for this process" \
    "Skipped host PID detection due to lock timeout" ; do
    if strings -a "$LV" | grep -qF "$s"; then echo "STALE|$s"; else echo "GONE|$s"; fi
  done
' 2>&1)"

# newpath records which implementation actually shipped, so part C can tell a
# missing record lock (a defect) from one that was never supposed to exist (the
# semaphore path, e.g. when this case is pointed at an older pin as a control).
newpath=0; a_present=0; a_bad=0
if echo "${a_out}" | grep -q '^MISSING$'; then
  row FAIL "/out/libvgpu.so exists" missing; fails=$((fails+1))
else
  while IFS='|' read -r verdict s; do
    case "${verdict}" in
      PRESENT) row PASS "new: ${s}" "present"; a_present=$((a_present+1)) ;;
      ABSENT)  row FAIL "new: ${s}" "ABSENT — pin did not move"; fails=$((fails+1)); a_bad=$((a_bad+1)) ;;
      GONE)    row PASS "old removed: ${s}" "absent" ;;
      STALE)   row FAIL "old removed: ${s}" "STILL PRESENT — semaphore path shipped"; fails=$((fails+1)); a_bad=$((a_bad+1)) ;;
    esac
  done <<< "$(echo "${a_out}" | grep -E '^(PRESENT|ABSENT|GONE|STALE)\|')"
  [ "${a_present}" -eq 3 ] && [ "${a_bad}" -eq 0 ] && newpath=1
fi

# ---------------------------------------------------------------- part B ----
# Upstream's own regression test, built from the pinned source already staged in
# the image. It links only -lrt -lpthread (no CUDA/NVML), so it needs no GPU.
echo "--- part B: upstream's GPU-free owner-death regression test (no GPU) ---"
b_out="$(docker run --rm --entrypoint bash "${IMG}" -c '
  set -u
  src=""
  for d in /tmp/hami-core /tmp/hami-core-src; do
    [ -f "$d/test/test_postinit_owner_death.c" ] && { src="$d"; break; }
  done
  [ -z "$src" ] && { echo "NOSRC"; exit 0; }
  echo "SRC=$src"
  bin="$(find "$src/build" -type f -name test_postinit_owner_death 2>/dev/null | head -1)"
  if [ -z "$bin" ] && [ -d "$src/build" ]; then
    (cd "$src/build" && make test_postinit_owner_death) >/tmp/mk.log 2>&1 || { echo "BUILDFAIL"; tail -5 /tmp/mk.log; exit 0; }
    bin="$(find "$src/build" -type f -name test_postinit_owner_death 2>/dev/null | head -1)"
  fi
  [ -z "$bin" ] && { echo "NOBIN"; exit 0; }
  echo "BIN=$bin"
  start=$(date +%s)
  timeout 120 "$bin" 2>&1; rc=$?
  echo "RC=$rc ELAPSED=$(( $(date +%s) - start ))s"
' 2>&1)"
echo "${b_out}" | sed 's/^/    /'

if echo "${b_out}" | grep -q '^NOSRC$'; then
  row WARN "upstream owner-death test" "pinned source not staged in the image — cannot run"
elif echo "${b_out}" | grep -qE '^(BUILDFAIL|NOBIN)$'; then
  row FAIL "upstream owner-death test builds" "$(echo "${b_out}" | grep -E '^(BUILDFAIL|NOBIN)$')"; fails=$((fails+1))
else
  brc="$(echo "${b_out}" | sed -nE 's/^RC=([0-9]+).*/\1/p' | tail -1)"
  if echo "${b_out}" | grep -qF 'postinit owner-death tests passed' && [ "${brc}" = "0" ]; then
    row PASS "upstream owner-death test" "passed ($(echo "${b_out}" | sed -nE 's/.*ELAPSED=([0-9]+s).*/\1/p' | tail -1))"
  else
    row FAIL "upstream owner-death test" "rc=${brc:-none}, no pass marker"; fails=$((fails+1))
  fi
fi

# ---------------------------------------------------------------- part C ----
echo "--- part C: real container, host-observed record lock + kill sweep (needs GPU) ---"
if ! command -v nvidia-smi >/dev/null 2>&1 || [ "$(nvidia-smi -L 2>/dev/null | wc -l)" -lt 1 ]; then
  row WARN "part C" "no NVIDIA GPU on this host — container rows unavailable"
  echo "FAILS=${fails}"
  exit 0
fi

T="${STAGE}/test-case4"; rm -rf "${T}"; mkdir -p "${T}/vgpulock" "${T}/vgpu"
printf '/usr/local/vgpu/libvgpu.so\n' > "${T}/ld.so.preload"; chmod 0644 "${T}/ld.so.preload"
CACHE="${T}/vgpu/cudevshr.cache"

# A probe that drives postInit (cuInit + primary ctx) and then holds the process
# alive, so the container can be SIGKILLed at a chosen point.
cat > "${T}/probe.c" <<'PY'
#include <cuda.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
int main(int argc, char** argv){
  CUcontext c; CUdevice d; size_t f=0,t=0;
  if(cuInit(0)){printf("PROBE cuInit fail\n");fflush(stdout);return 1;}
  cuDeviceGet(&d,0);
  if(cuDevicePrimaryCtxRetain(&c,d)){printf("PROBE ctx fail\n");fflush(stdout);return 1;}
  cuCtxSetCurrent(c);
  cuMemGetInfo(&f,&t);
  printf("PROBE ok total=%zuMiB\n", t/1048576); fflush(stdout);
  if(argc>1) sleep(atoi(argv[1]));
  return 0;
}
PY

INJ="-e NVIDIA_VISIBLE_DEVICES=${GPU} \
 -e CUDA_DEVICE_MEMORY_LIMIT_0=${MEM}m -e CUDA_DEVICE_SM_LIMIT=100 \
 -e CUDA_DEVICE_MEMORY_SHARED_CACHE=/tmp/vgpu/cudevshr.cache \
 -e LIBCUDA_LOG_LEVEL=3 \
 -v ${STAGE}/libvgpu.so:/usr/local/vgpu/libvgpu.so:ro \
 -v ${T}/ld.so.preload:/etc/ld.so.preload:ro \
 -v ${T}/vgpulock:/tmp/vgpulock -v ${T}/vgpu:/tmp/vgpu -v /dev/shm:/dev/shm"

# Compile the probe once and reuse the binary for every round.
docker run --rm -v "${T}:/w" --entrypoint bash "${IMG}" \
  -c 'nvcc -o /w/probe /w/probe.c -L/usr/local/cuda/lib64/stubs -lcuda' >/tmp/nvcc.log 2>&1
if [ ! -x "${T}/probe" ]; then
  row FAIL "probe compiles" "$(tail -2 /tmp/nvcc.log)"; fails=$((fails+1))
  echo "FAILS=${fails}"; exit 0
fi
row PASS "probe compiles" "ok"

give_up_re='Postinit lock timeout after|Skipping host PID detection|Skipped host PID detection'

# C1 — baseline: a lone container completes postInit, with neither give-up line.
base="$(timeout "${RECOVER_TIMEOUT}" docker run --rm ${INJ} -v "${T}/probe:/probe:ro" \
  --entrypoint /probe "${IMG}" 2>&1)"; brc=$?
if [ ${brc} -eq 0 ] && echo "${base}" | grep -q 'PROBE ok'; then
  row PASS "baseline container completes postInit" "rc=0"
else
  row FAIL "baseline container completes postInit" "rc=${brc}"; fails=$((fails+1))
fi
if echo "${base}" | grep -qE "${give_up_re}"; then
  row FAIL "baseline logs no give-up" "$(echo "${base}" | grep -oE "${give_up_re}" | head -1)"; fails=$((fails+1))
else
  row PASS "baseline logs no give-up" "clean"
fi

if [ ! -f "${CACHE}" ]; then
  row FAIL "shared cache file created" "${CACHE} absent after the baseline run"; fails=$((fails+1))
  echo "FAILS=${fails}"; exit 0
fi
ino="$(stat -c %i "${CACHE}")"

# Spin on /proc/locks until the holder's record lock shows up on the cache
# inode at LOCK_OFFSET. Pure bash — $(<file) forks nothing, so the sample rate is
# limited only by the read itself, and a window a sleep-based poll would step
# over is still caught. Echoes the matching line; empty means never seen.
await_lock() {
  local deadline="$1" l
  SECONDS=0
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    l="$(</proc/locks)"
    case "${l}" in
      *":${ino} ${LOCK_OFFSET} "*|*":${ino} ${LOCK_OFFSET}")
        printf '%s\n' "${l}" | grep -E ":${ino} ${LOCK_OFFSET}( |$)" | head -1
        return 0 ;;
    esac
  done
  return 1
}

# C2/C3 — one loop: start a holder, wait for its lock to actually appear, and
# SIGKILL it at that moment. The first observation doubles as the mechanism row.
# A round whose lock never appears fires anyway but is recorded as unarmed and
# proves nothing, so a run that armed nothing cannot read as a pass.
armed=0; unarmed=0; stalled=0; dirty=0; observed=""
r=0
while [ "${r}" -lt "${KILL_ROUNDS}" ]; do
  r=$((r+1))
  cid="$(docker run -d ${INJ} -v "${T}/probe:/probe:ro" --entrypoint /probe "${IMG}" 30 2>/dev/null)"
  [ -z "${cid}" ] && { row FAIL "round ${r} starts a holder" "docker run failed"; fails=$((fails+1)); continue; }

  hit="$(await_lock "${ARM_TIMEOUT}")"
  docker kill -s KILL "${cid}" >/dev/null 2>&1
  docker rm -f "${cid}" >/dev/null 2>&1

  if [ -n "${hit}" ]; then
    armed=$((armed+1)); [ -z "${observed}" ] && observed="${hit}"
  else
    unarmed=$((unarmed+1))
    row WARN "round ${r}" "lock never appeared within ${ARM_TIMEOUT}s — killed unarmed, proves nothing"
    continue
  fi

  s=$(date +%s)
  nxt="$(timeout "${RECOVER_TIMEOUT}" docker run --rm ${INJ} -v "${T}/probe:/probe:ro" \
    --entrypoint /probe "${IMG}" 2>&1)"; nrc=$?
  e=$(( $(date +%s) - s ))
  if [ ${nrc} -ne 0 ] || ! echo "${nxt}" | grep -q 'PROBE ok'; then
    stalled=$((stalled+1)); fails=$((fails+1))
    row FAIL "round ${r}: recover after killing the lock holder" "rc=${nrc} after ${e}s (timeout ${RECOVER_TIMEOUT}s)"
  elif echo "${nxt}" | grep -qE "${give_up_re}"; then
    dirty=$((dirty+1)); fails=$((fails+1))
    row FAIL "round ${r}: recover after killing the lock holder" "completed in ${e}s but logged: $(echo "${nxt}" | grep -oE "${give_up_re}" | head -1)"
  else
    row PASS "round ${r}: recover after killing the lock holder" "${e}s, no give-up"
  fi
done

# The mechanism row, decided by what shipped: under the record-lock path the lock
# must be observable, and its absence is the defect; under the semaphore path
# there is no record lock to find and saying so is the correct outcome.
if [ -n "${observed}" ]; then
  case "${observed}" in
    *WRITE*) row PASS "record lock at 0x40000000 on the cache inode" "$(echo "${observed}" | xargs)" ;;
    *) row FAIL "record lock is a WRITE lock" "$(echo "${observed}" | xargs)"; fails=$((fails+1)) ;;
  esac
elif [ "${newpath}" -eq 1 ]; then
  row FAIL "record lock at 0x40000000 on the cache inode" \
    "never observed in ${KILL_ROUNDS} rounds though the record-lock path is what shipped"; fails=$((fails+1))
else
  row INFO "record lock at 0x40000000" "absent, as expected: this artifact ships the semaphore path"
fi
row INFO "kill rounds" "${armed} armed, ${unarmed} unarmed, ${stalled} stalled, ${dirty} logged a give-up"
if [ "${newpath}" -eq 1 ] && [ "${armed}" -eq 0 ]; then
  row FAIL "owner death was actually exercised" "no round armed — the recovery rows attest to nothing"; fails=$((fails+1))
fi

# C4 — after every kill, the kernel must have dropped the lock: no entry for the
# cache inode survives. This is the half of the mechanism a live poll cannot see.
if [ -f "${CACHE}" ]; then
  ino="$(stat -c %i "${CACHE}")"
  stale="$(awk -v ino="${ino}" '{split($6,d,":"); if ((d[3]+0)==(ino+0)) print}' /proc/locks 2>/dev/null | head -1)"
  if [ -z "${stale}" ]; then
    row PASS "no stale lock after owner death" "/proc/locks clean for inode ${ino}"
  else
    row FAIL "no stale lock after owner death" "$(echo "${stale}" | xargs)"; fails=$((fails+1))
  fi
fi

rm -rf "${T}"
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "NVIDIA-CASE 4" "$(xb_fails "${out}")"
