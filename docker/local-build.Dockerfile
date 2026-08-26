FROM ubuntu:22.04 AS builder

ARG GO_VERSION=1.25.8
# sha256 of go${GO_VERSION}.linux-amd64.tar.gz from https://go.dev/dl/.
# Keep in lockstep with GO_VERSION above; a mismatch fails the build loudly.
ARG GO_SHA256=ceb5e041bbc3893846bd1614d76cb4681c91dadee579426cf21a63f2d7e03be6
# Injected into both binaries via -ldflags "-X main.version=…"; empty for dev builds.
ARG VERSION=""

# Optional apt mirror. archive.ubuntu.com / security.ubuntu.com are poorly
# reachable from some networks (Russia in particular), so the mirror is
# configurable rather than hard-coded: pass --build-arg APT_MIRROR="" to use
# Ubuntu's default archives, or point it at any mirror you prefer.
ARG APT_MIRROR="http://mirror.yandex.ru/ubuntu"
RUN if [ -n "$APT_MIRROR" ]; then \
        sed -i \
            -e "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR}|g" \
            -e "s|http://security.ubuntu.com/ubuntu|${APT_MIRROR}|g" \
            -e "s|http://[a-z]*.archive.ubuntu.com/ubuntu|${APT_MIRROR}|g" \
            /etc/apt/sources.list; \
    fi

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        wget \
        gcc \
        make \
        libc6-dev \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Verify the toolchain tarball against its published checksum before unpacking:
# without this the build trusts whatever the network returns. GO_SHA256 must be
# updated together with GO_VERSION — the checksums are published at
# https://go.dev/dl/ (and as go${GO_VERSION}.linux-amd64.tar.gz.sha256).
RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tar.gz && \
    echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz

ENV PATH="/usr/local/go/bin:${PATH}"

WORKDIR /src
COPY go.mod ./
COPY go.sum ./
COPY internal/ internal/
COPY pkg/ pkg/
COPY cmd/ cmd/

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -ldflags "-X main.version=${VERSION}" -o /gpu-metrics-exporter ./cmd/gpu-metrics-exporter/

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -ldflags "-X main.version=${VERSION}" -o /gpu-metrics-receiver ./cmd/gpu-metrics-receiver/


FROM ubuntu:22.04
COPY --from=builder /gpu-metrics-exporter /gpu-metrics-exporter
COPY --from=builder /gpu-metrics-receiver /gpu-metrics-receiver
