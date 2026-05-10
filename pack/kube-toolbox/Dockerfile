FROM alpine/k8s:1.35.3
SHELL ["/bin/sh", "-c"]

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

COPY --from=gerritk/ytt:0.54.0 /usr/bin/ytt /usr/bin/ytt

WORKDIR /
ENTRYPOINT ["/bin/sh"]
