package selfupdate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// commander runs the programs the update drives: dpkg and systemctl. Tests
// replace it with a model of the machine.
type commander interface {
	// output runs a command and returns its stdout, trimmed. A failure's
	// error carries the command line and its stderr.
	output(name string, args ...string) (string, error)
	// attached runs a command on the user's terminal: dpkg -i, whose
	// progress, warnings and any prompt are the user's to see.
	attached(name string, args ...string) error
}

type execCommander struct {
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (c execCommander) output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		err = fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return strings.TrimSpace(stdout.String()), err
}

func (c execCommander) attached(name string, args ...string) error {
	// No context: interrupting dpkg halfway through an upgrade leaves the
	// package half-installed. A Ctrl-C on the terminal reaches it anyway.
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return nil
}

// installedVersion is the version of the package dpkg has installed. An
// error means there is none: dpkg does not know the package, has only its
// configuration files left, or holds it half-installed.
func (u *Updater) installedVersion() (string, error) {
	out, err := u.cmd.output("dpkg-query", "--show", "--showformat=${Status}\t${Version}", packageName)
	if err != nil {
		return "", err
	}
	status, version, _ := strings.Cut(out, "\t")
	if !strings.HasSuffix(status, " installed") || version == "" {
		return "", fmt.Errorf("dpkg lists %s as %q", packageName, status)
	}
	return version, nil
}

// checkPackage checks that the downloaded file is the package the release
// promises: ours, for this architecture, and at the release's version. The
// last one is what keeps an old package, republished under a newer tag, from
// passing for an upgrade. dpkg-deb only reads the control file; it runs none
// of the package's scripts.
func (u *Updater) checkPackage(path, tag string) error {
	out, err := u.cmd.output("dpkg-deb", "--field", path, "Package", "Version", "Architecture")
	if err != nil {
		return fmt.Errorf("read the package's control fields: %w", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	for _, want := range [][2]string{{"Package", packageName}, {"Version", tag}, {"Architecture", packageArch}} {
		if got := fields[want[0]]; got != want[1] {
			return fmt.Errorf("%s: its control file says %s %q, expected %q", filepath.Base(path), want[0], got, want[1])
		}
	}
	return nil
}

// unitState is a unit as systemd sees it.
type unitState struct {
	LoadState     string // "loaded", "not-found", ...
	UnitFileState string // "enabled", "disabled", ...
	ActiveState   string // "active", "inactive", "activating", "failed", ...
	SubState      string // "running", "dead", "auto-restart", ...
	MainPID       int
}

func (s unitState) String() string {
	str := s.ActiveState + " (" + s.SubState + ")"
	if s.MainPID > 0 {
		str += ", pid " + strconv.Itoa(s.MainPID)
	}
	if s.UnitFileState != "" {
		str += ", " + s.UnitFileState
	}
	return str
}

// stopped is a service someone stopped, or never started: the package's
// postinst starts it on every install, and a crashed one is "failed" or
// "activating" (Restart=always), never "inactive".
func (s unitState) stopped() bool { return s.ActiveState == "inactive" }

// show returns the given properties of unit, as `systemctl show` prints
// them. It succeeds for a unit systemd does not know: LoadState says so.
func (u *Updater) show(unit string, props ...string) (map[string]string, error) {
	out, err := u.cmd.output("systemctl", "show", "--property="+strings.Join(props, ","), unit)
	if err != nil {
		return nil, err
	}
	p := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			p[k] = v
		}
	}
	return p, nil
}

func (u *Updater) unitState(unit string) (unitState, error) {
	p, err := u.show(unit, "LoadState", "UnitFileState", "ActiveState", "SubState", "MainPID")
	if err != nil {
		return unitState{}, err
	}
	pid, _ := strconv.Atoi(p["MainPID"])
	return unitState{
		LoadState:     p["LoadState"],
		UnitFileState: p["UnitFileState"],
		ActiveState:   p["ActiveState"],
		SubState:      p["SubState"],
		MainPID:       pid,
	}, nil
}

func (u *Updater) systemctl(args ...string) error {
	_, err := u.cmd.output("systemctl", args...)
	return err
}

// restoreService leaves the service as the operator had it before the
// upgrade, running the new binary if it ran at all.
//
// The package cannot do that itself. Every release so far disables and stops
// the service in prerm and enables and starts it in postinst, whatever state
// it was in, and on an upgrade the prerm that runs is the old package's -- so
// the new one could not even tell what the state was. Left alone, an upgrade
// would bring back a service disabled on purpose, as it must be on a
// Kubernetes node whose DaemonSet runs the exporter. And should a future
// package leave a running service alone, restarting it is what moves it onto
// the new binary.
func (u *Updater) restoreService(before unitState) error {
	if before.LoadState != "loaded" {
		return nil // no state to go back to
	}
	after, err := u.unitState(serviceName)
	if err != nil {
		return err
	}
	switch {
	case before.UnitFileState == "disabled" && after.UnitFileState == "enabled":
		if err := u.systemctl("disable", serviceName); err != nil {
			return err
		}
		u.printf("%s disabled again, as it was\n", serviceName)
	case before.UnitFileState == "enabled" && after.UnitFileState == "disabled":
		if err := u.systemctl("enable", serviceName); err != nil {
			return err
		}
		u.printf("%s enabled again, as it was\n", serviceName)
	}

	if before.stopped() {
		if !after.stopped() {
			if err := u.systemctl("stop", serviceName); err != nil {
				return err
			}
			u.printf("%s stopped again, as it was\n", serviceName)
		}
		return nil
	}
	if after.ActiveState == "active" && u.runsInstalledBinary(after.MainPID) {
		return nil
	}
	u.printf("restarting %s on the new binary\n", serviceName)
	return u.systemctl("restart", serviceName)
}

// runsInstalledBinary reports whether pid runs the file now at binaryPath.
// dpkg puts a new file in place by renaming it over the old one, so a
// process started before that still maps the old, unlinked inode.
func (u *Updater) runsInstalledBinary(pid int) bool {
	if pid <= 0 {
		return false
	}
	exe, err := os.Stat(filepath.Join(u.procRoot, strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	bin, err := os.Stat(u.binaryPath)
	return err == nil && os.SameFile(exe, bin)
}
