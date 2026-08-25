# GPU metrics exporter

## Goal
Install a GPU metrics exporter in the guest VM
and pass various GPU device metrics from the
guest VM to the host for observability and monitoring.

## How it works

### Guest -> Host transport
For transport between guest and host, we use
VSOCK sockets (AF_VSOCK) [doc](https://github.com/rust-vmm/vhost-device/blob/main/vhost-device-vsock/README.md).

To configure the VM, add a vsock device in the qemu command line:
```
-device vhost-vsock-pci,disable-legacy=on,guest-cid=3
```
where 3 (or another number >2) should be a unique guest VM ID (CID) on the host.

The host vsock socket always has `cid = 2`.

The host vsock specifies the port number on which it accepts incoming connections;
the guest side uses this port to connect to the host. The CLI default is port 1234;
the deployed systemd units (see `systemd/`) use port 9999.

So VSOCK provides socket-based client and server libraries;
see `pkg/vsock` code.

### GPU metrics exporter
The GPU metrics exporter is a guest binary that is a vsock client.
It knows the peer (some fixed port and host CID = 2).

The GPU metrics exporter periodically queries GPU metrics using [go-nvml](https://github.com/NVIDIA/go-nvml)
(with `nvidia-smi`/`dmesg`/`systemctl` shelling-out for a few things) and sends them to the host.

Source code location: `cmd/gpu-metrics-exporter` and `pkg/gpumetricsexporter`.

### GPU metrics receiver
The GPU metrics receiver is a host-side component that accepts
connections from VMs, reads GPU metrics, and passes them to a specified
GPU metrics collector (another component) for further processing.

Source code location: `cmd/gpu-metrics-receiver` and `pkg/gpumetricsreceiver`.

The GPU metrics collector should convert CID into VMID, for example by parsing
and searching in running qemu command lines.

Guests are less trusted than the host, so the receiver bounds what one can
consume. `GpuMetricsReceiverConfig` exposes three limits, each defaulted when
left at zero:

| Field | Default | Purpose |
| --- | --- | --- |
| `MaxConnections` | 64 | Total simultaneous guest connections |
| `MaxConnectionsPerCID` | 4 | Simultaneous connections from one VM |
| `ReadTimeout` | 60 s | Per-frame read deadline |

Connections over a limit are closed immediately. The read deadline is what stops
a guest from opening a connection, sending a frame header, and going silent to
pin a goroutine and an fd until the process exits.

## Exported metrics

On every tick (default 10 s; the deployed systemd unit uses 60 s) the exporter
collects one `GpuMetrics` payload and ships it to the host. The wire format is
JSON → gzip, framed over vsock (see `pkg/gpumetrics`). Field names below are the
JSON keys as they appear on the wire. Every per-GPU field is collected on a
best-effort basis: unsupported features silently no-op and are left zero-valued
rather than failing the tick.

Cumulative counters carry a `*_delta` field giving the change since the previous
tick (a counter reset/reboot yields 0). The baseline is the previous *collected*
snapshot, not the previous delivered one, which makes deltas additive: summing
`*_delta` across a run of payloads reconstructs the true increase even when
several backlog behind a slow send. The trade-off is that a payload which fails
to send takes its delta with it — that increase is not folded into the next
payload. Consumers that must not miss an increase should track the absolute
counter (`sbe_pages`, `correctable`, …), which every payload carries in full;
`*_delta` is a convenience for the common case.

### Payload envelope

| Field | Description |
| --- | --- |
| `source.vsock_client_id` | Guest VM vsock CID, filled in by the receiver from the peer address |
| `source.instance_id` | Cloud-init instance ID (read from `/var/lib/cloud/data/instance-id`) |
| `gpu_device_count` | Number of NVIDIA GPUs detected |

### Exporter health (`exporter_info`)

| Field | Description |
| --- | --- |
| `seqno` | Monotonically increasing tick sequence number |
| `timestamp` | Collection time, Unix UTC seconds |
| `version` | Exporter build version |
| `start_time` | Exporter process start time, Unix UTC seconds |
| `init_nvml_error` | Error from NVML initialization, empty on success |
| `get_device_count_error` | Error from `DeviceGetCount`, empty on success |
| `read_instance_id_error` | Error reading the instance ID, empty on success |
| `send_metrics_error_count` | Running total of send failures since start |
| `send_metrics_last_error` | Most recent send failure message |

### Fabric manager (`nv_fabric_manager_status`)

Health of the `nvidia-fabricmanager` service (relevant for multi-GPU NVLink
fabrics such as H100), queried via `systemctl`.

| Field | Description |
| --- | --- |
| `active` | `systemctl is-active` reports `active` |
| `enabled` | `systemctl is-enabled` reports `enabled` |
| `not_degraded` | `systemctl is-failed` reports `active` (i.e. not failed) |
| `error` | Error message if the check itself failed |

### XID / SXid errors (`xid_errors`)

Recent GPU error lines from the kernel log (`dmesg` within `tickPeriod + 10 s`),
filtered for `xid`/`sxid`.

A line is buffered as soon as it is observed and re-shipped on every payload
until a payload carrying it is confirmed delivered, so neither a transient send
failure nor a send stall longer than the look-back window can lose an error.
Once delivered, a line is remembered long enough that the overlapping window
cannot re-ship it as a duplicate. The pending buffer is capped; if it overflows
(only reachable when the host has been unreachable for a long time) the oldest
lines are discarded and counted in `dropped_count`, because an unbounded buffer
would eventually push the frame past the 64 KiB vsock limit and stop delivery
altogether.

| Field | Description |
| --- | --- |
| `xid_errors` | List of matching `dmesg` lines pending or newly delivered |
| `dropped_count` | Cumulative lines discarded on buffer overflow; non-zero means XID errors were lost and only the guest's kernel log still has them |
| `error` | Error message if the `dmesg` invocation failed |

### Per-GPU metrics (`gpu_info[]`)

One entry per detected GPU. `error` is set and the remaining fields are left
zero when the device handle cannot be opened.

**Identity & driver**

| Field | Description |
| --- | --- |
| `index` | GPU index in the system |
| `uuid` | GPU UUID |
| `name` | GPU product name |
| `serial` | GPU serial number |
| `architecture` | Architecture name (Kepler, Maxwell, … Hopper, Blackwell) |
| `driver_version` | NVIDIA driver version |
| `vbios_version` | VBIOS version |
| `driver_model` | Driver model (e.g. `WDDM`) |
| `error` | Per-GPU collection error, empty on success |

**Thermal & power**

| Field | Description |
| --- | --- |
| `temperature_celsius` | GPU temperature |
| `memory_temperature_celsius` | Memory temperature |
| `power_usage_watts` | Current power draw |
| `power_limit_watts` | Configured power limit |
| `fan_speed_percent` | Fan speed (0 for data-center GPUs without a fan) |

**Memory**

| Field | Description |
| --- | --- |
| `memory_total_bytes` | Total VRAM |
| `memory_used_bytes` | Used VRAM |
| `memory_free_bytes` | Free VRAM |
| `bar1_memory_total_bytes` | Total BAR1 memory |
| `bar1_memory_used_bytes` | Used BAR1 memory |
| `bar1_memory_free_bytes` | Free BAR1 memory |

**Utilization & clocks**

| Field | Description |
| --- | --- |
| `utilization.gpu_percent` | GPU utilization |
| `utilization.memory_percent` | Memory controller utilization |
| `decoder_utilization_percent` | Decoder utilization |
| `encoder_utilization_percent` | Encoder utilization |
| `clocks.graphics_mhz` | Graphics clock |
| `clocks.memory_mhz` | Memory clock |
| `clocks.sm_mhz` | SM clock |
| `fan_speed_percent` | Fan speed percentage |

**PCIe**

| Field | Description |
| --- | --- |
| `pci_info.bus_id` | PCI bus ID, formatted `domain:bus:device.function` |
| `pci_info.domain`, `bus`, `device` | PCI location components |
| `pci_info.pci_generation` | Current PCIe link generation |
| `pci_info.link_width_current` | Current PCIe link width |
| `pci_info.max_pci_generation` | Maximum supported PCIe generation |
| `pci_info.max_link_width` | Maximum supported link width |
| `pci_info.tx_throughput` | PCIe TX throughput (kB/s; can be 0 when idle) |
| `pci_info.rx_throughput` | PCIe RX throughput (kB/s; can be 0 when idle) |

**Compute mode & performance state**

| Field | Description |
| --- | --- |
| `compute_mode` | Compute mode label (`Default`, `Exclusive Process`, …) |
| `compute_mode_value` | Numeric compute mode |
| `performance_state` | Performance state label (`P0`…`P15`) |
| `performance_state_value` | Numeric performance state |
| `persistence_mode` | Persistence mode (0/1) |

**MIG (Multi-Instance GPU)**

| Field | Description |
| --- | --- |
| `mig_info.supported` | NVML reports MIG as available on this GPU (false on pre-Ampere cards) |
| `mig_info.enabled` | MIG mode currently in force |
| `mig_info.pending_enabled` | MIG mode NVML will apply after the next GPU reset |
| `mig_info.pending_change` | `enabled` and `pending_enabled` disagree — the configured partitioning is not the running one, and a GPU reset is required |
| `mig_info.instance_count` | Number of instantiated MIG devices |
| `mig_info.instances[]` | One entry per MIG device (see below) |
| `mig_info.error` | Error message if the MIG query itself failed |

Each `mig_info.instances[]` entry:

| Field | Description |
| --- | --- |
| `index` | NVML MIG device index on the parent GPU. Sparse: a partitioning leaves gaps, so indices are not consecutive |
| `uuid` | MIG device UUID (`MIG-…`) |
| `name` | Instance product name, e.g. `NVIDIA A100-SXM4-40GB MIG 3g.20gb` |
| `gpu_instance_id` / `compute_instance_id` | Identifiers NVML and `nvidia-smi` use to name the partition |
| `memory_total_bytes` / `memory_used_bytes` / `memory_free_bytes` | Memory scoped to this instance |
| `multiprocessor_count` | SMs assigned to the instance |
| `gpu_instance_slice_count` / `compute_instance_slice_count` | Partition size in the card's slice units (e.g. 3 of 7) |
| `error` | Error message if this specific instance could not be read |

> Note: when MIG is enabled, NVML answers many whole-device queries with
> `NOT_SUPPORTED` — utilization, per-device volatile ECC counters, clocks. Those
> fields are then reported as `0`, following the same best-effort rule as any
> unsupported feature. Check `mig_info.enabled` before reading a zero as a
> genuinely idle GPU.
| `cuda_compute_capability.major` / `.minor` | CUDA compute capability |

**ECC (`ecc_info`)** — populated when ECC is enabled.

| Field | Description |
| --- | --- |
| `enabled` | ECC currently enabled |
| `pending` | ECC enable pending a reboot |
| `mode` | `Enabled` / `Disabled` |
| `dram_errors`, `sram_errors` | Counters per memory location, each split into `volatile` (since reset) and `aggregate` (lifetime) |
| `*.volatile.correctable` / `uncorrectable` | Cumulative ECC error counts |
| `*.volatile.correctable_delta` / `uncorrectable_delta` | Per-tick change |
| `*.aggregate.*` | Same fields for the lifetime scope |
| `*.error` | Error message if a counter read failed |
| `retired_pages.sbe_pages` / `dbe_pages` | Retired pages (single-bit / double-bit errors) |
| `retired_pages.pending_pages` | Pages pending retirement |
| `retired_pages.*_delta` | Per-tick change for each retired-page counter |
| `retired_pages.error` | Error message |

**Row remapping (`row_remapping_info`)**

| Field | Description |
| --- | --- |
| `pending` | Remapping pending |
| `failed` | Remapping failed |
| `correctable` / `uncorrectable` | Cumulative remapped rows |
| `correctable_delta` / `uncorrectable_delta` | Per-tick change |
| `error` | Error message |

**Clocks throttling (`clocks_throttle_info`)**

| Field | Description |
| --- | --- |
| `throttle_reasons` | Bitmask of active throttle reasons |
| `throttle_reasons_string` | Human-readable throttle reasons (`None` if idle) |
| `event_reasons` | Bitmask of clock event reasons |
| `event_reasons_string` | Human-readable event reasons |

**NVLink (`nvlink_info.links[]`)** — one entry per NVLink link index.

| Field | Description |
| --- | --- |
| `index` | Link index |
| `state` | Link state (`Active`, `Inactive`, `Sleep`, `Unknown`) |
| `errors` | Map of NVML error-counter type → cumulative count |
| `errors_delta` | Map of error-counter type → per-tick change |
| `error` | Error message |

## How to build
```bash
make docker-build
```
This produces two Linux amd64 binaries: `bin/gpu-metrics-exporter` and `bin/gpu-metrics-receiver`.

To build `.deb` packages (requires the binaries first, or just run `make deb` which builds them):
```bash
make deb VERSION=2026.06.08-2
```
This writes `dist/gpu-metrics-exporter_<version>_amd64.deb` and `dist/gpu-metrics-receiver_<version>_amd64.deb`. If `VERSION` is omitted, it falls back to `git describe --tags --always`.

CI (`.github/workflows/build.yml`) runs the test suite (`make docker-test`, in a linux/amd64 container), then builds the binaries on every commit / pull request (uploaded as a workflow artifact, not published), and runs the tests + builds + publishes the `.deb` packages to a GitHub Release on a tag push, using the tag name as the version.

## License
This project is licensed under the [Apache License 2.0](LICENSE).

Copyright 2026 MWS Cloud Platform. See [NOTICE](NOTICE) for details.

### Third-party dependencies
This project redistributes or depends on third-party software under their own licenses:

| Dependency | License |
| --- | --- |
| [github.com/NVIDIA/go-nvml](https://github.com/NVIDIA/go-nvml) | Apache License 2.0 |
| [go.uber.org/zap](https://github.com/uber-go/zap) | MIT |
| [go.uber.org/multierr](https://github.com/uber-go/multierr) | MIT |
| [github.com/cespare/xxhash/v2](https://github.com/cespare/xxhash) | MIT |
| [golang.org/x/sys](https://go.googlesource.com/sys) | BSD-3-Clause |
| [github.com/stretchr/testify](https://github.com/stretchr/testify) | MIT |
| [github.com/davecgh/go-spew](https://github.com/davecgh/go-spew) | ISC |
| [github.com/pmezard/go-difflib](https://github.com/pmezard/go-difflib) | BSD-3-Clause |
