package gpumetricsexporter

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"go.uber.org/zap"
)

// TestInitNVMLForTick covers the per-tick NVML lifecycle: init is attempted
// on every tick (a failing init — e.g. "Driver/library version mismatch"
// after a guest-side driver upgrade without a reboot — must not pin the
// exporter to empty metrics until the VM is restarted), and a successful
// tick is paired with a shutdown that releases the dlopen handle so a
// library update on disk is picked up by the next tick. Both are guarded on
// nvmlInitialized: shutdown after a failed init must be a no-op (the real
// nvml.Shutdown would be a cgo abort no recover() can contain), and a second
// init must not bump NVML's refcount past what the paired shutdown releases.
func TestInitNVMLForTick(t *testing.T) {
	newExporter := func() *GpuMetricsExporter {
		return NewGpuMetricsExporter(GpuMetricsExporterConfig{
			ServerPort: 9999,
			Log:        zap.NewNop(),
			TickPeriod: 60 * time.Second,
		})
	}

	t.Run("init failure retried on every tick, recovers on its own", func(t *testing.T) {
		e := newExporter()
		initErr := errors.New("Driver/library version mismatch")
		inits, shutdowns := 0, 0
		e.initNVMLFn = func() error {
			inits++
			if inits < 3 {
				return initErr
			}
			return nil
		}
		e.shutdownNVMLFn = func() { shutdowns++ }

		// Tick 1 and 2: init fails, error recorded, nvmlInitialized false.
		// shutdownNVML no-ops on these ticks — which is what makes the
		// unconditional `defer e.shutdownNVML()` in queryMetrics safe.
		e.initNVMLForTick()
		if e.nvmlInitialized {
			t.Fatal("nvmlInitialized set despite init failure")
		}
		if want := "e.initNVML error: " + initErr.Error(); e.initNVMLError != want {
			t.Fatalf("initNVMLError = %q, want %q", e.initNVMLError, want)
		}
		e.shutdownNVML()
		e.initNVMLForTick()
		if e.nvmlInitialized {
			t.Fatal("nvmlInitialized set despite init failure")
		}
		e.shutdownNVML()

		// Tick 3: init succeeds, error cleared, shutdown pairs with it.
		e.initNVMLForTick()
		if !e.nvmlInitialized {
			t.Fatal("nvmlInitialized not set after init succeeded")
		}
		if e.initNVMLError != "" {
			t.Fatalf("initNVMLError = %q after recovery, want empty", e.initNVMLError)
		}
		e.shutdownNVML()
		if e.nvmlInitialized {
			t.Fatal("nvmlInitialized still set after shutdown")
		}
		if inits != 3 {
			t.Fatalf("init called %d times, want 3", inits)
		}
		if shutdowns != 1 {
			t.Fatalf("shutdown called %d times, want 1 (only after successful init)", shutdowns)
		}
	})

	t.Run("second init while initialized is a no-op", func(t *testing.T) {
		// A second Init would bump NVML's refcount, and the single paired
		// Shutdown would leave the library open past the tick — losing the
		// dlclose-per-tick property.
		e := newExporter()
		inits, shutdowns := 0, 0
		e.initNVMLFn = func() error { inits++; return nil }
		e.shutdownNVMLFn = func() { shutdowns++ }

		e.initNVMLForTick()
		e.initNVMLForTick()
		if inits != 1 {
			t.Fatalf("init called %d times, want 1", inits)
		}
		e.shutdownNVML()
		if shutdowns != 1 {
			t.Fatalf("shutdown called %d times, want 1", shutdowns)
		}
	})
}

// TestRemoveStrings covers the helper that drains the XID buffer on a
// successful send: only the lines a payload actually carried are retired, the
// rest stay pending for the next tick, and order is preserved.
func TestRemoveStrings(t *testing.T) {
	tests := []struct {
		name string
		dst  []string
		drop []string
		want []string
	}{
		{
			name: "drains delivered subset, keeps pending",
			dst:  []string{"Xid 79 on GPU 0", "SXid 13", "Xid 119"},
			drop: []string{"Xid 79 on GPU 0", "SXid 13"},
			want: []string{"Xid 119"},
		},
		{name: "drop nothing", dst: []string{"a", "b"}, drop: nil, want: []string{"a", "b"}},
		{name: "drop everything", dst: []string{"a", "b"}, drop: []string{"a", "b"}, want: []string{}},
		{name: "drop not present", dst: []string{"a"}, drop: []string{"z"}, want: []string{"a"}},
		{name: "empty dst", dst: nil, drop: []string{"a"}, want: nil},
		{name: "duplicate in dst preserved once removed", dst: []string{"a", "a", "b"}, drop: []string{"a"}, want: []string{"b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := removeStrings(tc.dst, tc.drop)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("removeStrings(%v, %v) = %v, want %v", tc.dst, tc.drop, got, tc.want)
			}
		})
	}
}

