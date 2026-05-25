#!/bin/sh

set -e

if [ -f /var/run/sshd-authorized-keys/authorized_keys ]; then
  echo "AuthorizedKeysCommand /get-authorized-keys.sh" >> /etc/ssh/sshd_config
  echo "AuthorizedKeysCommandUser root" >> /etc/ssh/sshd_config
else
  if [ -n "$SSH_PUBLIC_KEY" ]; then
    if ! [ -f /root/.ssh/authorized_keys ]; then
      mkdir -p /root/.ssh
      echo "$SSH_PUBLIC_KEY" >/root/.ssh/authorized_keys
      chmod 600 /root/.ssh/authorized_keys
    else
      echo "!!! Warning: /root/.ssh/authorized_keys already exists, ignore SSH_PUBLIC_KEY env !!!" >&2
    fi
  fi
fi

exec /usr/sbin/sshd -D -e
