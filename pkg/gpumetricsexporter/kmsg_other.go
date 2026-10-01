//go:build !linux

package gpumetricsexporter

import (
	"errors"
	"time"
)

// The kernel log is Linux's /dev/kmsg; elsewhere (the darwin dev host) the
// reader only has to build, and reports that it cannot read anything.

func openKmsg() (kmsgDevice, error) {
	return nil, errors.New("reading the kernel log needs Linux " + kmsgPath)
}

func kmsgSinceBoot() (time.Duration, error) {
	return 0, errors.New("the kernel log clock needs Linux")
}
