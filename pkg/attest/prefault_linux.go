package attest

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PinImage pins this executable's own pages resident with mlock, so the
// device -- reading them through our page tables while the guest is stopped
// -- finds every page.  Open calls it; it is exported so a caller that
// re-execs or manages locking itself can invoke it directly.  It locks all of
// the executable's readable mappings, not just the r-x code: the attestation
// note lives in a read-only segment (with -z separate-code the note and the
// code are in different segments), and a note page that is not resident reads
// back as "the binary carries no attestation note".  Shared libraries and
// anonymous memory are left alone.
func PinImage() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var locked int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// address           perms offset  dev   inode  pathname
		// 00400000-004b2000 r-xp 00000000 fe:01 12345  /path/to/exe
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || !strings.HasPrefix(fields[1], "r") {
			continue // not readable: nothing the device would hash or read
		}
		if path := strings.Join(fields[5:], " "); path != exe {
			continue // only our own image, not libraries
		}
		lo, hi, ok := parseRange(fields[0])
		if !ok {
			continue
		}
		if err := mlockRange(lo, hi-lo); err != nil {
			return fmt.Errorf("mlock %x-%x: %w", lo, hi, err)
		}
		locked++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if locked == 0 {
		return fmt.Errorf("found no readable mapping for %s", exe)
	}
	return nil
}

func parseRange(s string) (lo, hi uintptr, ok bool) {
	dash := strings.IndexByte(s, '-')
	if dash < 0 {
		return 0, 0, false
	}
	a, err1 := strconv.ParseUint(s[:dash], 16, 64)
	b, err2 := strconv.ParseUint(s[dash+1:], 16, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uintptr(a), uintptr(b), true
}

// mlock an arbitrary VA range (not backed by a Go slice), so we can lock the
// code segment without constructing a slice over executable memory.
func mlockRange(addr uintptr, length uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_MLOCK, addr, length, 0); errno != 0 {
		return errno
	}
	return nil
}
