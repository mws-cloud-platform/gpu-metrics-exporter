//go:build !linux

// Stubs so the exporter still builds, vets and tests on non-Linux hosts (a
// developer's macOS laptop, say).  The real device needs Linux sysfs and
// mmap of a PCI BAR; there is nothing to talk to elsewhere.

package attest

import (
	"errors"
	"fmt"
	"runtime"
	"time"
)

// ErrNoDevice reports that there is no pci-attest device to open.
var ErrNoDevice = errors.New("no pci-attest device")

type Info struct {
	Slot string
}

type Device struct{}

func Open() (*Device, error) {
	return nil, fmt.Errorf("%w: pci-attest is only supported on Linux (this is %s)",
		ErrNoDevice, runtime.GOOS)
}

func (d *Device) Close() error    { return nil }
func (d *Device) Info() Info      { return Info{} }
func (d *Device) MaxPayload() int { return 0 }
func (d *Device) Send([]byte, time.Duration) error {
	return fmt.Errorf("pci-attest is only supported on Linux")
}

// PinImage is a no-op off Linux.
func PinImage() error { return nil }
