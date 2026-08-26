package gpumetrics

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"

	v1 "go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics/v1"
)

// CurrentWireVersion is the format version this build's exporter writes.
//
// Bump it only for a change old receivers cannot absorb: a renamed or retyped
// json key, changed units, or restructured nesting. Purely additive fields do
// not need a bump — an old receiver ignores what it does not know, and a new
// receiver reads the absent field as zero.
//
// Version history:
//
//	v1 — tag 2026.07.29-1. Predates this field; payloads carry no wire_version
//	     at all, which is why absent is read as 1. Spells the PCIe throughput
//	     keys tx_throughtput/rx_throughtput.
//	v2 — renames those to tx_throughput/rx_throughput (the break that forces
//	     this versioning), and adds gpu_info[].mig_info and
//	     xid_errors.dropped_count.
const CurrentWireVersion = 2

// wireEnvelope is decoded first, to learn which full struct to decode into.
// It must never gain a field that is not present in every version.
type wireEnvelope struct {
	WireVersion int `json:"wire_version"`
}

// ToBytes serializes the metrics to the on-the-wire representation: JSON
// followed by gzip compression. The returned bytes are the payload of a vsock
// frame (see pkg/vsock/common).
func (m *GpuMetrics) ToBytes() ([]byte, error) {
	return compressJSON(m)
}

// NewGpuMetricsFromBytes decodes a wire payload (gzip-compressed JSON) into the
// *current* GpuMetrics shape, upconverting older formats on the way.
//
// The exporter runs inside a guest VM whose owner has root, so old exporters
// can stay in the field indefinitely and the receiver has to keep reading every
// version it ever shipped. The returned struct is always the current shape;
// its WireVersion records what the exporter actually spoke, so a consumer can
// tell "the exporter reported zero" from "the exporter was too old to report".
func NewGpuMetricsFromBytes(data []byte) (*GpuMetrics, error) {
	raw, err := gunzip(data)
	if err != nil {
		return nil, err
	}

	// Two passes over the same decompressed bytes: once for the version, once
	// for the payload. Cheaper and simpler than gunzipping twice.
	var env wireEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode wire envelope: %w", err)
	}

	switch {
	case env.WireVersion <= 1:
		// Absent (0) means a pre-versioning exporter, i.e. v1 by definition.
		var m1 v1.GpuMetrics
		if err := json.Unmarshal(raw, &m1); err != nil {
			return nil, fmt.Errorf("decode v1 payload: %w", err)
		}
		return upconvertV1(&m1), nil

	case env.WireVersion == CurrentWireVersion:
		m := &GpuMetrics{}
		if err := json.Unmarshal(raw, m); err != nil {
			return nil, fmt.Errorf("decode v%d payload: %w", env.WireVersion, err)
		}
		return m, nil

	default:
		// A newer exporter than this receiver. Rejecting would drop the metrics
		// of a VM that is probably fine, and within a major version changes are
		// additive, so decode optimistically as the current shape. WireVersion
		// keeps the real value so the caller can see it was decoded by a
		// receiver that predates the format.
		m := &GpuMetrics{}
		if err := json.Unmarshal(raw, m); err != nil {
			return nil, fmt.Errorf("decode future v%d payload as v%d: %w",
				env.WireVersion, CurrentWireVersion, err)
		}
		m.WireVersion = env.WireVersion
		return m, nil
	}
}

