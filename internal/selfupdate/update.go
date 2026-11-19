// Package selfupdate implements `gpu-metrics-exporter update` and
// `gpu-metrics-exporter auto-update`: upgrade the installed
// gpu-metrics-exporter package on a VM to the latest GitHub release -- by
// hand, or from the systemd timer auto-update enables -- and put its service
// back the way it was.
//
// It refuses to run in a Kubernetes pod: there the exporter comes from the
// DaemonSet's image, which the cluster admin rolls out.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"golang.org/x/sys/unix"
)

const (
	DefaultRepo    = "mws-cloud-platform/gpu-metrics-exporter"
	DefaultAPIBase = "https://api.github.com"

	// ExitTempFail (EX_TEMPFAIL) is what `update -auto` exits with when it
	// failed before installing anything: the next run tries again. The update
	// unit counts it as success (SuccessExitStatus=75), so a VM that cannot
	// reach GitHub does not show a failed unit three times a day.
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

// Updater upgrades the installed package to the latest release.
type Updater struct {
	// CheckOnly reports whether a newer release exists and changes nothing.
	CheckOnly bool
	// Force installs the latest release even when it is not newer than the
	// installed version, or when the installed version cannot be compared,
	// and even while the service is stopped (it is left stopped).
	Force bool
	// Auto is the unattended run the auto-update timer starts. It leaves a
	// stopped service alone instead of failing, and takes no Force.
	Auto bool

	self    string // this binary's version, for -check without the package
	repo    string
	apiBase string
	http    *http.Client
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
// binary's version.
func New(selfVersion string) *Updater {
	return &Updater{
		self:              selfVersion,
		repo:              DefaultRepo,
		apiBase:           DefaultAPIBase,
		http:              newHTTPClient(http.DefaultTransport),
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

// Update checks for a newer release and, unless CheckOnly, installs it.
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
			"Install the .deb from https://github.com/%s/releases first", packageName, err, u.repo)
	}

	rel, err := u.latestRelease(ctx)
	if err != nil {
		return err
	}
	latest, err := ParseVersion(rel.TagName)
	if err != nil || !latest.IsRelease() {
		return fmt.Errorf("the latest release's tag %q is not a release version (YYYY.MM.DD-N)", rel.TagName)
	}
	u.printf("latest release: %s %s\n", rel.TagName, rel.HTMLURL)

	newer := false
	switch cur, err := ParseVersion(current); {
	case err != nil:
		u.printf("cannot tell whether %s is newer than %s\n", rel.TagName, displayVersion(current))
		if !u.CheckOnly && !u.Force {
			return errors.New("pass -force to install it anyway")
		}
	case latest.Compare(cur) > 0:
		newer = true
		u.printf("update available: %s -> %s\n", current, rel.TagName)
	case latest.Compare(cur) == 0:
		u.printf("already up to date\n")
	default:
		u.printf("the installed version is newer than the latest release\n")
	}
	if u.CheckOnly || !(newer || u.Force) {
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

	name := fmt.Sprintf("%s_%s_%s.deb", packageName, rel.TagName, packageArch)
	var pkg *asset
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			pkg = &rel.Assets[i]
		}
	}
	if pkg == nil {
		return fmt.Errorf("release %s has no %s", rel.TagName, name)
	}
	if pkg.State != "" && pkg.State != "uploaded" {
		return fmt.Errorf("release %s: %s is still %q, not uploaded", rel.TagName, name, pkg.State)
	}

	dir, err := os.MkdirTemp(u.tempDir, "gpu-metrics-exporter-update-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	u.printf("downloading %s (%.1f MiB)\n", name, float64(pkg.Size)/(1<<20))
	path, err := u.download(ctx, *pkg, dir, name)
	if err != nil {
		return err
	}
	u.printf("size and %s match what GitHub recorded for the asset\n", pkg.Digest)
	if err := u.checkPackage(path, rel.TagName); err != nil {
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
		return &InstallError{fmt.Errorf("%s is installed, but putting the service back failed: %w", rel.TagName, err)}
	}
	if got, err := u.installedVersion(); err != nil || got != rel.TagName {
		return &InstallError{fmt.Errorf("dpkg -i succeeded, yet dpkg reports %q installed (%v), not %s", got, err, rel.TagName)}
	}
	after, err := u.unitState(serviceName)
	if err != nil {
		return &InstallError{err}
	}
	u.printf("updated %s -> %s; %s: %s\n", current, rel.TagName, serviceName, after)
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

func (u *Updater) userAgent() string { return "gpu-metrics-exporter/" + displayVersion(u.self) }

func (u *Updater) printf(format string, args ...any) { _, _ = fmt.Fprintf(u.out, format, args...) }

func displayVersion(v string) string {
	if v == "" {
		return "unversioned"
	}
	return v
}
