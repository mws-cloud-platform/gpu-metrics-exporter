// Package selfupdate implements `gpu-metrics-exporter update` and
// `gpu-metrics-exporter auto-update`: upgrade the installed
// gpu-metrics-exporter package on a VM to the version served by its host over
// the existing metrics vsock port -- by hand, or from the systemd timer
// auto-update enables -- and put its service back the way it was.
//
// It refuses to run in a Kubernetes pod: there the exporter comes from the
// DaemonSet's image, which the cluster admin rolls out.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.mws.cloud/gpu-metrics-exporter/pkg/exporterupdate"
	"golang.org/x/sys/unix"
)

const (
	// ExitTempFail (EX_TEMPFAIL) is what `update -auto` exits with when it
	// failed before installing anything: the next run tries again. The update
	// unit counts it as success (SuccessExitStatus=75), so a VM whose host
	// update service is unavailable does not show a failed unit three times a day.
	ExitTempFail = 75

	packageName = "gpu-metrics-exporter"
	packageArch = "amd64"
	serviceName = "gpu-metrics-exporter.service"
	// The auto-update timer, and the unit it starts to run `update -auto`.
	timerName         = "gpu-metrics-exporter-update.timer"
	updateServiceName = "gpu-metrics-exporter-update.service"
	// Where the package installs the binary the service runs.
	binaryPath = "/usr/local/bin/gpu-metrics-exporter"
	// Held for the length of an update, so a manual one and the timer's
	// never run at once.
	lockPath = "/run/gpu-metrics-exporter-update.lock"
)

var errKubernetes = errors.New("this is a Kubernetes pod: the exporter here runs from the DaemonSet's image, " +
	"and the cluster admin updates it by rolling out a new one. update and auto-update manage the .deb package on a VM")

// InstallError is an update that failed at or after `dpkg -i`: the package or
// the service may need a look. Any other error left the system as it was.
type InstallError struct{ Err error }

func (e *InstallError) Error() string { return e.Err.Error() }
func (e *InstallError) Unwrap() error { return e.Err }

type packageSource interface {
	GetMetadata(context.Context) (exporterupdate.Metadata, error)
	Download(context.Context, exporterupdate.Metadata, io.Writer) error
}

// Updater upgrades the installed package to the version served by the host.
type Updater struct {
	// CheckOnly reports whether the host has a newer package and changes nothing.
	CheckOnly bool
	// Force installs the host package even when it is not newer than the
	// installed version, or when the installed version cannot be compared,
	// and even while the service is stopped (it is left stopped).
	Force bool
	// Auto is the unattended run the auto-update timer starts. It leaves a
	// stopped service alone instead of failing, and takes no Force.
	Auto bool

	self    string // this binary's version, for -check without the package
	source  packageSource
	cmd     commander
	out     io.Writer
	geteuid func() int
	getenv  func(string) string
	// The machine's paths, replaced by tests.
	binaryPath        string
	procRoot          string
	systemdRunDir     string
	serviceAccountDir string
	tempDir           string
	lockPath          string
}

// New returns an Updater for this machine. selfVersion is the running
// binary's version. serverPort is the existing host metrics/update vsock port.
func New(selfVersion string, serverPort int) *Updater {
	return &Updater{
		self:              selfVersion,
		source:            exporterupdate.NewClient(nil, serverPort),
		cmd:               execCommander{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr},
		out:               os.Stdout,
		geteuid:           os.Geteuid,
		getenv:            os.Getenv,
		binaryPath:        binaryPath,
		procRoot:          "/proc",
		systemdRunDir:     "/run/systemd/system",
		serviceAccountDir: "/var/run/secrets/kubernetes.io/serviceaccount",
		lockPath:          lockPath,
	}
}

