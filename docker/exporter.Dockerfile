# syntax=docker/dockerfile:1
FROM ubuntu:22.04 AS builder

ARG GO_VERSION=1.25.8
ARG GO_SHA256=ceb5e041bbc3893846bd1614d76cb4681c91dadee579426cf21a63f2d7e03be6
ARG VERSION=""
ARG APT_MIRROR=""

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

RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tar.gz && \
    echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz

ENV PATH="/usr/local/go/bin:${PATH}"

WORKDIR /src
COPY go.mod go.sum ./
COPY internal/ internal/
COPY pkg/ pkg/
COPY cmd/ cmd/

RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -ldflags "-X main.version=${VERSION} -s -w" -o /gpu-metrics-exporter ./cmd/gpu-metrics-exporter/

FROM ubuntu:22.04

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        util-linux && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /gpu-metrics-exporter /usr/local/bin/gpu-metrics-exporter

ENTRYPOINT ["/usr/local/bin/gpu-metrics-exporter"]
