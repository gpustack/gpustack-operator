#!/bin/sh

set -e

if [ -t 1 ]; then
  clear || true
fi

###############################################################################
# Target process detection
###############################################################################

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
if [ -z "${TARGET_PID:-}" ]; then
  echo "WARNING: unable to find a suitable target process" >&2
  exec /bin/sh -l
fi

TARGET_ROOT="/proc/$TARGET_PID/root"
CGROUP_DIR="$TARGET_ROOT/sys/fs/cgroup"

###############################################################################
# Shell detection
###############################################################################

# Find a suitable shell in the target process's filesystem.
TARGET_SHELL=""
for shell in /bin/bash /usr/bin/bash /bin/zsh /usr/bin/zsh; do
  if [ -x "$TARGET_ROOT$shell" ]; then
    TARGET_SHELL="$shell"
    break
  fi
done
[ -z "$TARGET_SHELL" ] && TARGET_SHELL="/bin/sh"

###############################################################################
# System information
###############################################################################

get_uptime_info() {
  if [ -r /proc/1/stat ]; then
    bt="$(awk '/btime/ {print $2}' /proc/stat)"
    st="$(awk '{print $22}' /proc/1/stat 2>/dev/null || echo 0)"
    hz="$(getconf CLK_TCK 2>/dev/null || echo 100)"
    now="$(date +%s)"

    awk -v st="$st" -v hz="$hz" -v bt="$bt" -v now="$now" '
      BEGIN {
        start_epoch = bt + (st / hz)
        uptime = now - start_epoch
        if (uptime < 0) uptime = 0

        days = int(uptime / 86400)
        uptime %= 86400
        hours = int(uptime / 3600)
        uptime %= 3600
        minutes = int(uptime / 60)
        uptime %= 60
        seconds = int(uptime)

        if (days > 0)
          if (days == 1)
            print "1 Day"
          else
            printf "%d Days\n", days
        else
          if (hours > 0)
            if (hours == 1)
              print "1 Hour"
            else
              printf "%d Hours\n", hours
          else
            if (minutes > 0)
              if (minutes == 1)
                print "1 Minute"
              else
                printf "%d Minutes\n", minutes
            else
              printf "%d Seconds\n", seconds
      }
    '
    return
  fi

  uptime 2>/dev/null \
    | sed -E 's/.*up ([^,]+),.*/\1/' \
    || echo 'N/A'
}

get_cpu_load_info() {
  # cgroup v2
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    cpu_max="$CGROUP_DIR/cpu.max"

    if [ -r "$cpu_max" ]; then
      read -r quota period <"$cpu_max"

      if [ "$quota" = "max" ]; then
        limit="$(nproc 2>/dev/null || echo 1)"
      else
        limit="$(awk -v q="$quota" -v p="$period" \
          'BEGIN {
            if (p == 0) { print 0; exit }
            r = q / p
            if (r < 0) { print 0; exit }
            print int(r) == r ? int(r) : int(r) + 1
          }')"
      fi

      echo "${limit} Cores"
      return
    fi
  fi

  # cgroup v1
  if [ -r "$CGROUP_DIR/cpu.cfs_quota_us" ]; then
    quota="$(cat "$CGROUP_DIR/cpu.cfs_quota_us" 2>/dev/null || echo "-1")"
    period="$(cat "$CGROUP_DIR/cpu.cfs_period_us" 2>/dev/null || echo "100000")"

    if [ "$quota" -lt 0 ] 2>/dev/null; then
      limit="$(nproc 2>/dev/null || echo 1)"
    else
      limit="$(awk -v q="$quota" -v p="$period" \
        'BEGIN {
          if (p == 0) { print 0; exit }
          r = q / p
          if (r < 0) { print 0; exit }
          print int(r) == r ? int(r) : int(r) + 1
        }')"
    fi

    echo "${limit} Cores"
    return
  fi

  echo "N/A"
}

get_memory_info() {
  # cgroup v2
  if [ -r "$CGROUP_DIR/memory.current" ]; then
    current="$(cat "$CGROUP_DIR/memory.current" 2>/dev/null || echo "0")"
    max="$(cat "$CGROUP_DIR/memory.max" 2>/dev/null || echo "max")"

    if [ "$max" = "max" ]; then
      max="$(awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo)"
    fi

    awk -v cur="$current" -v max="$max" '
      BEGIN {
        pct = (max > 0) ? (cur / max * 100) : 0
        printf "%.0f / %.0f MB (%.1f%%)",
          cur / 1024 / 1024,
          max / 1024 / 1024,
          pct
      }
    '
    return
  fi

  # cgroup v1
  if [ -r "$CGROUP_DIR/memory.usage_in_bytes" ]; then
    current="$(cat "$CGROUP_DIR/memory.usage_in_bytes" 2>/dev/null || echo "0")"
    max="$(cat "$CGROUP_DIR/memory.limit_in_bytes" 2>/dev/null || echo "0")"

    if [ "$max" -ge 9223372036850000000 ] 2>/dev/null; then
      max="$(awk '/MemTotal/ {print $2 * 1024}' /proc/meminfo)"
    fi

    awk -v cur="$current" -v max="$max" '
      BEGIN {
        pct = (max > 0) ? (cur / max * 100) : 0
        printf "%.0f / %.0f MB (%.1f%%)",
          cur / 1024 / 1024,
          max / 1024 / 1024,
          pct
      }
    '
    return
  fi

  echo "N/A"
}

get_process_info() {
  if [ -r "$CGROUP_DIR/cgroup.procs" ]; then
    wc -l <"$CGROUP_DIR/cgroup.procs" 2>/dev/null | tr -d ' '
    return
  fi

  ret="$(nsenter \
    --target "$TARGET_PID" \
    --pid \
    -- \
    sh -c 'find /proc -maxdepth 1 -type d -name "[0-9]*" | wc -l' \
    2>/dev/null \
    | tr -d ' '
  )"

  [ -n "$ret" ] && echo "$ret" || echo "N/A"
}