// gunzip decompresses a wire payload into raw JSON bytes.
func gunzip(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// upconvertV1 maps a v1 payload into the current shape.
//
// Fields v1 has no notion of (MIG, XIDErrors.DroppedCount) are left zero, and
// WireVersion stays 1 to say so: those zeros mean "not reported by this
// exporter", not "reported as zero".
func upconvertV1(m1 *v1.GpuMetrics) *GpuMetrics {
	m := &GpuMetrics{
		WireVersion: 1,
		Source: GpuMetricsSource{
			VsockClientID: m1.Source.VsockClientID,
			InstanceID:    m1.Source.InstanceID,
		},
		ExporterInfo:          ExporterInfo(m1.ExporterInfo),
		NvFabricManagerStatus: NvFabricManagerStatus(m1.NvFabricManagerStatus),
		XIDErrors: XIDErrors{
			Error:     m1.XIDErrors.Error,
			XIDErrors: m1.XIDErrors.XIDErrors,
			// DroppedCount: v1 had no overflow buffer, so it cannot have lost
			// lines this way. Zero is accurate here rather than merely unknown.
		},
		GpuDeviceCount: m1.GpuDeviceCount,
	}

	if m1.Gpus != nil {
		m.Gpus = make([]GPUInfo, 0, len(m1.Gpus))
		for i := range m1.Gpus {
			m.Gpus = append(m.Gpus, upconvertV1GPUInfo(&m1.Gpus[i]))
		}
	}

	return m
}

func upconvertV1GPUInfo(g1 *v1.GPUInfo) GPUInfo {
	g := GPUInfo{
		Index:                 g1.Index,
		Error:                 g1.Error,
		UUID:                  g1.UUID,
		Name:                  g1.Name,
		Serial:                g1.Serial,
		Temperature:           g1.Temperature,
		MemoryTemp:            g1.MemoryTemp,
		PowerUsage:            g1.PowerUsage,
		PowerLimit:            g1.PowerLimit,
		MemoryTotal:           g1.MemoryTotal,
		MemoryUsed:            g1.MemoryUsed,
		MemoryFree:            g1.MemoryFree,
		BAR1MemoryTotal:       g1.BAR1MemoryTotal,
		BAR1MemoryUsed:        g1.BAR1MemoryUsed,
		BAR1MemoryFree:        g1.BAR1MemoryFree,
		Utilization:           GPUUtilization(g1.Utilization),
		DecoderUtil:           g1.DecoderUtil,
		EncoderUtil:           g1.EncoderUtil,
		Clocks:                GPUClocks(g1.Clocks),
		FanSpeed:              g1.FanSpeed,
		DriverModel:           g1.DriverModel,
		DriverVersion:         g1.DriverVersion,
		VBios:                 g1.VBios,
		ComputeMode:           g1.ComputeMode,
		ComputeModeValue:      g1.ComputeModeValue,
		PerformanceState:      g1.PerformanceState,
		PerformanceStateValue: g1.PerformanceStateValue,
		ClocksThrottle:        ClocksThrottleInfo(g1.ClocksThrottle),
		Architecture:          g1.Architecture,
		CUDAComputeCapability: CUDAComputeCapability(g1.CUDAComputeCapability),
		PersistenceMode:       g1.PersistenceMode,
		// MIG: v1 exporters never queried it. Left zero; WireVersion says why.
	}

	// The rename this whole mechanism exists for: v1 wrote the PCIe throughput
	// under the misspelled keys, so read them from the v1 fields explicitly.
	// Copying PCIInfo wholesale would compile — the Go field names match — but
	// that is exactly the mistake that made a bare rename break the wire.
	g.PCI = PCIInfo{
		BusID:        g1.PCI.BusID,
		Domain:       g1.PCI.Domain,
		Bus:          g1.PCI.Bus,
		Device:       g1.PCI.Device,
		PCIGen:       g1.PCI.PCIGen,
		LinkWidth:    g1.PCI.LinkWidth,
		MaxPCIGen:    g1.PCI.MaxPCIGen,
		MaxLinkWidth: g1.PCI.MaxLinkWidth,
		TxThroughput: g1.PCI.TxThroughput,
		RxThroughput: g1.PCI.RxThroughput,
	}

	g.ECC = ECCInfo{
		Enabled:      g1.ECC.Enabled,
		Pending:      g1.ECC.Pending,
		Mode:         g1.ECC.Mode,
		DRAMErrors:   upconvertV1ECCCounters(&g1.ECC.DRAMErrors),
		SRAMErrors:   upconvertV1ECCCounters(&g1.ECC.SRAMErrors),
		RetiredPages: RetiredPagesInfo(g1.ECC.RetiredPages),
	}
	g.RowRemapping = RowRemappingInfo(g1.RowRemapping)

	if g1.NvLink.Links != nil {
		links := make([]NvLinkState, 0, len(g1.NvLink.Links))
		for i := range g1.NvLink.Links {
			l1 := &g1.NvLink.Links[i]
			links = append(links, NvLinkState{
				LinkIndex:   l1.LinkIndex,
				State:       l1.State,
				Errors:      l1.Errors,
				ErrorsDelta: l1.ErrorsDelta,
				Error:       l1.Error,
			})
		}
		g.NvLink = NvLinkInfo{Links: links}
	}

	return g
}

func upconvertV1ECCCounters(c1 *v1.ECCErrorsCounters) ECCErrorsCounters {
	return ECCErrorsCounters{
		Volatile:  ECCErrors(c1.Volatile),
		Aggregate: ECCErrors(c1.Aggregate),
	}
}

// compressRaw gzips already-marshalled JSON. Used by tests to build a payload
// from a literal document rather than from a current-version struct.
func compressRaw(jsonData []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(jsonData); err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
