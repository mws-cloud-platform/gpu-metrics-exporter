//go:build linux

package gpumetricsexporter

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestKmsgReaderRealDevice reads the live /dev/kmsg, guarding what the fakes
// cannot: that the real device passes the device check, that every record it
// hands out parses, and that a drain returns once it has read the log instead
// of blocking until the kernel logs something new. It needs the device and
// CAP_SYSLOG, so it skips in `make docker-test`; run it as root on a Linux
// host, or in a privileged container:
//
//	docker run --rm --privileged --platform linux/amd64 gpu-metrics-exporter-builder-test \
//		go test -run RealDevice -v ./pkg/gpumetricsexporter/
func TestKmsgReaderRealDevice(t *testing.T) {
	dev, err := openKmsg()
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		t.Skipf("kernel log not readable here: %v", err)
	}
	if err != nil {
		t.Fatalf("openKmsg: %v", err)
	}
	_ = dev.Close()

	r := newKmsgReader(zap.NewNop())
	defer r.closeDevice()
	for pass := range 2 {
		var records int
		done := make(chan error, 1)
		go func() {
			done <- r.drain(time.Duration(math.MaxInt64), func(kmsgRecord) { records++ })
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("drain %d: %v", pass, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("drain %d did not return: reads block instead of reporting EAGAIN", pass)
		}
		if pass == 0 && records == 0 {
			t.Fatal("read no records from a live kernel log")
		}
		t.Logf("drain %d: %d records", pass, records)
	}
}

// TestKmsgSinceBoot checks the startup look-back's clock against the kernel's
// own uptime: time since boot, not since the epoch, and never ahead of it.
// (The tolerance below is for the raw clock lagging on a long-running host.)
func TestKmsgSinceBoot(t *testing.T) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Skipf("no /proc/uptime: %v", err)
	}
	fields := strings.Fields(string(data))
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Fatalf("parsing /proc/uptime %q: %v", data, err)
	}
	uptime := time.Duration(secs * float64(time.Second))

	d, err := kmsgSinceBoot()
	if err != nil {
		t.Fatalf("kmsgSinceBoot: %v", err)
	}
	if d <= 0 || d > uptime+time.Second || d < uptime-time.Hour {
		t.Fatalf("kmsgSinceBoot = %v, /proc/uptime = %v", d, uptime)
	}
}