get_user_info() {
  ret="$(ps -eo args 2>/dev/null \
    | awk '
        /sshd-session:/ && /@pts\// {
          count++
        }
        END {
          print count + 0
        }
      '
  )"

  [ -n "$ret" ] && echo "$ret" || echo "N/A"
}

get_disk_info() {
  VOLUME_MOUNT_PATH="${VOLUME_MOUNT_PATH:-/workspace}"
  if [ -e "$TARGET_ROOT/$VOLUME_MOUNT_PATH" ]; then
    ret="$(nsenter \
      --target "$TARGET_PID" \
      --mount \
      -- \
      df -P "$VOLUME_MOUNT_PATH" 2>/dev/null \
      | awk 'NR==2 && NF>=6 {print $3, $2, $5}'
    )"
    if [ -n "$ret" ]; then
      current="$(echo "$ret" | awk '{print $1}')"
      max="$(echo "$ret" | awk '{print $2}')"
      pct="$(echo "$ret" | awk '{print $3}' | tr -d '%')"
      ret="$(awk -v cur="$current" -v max="$max" -v pct="$pct" '
          BEGIN {
            printf "%.0f / %.0f GB (%.1f%%)",
              cur / 1024 / 1024,
              max / 1024 / 1024,
              pct
          }
        '
      )"
      echo "$ret [$VOLUME_MOUNT_PATH]"
      return
    fi
    echo "N/A [$VOLUME_MOUNT_PATH]"
    return
  fi

  echo "N/A"
}

get_ip_info() {
  nsenter \
    --target "$TARGET_PID" \
    --net \
    -- \
    ip -o -4 addr show scope global 2>/dev/null \
    | awk '{print $4}' \
    | cut -d/ -f1
}

###############################################################################
# Banner
###############################################################################

print_banner() {
  [ -f /banner.txt ] && cat /banner.txt >&2

  uptime_info="$(get_uptime_info)"
  cpu_info="$(get_cpu_load_info)"
  memory_info="$(get_memory_info)"
  process_info="$(get_process_info)"
  user_info="$(get_user_info)"
  disk_info="$(get_disk_info)"
  ip_info="$(get_ip_info)"

  if [ -r "$TARGET_ROOT/etc/os-release" ]; then
    # Parse PRETTY_NAME as data — never `.`/source it: os-release lives in the target
    # (user-controlled) image, and sourcing would execute arbitrary shell here in the
    # sidecar while it still holds SYS_ADMIN/SYS_PTRACE, before the shell's caps are
    # dropped below. awk reads the value only; echo prints it without re-evaluation.
    pretty_name="$(awk -F= '$1=="PRETTY_NAME"{sub(/^"/,"",$2);sub(/"$/,"",$2);print $2;exit}' \
      "$TARGET_ROOT/etc/os-release" 2>/dev/null)"
    if [ -n "$pretty_name" ]; then
      echo "Welcome to ${pretty_name}" >&2
    fi
    echo >&2
  fi

  echo "System information as of $(date)" >&2
  echo >&2

  echo "  * Uptime:          $uptime_info" >&2
  echo "  * CPU:             $cpu_info" >&2
  echo "  * Memory:          $memory_info" >&2
  echo "  * Processes:       $process_info" >&2
  echo "  * Users Online:    $user_info" >&2
  echo "  * Workspace:       $disk_info" >&2
  if [ -n "$ip_info" ]; then
    echo "  * IP:              $(echo "$ip_info" | head -n1)" >&2
    ip_count="$(echo "$ip_info" | wc -l | tr -d ' ')"
    if [ "$ip_count" -gt 1 ]; then
      echo "$ip_info" | tail -n +2 | while read -r ip; do
        echo "                     ${ip}" >&2
      done
    fi
    echo >&2
  fi
}

###############################################################################
# Environment preparation
###############################################################################

prepare_environment() {
  if [ -r "/proc/$TARGET_PID/environ" ]; then
    tmpfile="$(mktemp)"

    tr '\0' '\n' <"/proc/$TARGET_PID/environ" \
      | grep -Ev '^(KUBE|.*_POD_|.*_PORT|.*_ADDR=|.*_PROTO=|.*_HOST=)' \
      >"$tmpfile" || true

    while IFS= read -r var; do
      export "$var"
    done <"$tmpfile"

    rm -f "$tmpfile"
  fi

  export TERM=xterm-256color

  env | while IFS='=' read -r name _; do
    case "$name" in
      SSH_*|KUBE_*|SHELL)
        unset "$name"
        ;;
    esac
  done
}

###############################################################################
# Main
###############################################################################

print_banner
prepare_environment

nsenter_bin="$(command -v nsenter)"
chroot_bin="$(command -v chroot)"
setpriv_bin="$(command -v setpriv)"

# Enter the target namespaces (nsenter needs the sidecar's caps), then drop all
# capabilities before handing control to the interactive shell: --bounding-set=-all
# prevents any capability from ever being regained and --inh-caps=-all clears the
# inheritable set, so the login shell runs with an empty effective set. Device
# access and GPU slicing still work (they come from the device-cgroup grant, not a
# capability), while host escapes such as mknod are denied.
exec "$nsenter_bin" \
  --target "$TARGET_PID" \
  --mount \
  --uts \
  --ipc \
  --net \
  --pid \
  -- \
  "$chroot_bin" "$TARGET_ROOT" \
  "$setpriv_bin" --clear-groups \
    --bounding-set=-all --inh-caps=-all \
  "$TARGET_SHELL" -l
