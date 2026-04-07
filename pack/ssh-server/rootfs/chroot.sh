#!/bin/sh

set -e

clear || true

# Find a suitable target process to enter.
TARGET_PID=""
for pid in /proc/[0-9]*; do
  pid=${pid##*/}
  # Skip the init process and any process that doesn't have a stat file (e.g., kernel threads).
  [ "$pid" = "1" ] && continue
  [ -r "/proc/$pid/stat" ] || continue
  # Check if the process is a child of init (ppid=0) to avoid entering sshd or other user processes.
  ppid=$(awk '{print $4}' "/proc/$pid/stat")
  [ "$ppid" != "0" ] && continue
  # Check if the process is sshd to avoid entering it.
  comm=$(cat "/proc/$pid/comm" 2>/dev/null || echo "")
  [ "$comm" = "sshd" ] && continue
  # If we reach here, we have found a suitable target process.
  TARGET_PID=$pid
  break
done
if [ -z "$TARGET_PID" ]; then
  # If we couldn't find a suitable target process, just execute a shell in the current environment.
  echo "!!! Warning: can not find a suitable target process to switch !!!" >&2
  exec /bin/sh -l
fi

TARGET_ROOT="/proc/$TARGET_PID/root"

# Find a suitable shell in the target process's filesystem.
TARGET_SHELL=""
for shell in /bin/bash /usr/bin/bash /bin/zsh /usr/bin/zsh; do
  if [ -x "$TARGET_ROOT$shell" ]; then
    TARGET_SHELL="$shell"
    break
  fi
done
[ -z "$TARGET_SHELL" ] && TARGET_SHELL="/bin/sh"

# Print message.
cat /banner.txt >/dev/stderr

nsenter="$(which nsenter)"
chroot="$(which chroot)"
setpriv="$(which setpriv)"

# Prepare the environment for nsenter.
if [ -r "/proc/$TARGET_PID/environ" ]; then
  tmpfile=$(mktemp)
  tr '\0' '\n' <"/proc/$TARGET_PID/environ" | grep -Ev '^(KUBE|.*_POD_|.*_PORT|.*_ADDR=|.*_PROTO=|.*_HOST=)' >"$tmpfile"
  while IFS= read -r var; do
    export "$var"
  done <"$tmpfile"
  rm -f "$tmpfile"
fi
export TERM=xterm-256color
while IFS='=' read -r name _; do
  case "$name" in
  SSH_*) unset "$name" ;;
  KUBE_*) unset "$name" ;;
  SHELL) unset "$name" ;;
  *) ;;
  esac
done < <(env)

# Use nsenter to enter the target process's namespaces and chroot to its root filesystem.
exec "$nsenter" \
  --target "$TARGET_PID" \
  --mount --uts --ipc --net --pid \
  -- \
  "$chroot" "$TARGET_ROOT" \
  "$setpriv" --clear-groups "$TARGET_SHELL" -l
