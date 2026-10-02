package gpumetricsexporter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestParseNvidiaDriverVersion covers the NVRM line of both kernel module
// flavours and of the old two-part versions, and what is not a version.
func TestParseNvidiaDriverVersion(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string // empty: an error is expected
	}{
		{
			name: "open kernel module",
			content: "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  550.54.15  Release Build  (dvs-builder@U16-I3-B03-4-3)  Tue Mar  5 22:16:25 UTC 2024\n" +
				"GCC version:  gcc version 12.3.0 (Ubuntu 12.3.0-1ubuntu1~22.04)\n",
			want: "550.54.15",
		},
		{
			name: "proprietary kernel module",
			content: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  535.104.05  Sat Aug 19 01:15:15 UTC 2023\n" +
				"GCC version:  gcc version 12.2.0 (Debian 12.2.0-14)\n",
			want: "535.104.05",
		},
		{
			name:    "aarch64",
			content: "NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  570.86.15  Release Build  (dvs-builder@U16-I1-N08-12-1)  Thu Jan 23 23:01:38 UTC 2025\n",
			want:    "570.86.15",
		},
		{
			name:    "two-part version of an old branch",
			content: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  390.157  Wed Oct 12 09:19:07 UTC 2022\n",
			want:    "390.157",
		},
		{name: "empty", content: ""},
		{name: "no NVRM line", content: "GCC version:  gcc version 12.3.0 (Ubuntu 12.3.0-1ubuntu1~22.04)\n"},
		{name: "NVRM line without a version", content: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  Sat Aug 19 01:15:15 UTC 2023\n"},
		{name: "digits and dots too long for a version", content: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  " + strings.Repeat("1.", 5000) + "1\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNvidiaDriverVersion(tc.content)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("got version %q, want an error", got)
				}
				// Whatever the file holds, the error that ships stays small.
				if n := len(err.Error()); n > 2*maxProblemValueLen {
					t.Errorf("error is %d bytes long", n)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestQueryMetricsReportsNvidiaDriverVersion pins the wiring: the version rides
// on the payload of a tick whose NVML init failed -- it comes from the kernel
// module -- and once the module is gone the field is empty and the error says
// why.
func TestQueryMetricsReportsNvidiaDriverVersion(t *testing.T) {
	dir := t.TempDir()
	e := NewGpuMetricsExporter(GpuMetricsExporterConfig{
		Log:            zap.NewNop(),
		TickPeriod:     time.Hour,
		InstanceIDPath: filepath.Join(dir, "instance-id"),
	})
	e.initNVMLFn = func() error { return errors.New("no NVML here") }
	e.nvidiaDriverVersionPath = filepath.Join(dir, "version")
	content := "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  610.43.02  Release Build  (dvs-builder@U22)  Tue Sep  1 10:00:00 UTC 2026\n"
	if err := os.WriteFile(e.nvidiaDriverVersionPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := e.queryMetrics()
	if err != nil {
		t.Fatalf("queryMetrics: %v", err)
	}
	if m.NvidiaDriverVersion != "610.43.02" || m.ExporterInfo.ReadNvidiaDriverVersionError != "" {
		t.Errorf("with NVML down: version %q, error %q; want 610.43.02 and no error",
			m.NvidiaDriverVersion, m.ExporterInfo.ReadNvidiaDriverVersionError)
	}

	if err := os.Remove(e.nvidiaDriverVersionPath); err != nil {
		t.Fatal(err)
	}
	m, err = e.queryMetrics()
	if err != nil {
		t.Fatalf("queryMetrics: %v", err)
	}
	if m.NvidiaDriverVersion != "" || !strings.Contains(m.ExporterInfo.ReadNvidiaDriverVersionError, e.nvidiaDriverVersionPath) {
		t.Errorf("module unloaded: version %q, error %q; want no version and an error naming the file",
			m.NvidiaDriverVersion, m.ExporterInfo.ReadNvidiaDriverVersionError)
	}
}
