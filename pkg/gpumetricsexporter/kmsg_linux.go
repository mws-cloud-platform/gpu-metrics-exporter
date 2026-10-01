//go:build linux

package gpumetricsexporter

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// kmsgFile is /dev/kmsg opened non-blocking and read with raw syscalls. An
// os.File will not do: Go registers a character device with the netpoller,
// and Read then parks until the kernel logs something new instead of
// returning EAGAIN, so a drain would not finish until the next kernel message.
type kmsgFile struct {
	fd int
}

// openKmsg opens /dev/kmsg and checks that it is the kernel log device.
func openKmsg() (kmsgDevice, error) {
	fd, err := unix.Open(kmsgPath, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: kmsgPath, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: kmsgPath, Err: err}
	}
	if err := checkKmsgDevice(st.Mode, unix.Major(st.Rdev), unix.Minor(st.Rdev)); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &kmsgFile{fd: fd}, nil
}

func (f *kmsgFile) Read(p []byte) (int, error) {
	return unix.Read(f.fd, p)
}

func (f *kmsgFile) Close() error {
	return unix.Close(f.fd)
}

// kmsgSinceBoot is the clock the startup backlog is measured against. printk
// stamps records with local_clock(), which on a stable TSC or kvmclock runs at
// the raw counter rate, like CLOCK_MONOTONIC_RAW, and elsewhere follows
// CLOCK_MONOTONIC, which NTP slews — as it does CLOCK_BOOTTIME. The two drift
// apart by the counter's frequency error, seconds a week at tens of ppm, so
// the earlier of them is used: a look-back that reaches a little too far can
// re-ship a line, one that stops short loses it.
func kmsgSinceBoot() (time.Duration, error) {
	var raw, boot unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &raw); err != nil {
		return 0, err
	}
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot); err != nil {
		return 0, err
	}
	return time.Duration(min(raw.Nano(), boot.Nano())), nil
}