func TestGetValueDelta(t *testing.T) {
	tests := []struct {
		name   string
		newVal uint64
		oldVal uint64
		want   int64
	}{
		{name: "increase", newVal: 10, oldVal: 4, want: 6},
		{name: "equal", newVal: 7, oldVal: 7, want: 0},
		{name: "decrease resets to zero", newVal: 3, oldVal: 9, want: 0}, // counter reset/reboot
		{name: "both zero", newVal: 0, oldVal: 0, want: 0},
		{name: "from zero", newVal: 5, oldVal: 0, want: 5},
		{name: "to zero from value", newVal: 0, oldVal: 5, want: 0},
		{name: "max uint64 increase", newVal: ^uint64(0), oldVal: ^uint64(0) - 1, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := getValueDelta(tc.newVal, tc.oldVal)
			if got != tc.want {
				t.Fatalf("getValueDelta(%d, %d) = %d, want %d", tc.newVal, tc.oldVal, got, tc.want)
			}
		})
	}
}

func TestGetComputeModeString(t *testing.T) {
	tests := []struct {
		mode nvml.ComputeMode
		want string
	}{
		{nvml.COMPUTEMODE_DEFAULT, "Default"},
		{nvml.COMPUTEMODE_EXCLUSIVE_THREAD, "Exclusive Thread"},
		{nvml.COMPUTEMODE_PROHIBITED, "Prohibited"},
		{nvml.COMPUTEMODE_EXCLUSIVE_PROCESS, "Exclusive Process"},
		{nvml.ComputeMode(99999), "Unknown (99999)"}, // unknown -> formatted
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := getComputeModeString(tc.mode); got != tc.want {
				t.Fatalf("getComputeModeString(%v) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

func TestParseThrottleReasons(t *testing.T) {
	tests := []struct {
		name   string
		reason uint64
		want   string
	}{
		{"none", 0, "None"},
		{"gpu idle", nvml.ClocksThrottleReasonGpuIdle, "GPU Idle"},
		{"apps clocks", nvml.ClocksThrottleReasonApplicationsClocksSetting, "Applications Clocks Setting"},
		{"sw power cap", nvml.ClocksThrottleReasonSwPowerCap, "SW Power Cap"},
		{"hw slowdown", nvml.ClocksThrottleReasonHwSlowdown, "HW Slowdown"},
		{"sync boost", nvml.ClocksThrottleReasonSyncBoost, "Sync Boost"},
		{"sw thermal", nvml.ClocksThrottleReasonSwThermalSlowdown, "SW Thermal Slowdown"},
		{"hw thermal", nvml.ClocksThrottleReasonHwThermalSlowdown, "HW Thermal Slowdown"},
		{"hw power brake", nvml.ClocksThrottleReasonHwPowerBrakeSlowdown, "HW Power Brake Slowdown"},
		{"display clock", nvml.ClocksThrottleReasonDisplayClockSetting, "Display Clock Setting"},
		{
			"combination preserves fixed order",
			nvml.ClocksThrottleReasonSwPowerCap | nvml.ClocksThrottleReasonGpuIdle,
			"GPU Idle, SW Power Cap",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseThrottleReasons(tc.reason); got != tc.want {
				t.Fatalf("parseThrottleReasons(%d) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

func TestParseEventReasons(t *testing.T) {
	tests := []struct {
		name   string
		events uint64
		want   string
	}{
		{"none", 0, "None"},
		{"gpu idle", nvml.ClocksEventReasonGpuIdle, "GPU Idle"},
		{"apps clocks", nvml.ClocksEventReasonApplicationsClocksSetting, "Applications Clocks Setting"},
		{"sw power cap", nvml.ClocksEventReasonSwPowerCap, "SW Power Cap"},
		{"sync boost", nvml.ClocksEventReasonSyncBoost, "Sync Boost"},
		{"sw thermal", nvml.ClocksEventReasonSwThermalSlowdown, "SW Thermal Slowdown"},
		{"display clock", nvml.ClocksEventReasonDisplayClockSetting, "Display Clock Setting"},
		{
			"combination",
			nvml.ClocksEventReasonSwPowerCap | nvml.ClocksEventReasonGpuIdle,
			"GPU Idle, SW Power Cap",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseEventReasons(tc.events); got != tc.want {
				t.Fatalf("parseEventReasons(%d) = %q, want %q", tc.events, got, tc.want)
			}
		})
	}
}
