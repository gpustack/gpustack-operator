ARG GO_VERSION="1.26.4"
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS sftp-server

# Build a fully static SFTP server. The sidecar is musl/Alpine, but `main` is often
# glibc; a musl sftp-server cannot exec inside main's glibc chroot, and OpenSSH's
# internal-sftp would serve the sidecar's files, not main's. So the server is staged
# into main and run there, which means it must be statically linked (CGO off) to run
# under either libc. It speaks SFTP over stdin/stdout, exactly as sshd invokes a
# Subsystem sftp binary.
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN <<EOF
    set -ex
    go mod init gpustack-sftp-server
    cat >main.go <<'GO'
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/pkg/sftp"
)

func main() {
	rwc := struct {
		io.Reader
		io.WriteCloser
	}{os.Stdin, os.Stdout}

	server, err := sftp.NewServer(rwc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gpustack-sftp-server: init sftp server:", err)
		os.Exit(1)
	}
	if err := server.Serve(); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "gpustack-sftp-server: serve:", err)
		os.Exit(1)
	}
}
GO
    go get github.com/pkg/sftp@v1.13.7
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" CGO_ENABLED=0 \
        go build -trimpath -ldflags='-s -w' -o /gpustack-sftp-server .
EOF

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

COPY --from=sftp-server /gpustack-sftp-server /usr/local/bin/gpustack-sftp-server

EXPOSE 22
WORKDIR /
COPY pack/ssh-server/rootfs/ /
ENTRYPOINT ["/entrypoint.sh"]
