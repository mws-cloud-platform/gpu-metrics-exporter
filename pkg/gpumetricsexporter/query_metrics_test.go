package gpumetricsexporter

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

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
