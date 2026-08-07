# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

GPU metrics exporter ships NVIDIA GPU metrics from inside a guest VM to the host over **AF_VSOCK** sockets, for observability of MWS GPU VMs. It produces two Linux/amd64 binaries:

- **`gpu-metrics-exporter`** (guest) — a vsock *client* that periodically queries GPU metrics via [go-nvml](https://github.com/NVIDIA/go-nvml) (and `nvidia-smi`/`dmesg`/`systemctl` shelling-out for a few things) and sends them to the host.
- **`gpu-metrics-receiver`** (host) — a vsock *server* that accepts exporter connections, decodes metrics, and hands them to an injected `GpuMeticsConsumer`.

Transport is raw AF_VSOCK (`golang.org/x/sys/unix`), *not* `net.Dial`. The host is always CID 2; each guest VM has a unique CID > 2. The receiver reads the peer CID (`metrics.Source.VsockClientID`) to identify the VM — downstream code is expected to map CID → VMID.

## Commands

```bash
make docker-build     # build both linux/amd64 binaries via Docker into bin/ (primary build path)
make deb              # docker-build both binaries, then build .deb packages into dist/ (needs VERSION)
make docker-test      # run go test ./... inside the build container (linux/amd64)
make gpu-metrics-exporter      # native-ish build of exporter only (CGO_ENABLED=1, needs a C toolchain)
make gpu-metrics-receiver   # build receiver only (netgo/osusergo, no CGO needed)
make test            # go test -v ./...            (host is darwin — see note below)
make vet             # go vet ./...
make fmt             # gofmt -w .
make clean
```

Single test: `go test -v -run TestCompressDecompressRoundTrip ./pkg/gpumetrics/`

**Build environment caveat:** the dev host is macOS (darwin), but the binaries target `GOOS=linux GOARCH=amd64`. The exporter requires `CGO_ENABLED=1` because go-nvml cgo-links to `libnvidia-ml`. Cross-compiling the exporter from macOS requires a Linux/amd64 C cross-toolchain; in practice use `make docker-build` (or `docker-test`). Native `make test` on darwin works for the pure-Go `pkg/gpumetrics` round-trip test but the build tags/no-libnvml situation means tests that touch NVML only run meaningfully on Linux. Prefer `make docker-test` for CI-equivalent runs.

CI lives in `.github/workflows/build.yml` (GitHub Actions). On every branch push / pull request it runs the `binaries` job (`make docker-build`) and uploads the linux/amd64 binaries as a workflow artifact — nothing is published. On a tag push (tags look like `2026.06.08-2`) it runs the `debs` job: `make deb VERSION=<tag>` builds both `.deb`s into `dist/` and attaches them to a GitHub Release. The tag name is the package/binaries version. Both binaries expose a `version` string var set via ldflags (`-X main.version=…`, injected through the Dockerfile `VERSION` build-arg) and a `-version` flag. Module requires Go 1.25.0 (`go.mod`).

## Architecture

### Two-goroutine pipeline in the exporter (`pkg/gpumetricsexporter`)
`exporter.go` runs `queryMetricsLoop` (ticker-driven, every `TickPeriod` seconds, default 10) and `sendMetricsLoop` (separate goroutine, `wg`-tracked), connected by a buffered `metricsQueue` channel (cap 32). Decoupling query from send means a slow/blocked vsock send doesn't stall NVML collection. Stop is via two `stop*Chan` signaled from context (driven by SIGINT/SIGTERM in `main.go`). The query loop blocks on `metricsQueue <- m`; the send loop drains it.

### Per-tick collection (`query_metrics.go`)
`queryMetrics()` builds one `gpumetrics.GpuMetrics` per tick: reads instance ID from `/var/lib/cloud/data/instance-id` (cloud-init), increments an atomic `seqno`, then for each GPU index runs `collectGPUInfo` — a sequence of ~20 `collect*` methods each pulling one NVML facet and silently no-oping on non-`nvml.SUCCESS` (unsupported features don't fail the whole tick). Also shells out to `systemctl` (nvidia-fabricmanager health) and `dmesg` (XID/SXID errors within the last `TickPeriod+10` sec).

**Delta tracking:** the exporter keeps `lastMetrics` (RWMutex-guarded) and computes `*Delta` fields for ECC errors, retired pages, row remapping, and NVLink error counters via `getValueDelta` (monotonic: returns 0 if new ≤ old). `findLastGpuInfo(gpuID)` matches GPUs across ticks by UUID. When adding a new cumulative counter, follow this same last-vs-current delta pattern.

### Wire format (`pkg/gpumetrics` + `pkg/vsock/common`)
`GpuMetrics.ToBytes()` → JSON → gzip. Over vsock each message is framed by `VsockFrameHeader{Magic 0xBEADBEAF, Len, xxhash64}` with a 64 KiB max payload (`SendData`/`RecvData` in `pkg/vsock/common/common.go`). An empty (Len=0) frame is the exporter's "I'm done, close" signal — the receiver treats `ErrNoData` as a clean end-of-stream and breaks out of the read loop. `metrics_test.go` only covers the gzip/JSON round-trip.

### Vsock I/O (`pkg/vsock`)
`server.VsockListener` creates a non-blocking AF_VSOCK SOCK_STREAM socket bound to `VMADDR_CID_ANY`, and `Accept` polls (100 ms timeout) so `ctx.Done()` / `Close()` can interrupt it — the receiver uses context cancellation + `Close()` for shutdown. `client.NewClientConnection` connects to `VMADDR_CID_HOST` (CID 2). The exporter opens a **fresh connection per send** (connect → SendData → send empty close-frame → Close); there is no persistent connection.

### Receiver lifecycle (`pkg/gpumetricsreceiver`)
`Run(ctx)` accepts connections in a loop, spawns `handleConnection` per connection (tracked in a `connections` map + `wg`), and decodes each frame into `GpuMetrics`, stamping `Source.VsockClientID = conn.RemoteAddr().CID` before calling the consumer. It rejects CID < 3 (host/well-known). Shutdown is coordinated through an `atomic.Int32 stopping` flag plus `shutdownOnContextDone`; the consumer interface is `GpuMeticsConsumer.OnGpuMetricsReceived(*GpuMetrics) error`. The default consumer in `cmd/gpu-metrics-receiver/main.go` just logs — real consumers get plugged in here.

## Conventions

- Logging is uber zap, production config, RFC3339Nano timestamps, JSON to stderr, no caller/stacktrace. Each component tags logs with `zap.String("component", "gpu-metrics-exporter"|-receiver|-consumer)`.
- Deployment is via `.deb` packages built by `scripts/build-deb.sh` from `debian/<pkg>/DEBIAN/` templates (`control` + `postinst`/`prerm`/`postrm`) plus the binary and systemd unit — `make deb` drives it. The systemd units use **port 9999** and **tickPeriod 60** for the exporter — note these differ from the CLI defaults (port 1234, tick 10) in `main.go`.
- Module path: `go.mws.cloud/gpu-metrics-exporter`. Internal imports use the full `github.com/mws-cloud-platform/...` path.
- The exported consumer interface is spelled `GpuMeticsConsumer` (missing the `r` in "Metrics") — this is intentional/legacy, not a typo to fix casually: it's a public API name and renaming it is a breaking change for out-of-tree consumers.
- `make deb` requires Docker (it depends on `docker-build`); `VERSION` may be empty, in which case `scripts/build-deb.sh` falls back to `git describe --tags --always` (then `0.0.0-dev`).