// Update checks the host for a newer package and, unless CheckOnly, installs it.
func (u *Updater) Update(ctx context.Context) error {
	if u.inKubernetes() {
		return errKubernetes
	}
	if u.Auto && (u.Force || u.CheckOnly) {
		return errors.New("an automatic update takes neither -force nor -check")
	}
	if !u.CheckOnly {
		if err := u.checkHost("gpu-metrics-exporter update"); err != nil {
			return err
		}
		unlock, err := u.lock()
		if err != nil {
			return err
		}
		defer unlock()
	}

	current, err := u.installedVersion()
	switch {
	case err == nil:
		u.printf("installed: %s\n", current)
	case u.CheckOnly:
		current = u.self
		u.printf("the %s package is not installed (%v); this binary is %s\n", packageName, err, displayVersion(current))
	default:
		return fmt.Errorf("update upgrades an installed %s package, and there is none: %w. "+
			"Install the initial .deb by hand first", packageName, err)
	}

	candidate, err := u.source.GetMetadata(ctx)
	if err != nil {
		return err
	}
	if candidate.Size <= 0 || candidate.Size > exporterupdate.MaxPackageSize {
		return fmt.Errorf("host package has invalid size %d", candidate.Size)
	}
	latest, err := ParseVersion(candidate.Version)
	if err != nil || !latest.IsRelease() {
		return fmt.Errorf("the host package version %q is not a release version (YYYY.MM.DD-N)", candidate.Version)
	}
	u.printf("host package: %s (%d bytes)\n", candidate.Version, candidate.Size)

	newer := false
	switch cur, err := ParseVersion(current); {
	case err != nil:
		u.printf("cannot tell whether %s is newer than %s\n", candidate.Version, displayVersion(current))
		if !u.CheckOnly && !u.Force {
			return errors.New("pass -force to install it anyway")
		}
	case latest.Compare(cur) > 0:
		newer = true
		u.printf("update available: %s -> %s\n", current, candidate.Version)
	case latest.Compare(cur) == 0:
		u.printf("already up to date\n")
	default:
		u.printf("the installed version is newer than the host package\n")
	}
	if u.CheckOnly || (!newer && !u.Force) {
		return nil
	}

	before, err := u.unitState(serviceName)
	if err != nil {
		return err
	}
	if before.stopped() {
		switch {
		case u.Auto:
			u.printf("%s is stopped (%s): auto-update leaves a stopped exporter alone\n", serviceName, before)
			return nil
		case !u.Force:
			return fmt.Errorf("%s is stopped (%s), and installing the package starts it. "+
				"If it is down on purpose -- on a Kubernetes node the DaemonSet runs the exporter, "+
				"and two of them on one VM corrupt each other's stream -- leave it be; "+
				"otherwise pass -force to update anyway, and it is stopped again right after", serviceName, before)
		}
	}

	name := fmt.Sprintf("%s_%s_%s.deb", packageName, candidate.Version, packageArch)
	dir, err := os.MkdirTemp(u.tempDir, "gpu-metrics-exporter-update-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	u.printf("downloading %s from host (%.1f MiB)\n", name, float64(candidate.Size)/(1<<20))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	downloadErr := u.source.Download(ctx, candidate, f)
	closeErr := f.Close()
	if downloadErr != nil {
		return downloadErr
	}
	if closeErr != nil {
		return closeErr
	}
	u.printf("size and sha256 match the host package metadata\n")
	if err := u.checkPackage(path, candidate.Version); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err // interrupted before anything changed
	}

	// From here on, a failure may have changed the system.
	u.printf("installing: dpkg -i %s\n", name)
	if err := u.cmd.attached("dpkg", "-i", path); err != nil {
		return &InstallError{fmt.Errorf("%w; see `dpkg --audit` and `systemctl status %s`", err, serviceName)}
	}
	if err := u.restoreService(before); err != nil {
		return &InstallError{fmt.Errorf("%s is installed, but putting the service back failed: %w", candidate.Version, err)}
	}
	if got, err := u.installedVersion(); err != nil || got != candidate.Version {
		return &InstallError{fmt.Errorf("dpkg -i succeeded, yet dpkg reports %q installed (%v), not %s", got, err, candidate.Version)}
	}
	after, err := u.unitState(serviceName)
	if err != nil {
		return &InstallError{err}
	}
	u.printf("updated %s -> %s; %s: %s\n", current, candidate.Version, serviceName, after)
	return nil
}

// inKubernetes reports a Kubernetes pod: kubelet sets KUBERNETES_SERVICE_HOST
// in every container, and mounts a service account token unless told not to.
func (u *Updater) inKubernetes() bool {
	if u.getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	_, err := os.Stat(u.serviceAccountDir)
	return err == nil
}

// checkHost checks for what changing the installation takes: root, and the
// systemd that runs the exporter's service. command is how to run it as root.
func (u *Updater) checkHost(command string) error {
	if u.geteuid() != 0 {
		return fmt.Errorf("this changes the installed package and its services: run it as root (sudo %s)", command)
	}
	// What sd_booted(3) checks. A container has its own /run, so this also
	// holds where the pod check was evaded.
	if fi, err := os.Stat(u.systemdRunDir); err != nil || !fi.IsDir() {
		return errors.New("this system is not running systemd, which runs the exporter's service; " +
			"in a container, the exporter is updated through its image")
	}
	return nil
}

// lock takes the update lock, so that a manual update and the timer's never
// run at once. The kernel drops it when the process exits, however it exits.
func (u *Updater) lock() (unlock func(), err error) {
	f, err := os.OpenFile(u.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("another update is running")
		}
		return nil, fmt.Errorf("lock %s: %w", u.lockPath, err)
	}
	return func() { _ = f.Close() }, nil
}

func (u *Updater) printf(format string, args ...any) { _, _ = fmt.Fprintf(u.out, format, args...) }

func displayVersion(v string) string {
	if v == "" {
		return "unversioned"
	}
	return v
}
