#!/bin/sh

set -e

if [ -n "$SSH_PUBLIC_KEY" ]; then
  if ! [ -f /root/.ssh/authorized_keys ]; then
    mkdir -p /root/.ssh
    echo "$SSH_PUBLIC_KEY" >/root/.ssh/authorized_keys
    chmod 600 /root/.ssh/authorized_keys
  else
    echo "!!! Warning: /root/.ssh/authorized_keys already exists, ignore SSH_PUBLIC_KEY env !!!" >&2
  fi
fi

exec /usr/sbin/sshd -D -e
