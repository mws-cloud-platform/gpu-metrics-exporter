FROM ubuntu:22.04 AS builder

ARG GO_VERSION=1.25.8
# Injected into both binaries via -ldflags "-X main.version=…"; empty for dev builds.
ARG VERSION=""

# archive.ubuntu.com / security.ubuntu.com плохо доступны из РФ — используем зеркало Яндекса.
RUN sed -i \
    -e 's|http://archive.ubuntu.com/ubuntu|http://mirror.yandex.ru/ubuntu|g' \
    -e 's|http://security.ubuntu.com/ubuntu|http://mirror.yandex.ru/ubuntu|g' \
    -e 's|http://[a-z]*.archive.ubuntu.com/ubuntu|http://mirror.yandex.ru/ubuntu|g' \
    /etc/apt/sources.list

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        wget \
        gcc \
        make \
        libc6-dev \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tar.gz && \
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
