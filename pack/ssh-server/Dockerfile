FROM alpine:3.23.4
SHELL ["/bin/sh", "-c"]

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

## Install OpenSSH Server

RUN <<EOF
    # OpenSSH Server
    set -exo pipefail
    apk add --no-cache openssh util-linux

    # Generate SSH host keys
    ssh-keygen -A

    # Configure SSHD
    echo "ListenAddress 0.0.0.0" >> /etc/ssh/sshd_config
    echo "PasswordAuthentication no" >> /etc/ssh/sshd_config
    echo "PermitRootLogin yes" >> /etc/ssh/sshd_config
    echo "TCPKeepAlive yes" >> /etc/ssh/sshd_config
    echo "ClientAliveInterval 60" >> /etc/ssh/sshd_config
    echo "ClientAliveCountMax 3" >> /etc/ssh/sshd_config
    echo "UseDNS no" >> /etc/ssh/sshd_config
    echo "Banner no" >> /etc/ssh/sshd_config
    echo "ForceCommand /chroot.sh" >> /etc/ssh/sshd_config

    # Enable TCP and stream-local forwarding and pin the sftp subsystem to a known
    # path. Alpine ships an active `AllowTcpForwarding no` and `Subsystem sftp
    # internal-sftp`; sshd honours the first value of a keyword and rejects a duplicate
    # Subsystem, so drop the shipped active lines before writing ours.
    sed -i -E '/^[[:space:]]*(AllowTcpForwarding|AllowStreamLocalForwarding|Subsystem[[:space:]]+sftp)[[:space:]]/d' /etc/ssh/sshd_config
    echo "AllowTcpForwarding yes" >> /etc/ssh/sshd_config
    echo "AllowStreamLocalForwarding yes" >> /etc/ssh/sshd_config
    echo "Subsystem sftp /usr/lib/ssh/sftp-server" >> /etc/ssh/sshd_config

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/*
EOF

EXPOSE 22
WORKDIR /
COPY pack/ssh-server/rootfs/ /
ENTRYPOINT ["/entrypoint.sh"]
