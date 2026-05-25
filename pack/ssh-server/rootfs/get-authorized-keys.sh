#!/bin/sh

set -e

if [ -f /var/run/sshd-authorized-keys/authorized_keys ]; then
  cat /var/run/sshd-authorized-keys/authorized_keys
fi
