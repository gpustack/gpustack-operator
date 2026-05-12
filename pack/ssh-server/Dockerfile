FROM alpine:3.23.4
SHELL ["/bin/sh", "-c"]

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

## Install OpenSSH Server

RUN <<EOF
    # OpenSSH Server
    set -exo pipefail
    apk add --no-cache openssh util-linux linux-pam

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
    echo "UsePAM yes" >> /etc/ssh/sshd_config
    echo "Banner no" >> /etc/ssh/sshd_config
    echo "ForceCommand /chroot.sh" >> /etc/ssh/sshd_config

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/*
EOF

EXPOSE 22
WORKDIR /
COPY pack/ssh-server/rootfs/ /
ENTRYPOINT ["/entrypoint.sh"]
