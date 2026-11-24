package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/exporterupdate"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"golang.org/x/sys/unix"
)

const (
	oldTag = "2026.10.06-1"
	newTag = "2026.11.20-1"
)

// machine models what the update drives: dpkg's database, the installed
// binary, the exporter's service with its process, and the auto-update timer
// with the unit it starts.
type machine struct {
	t          *testing.T
	binaryPath string
	procRoot   string
	pkgStatus  string // dpkg's Status field; "" when dpkg has never heard of the package
	pkgVersion string
	unit       unitState
	timer      unitState
	// The update unit's last run: Result, ExecMainStatus, ExecMainExitTimestamp.
	lastCheck map[string]string
	nextPID   int
	// By default dpkg -i runs the maintainer scripts every release so far
	// ships: prerm disables and stops the service, postinst enables and
	// starts it. This models a package that leaves it alone instead.
	keepServiceOnUpgrade bool
	dpkgErr              error
	showErr              error            // what systemctl show fails with, as without D-Bus
	verbErr              map[string]error // what `systemctl <verb> gpu-metrics-exporter.service` fails with
	installAs            string           // a version for dpkg to record instead of the package's own
	calls                []string
	installed            [][]byte // each package handed to dpkg -i
}

func newMachine(t *testing.T, version string, enabled, running bool) *machine {
	dir := t.TempDir()
	m := &machine{
		t:          t,
		binaryPath: filepath.Join(dir, "gpu-metrics-exporter"),
		procRoot:   filepath.Join(dir, "proc"),
		pkgStatus:  "install ok installed",
		pkgVersion: version,
		nextPID:    100,
		unit:       unitState{LoadState: "loaded", UnitFileState: "disabled", ActiveState: "inactive", SubState: "dead"},
		timer:      unitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active", SubState: "waiting"},
		lastCheck:  map[string]string{"Result": "success", "ExecMainStatus": "0", "ExecMainExitTimestamp": ""},
	}
	if err := os.WriteFile(m.binaryPath, []byte("exporter "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	if enabled {
		m.unit.UnitFileState = "enabled"
	}
	if running {
		m.start()
	}
	return m
}

// start runs the binary now at binaryPath. /proc/<pid>/exe is a hard link
// to its inode, so -- like the real one -- it keeps naming the file the
// process was started from after dpkg renames a new one over the path.
func (m *machine) start() {
	if m.unit.ActiveState == "active" {
		return
	}
	m.nextPID++
	dir := filepath.Join(m.procRoot, strconv.Itoa(m.nextPID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.Link(m.binaryPath, filepath.Join(dir, "exe")); err != nil {
		m.t.Fatal(err)
	}
	m.unit.ActiveState, m.unit.SubState, m.unit.MainPID = "active", "running", m.nextPID
}

func (m *machine) stop() {
	m.unit.ActiveState, m.unit.SubState, m.unit.MainPID = "inactive", "dead", 0
}

// props is s as `systemctl show` prints it.
func (s unitState) props() map[string]string {
	return map[string]string{
		"LoadState":     s.LoadState,
		"UnitFileState": s.UnitFileState,
		"ActiveState":   s.ActiveState,
		"SubState":      s.SubState,
		"MainPID":       strconv.Itoa(s.MainPID),
	}
}

func (m *machine) output(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	m.calls = append(m.calls, line)
	switch {
	case name == "dpkg-query":
		if m.pkgStatus == "" {
			return "", errors.New("dpkg-query: no packages found matching gpu-metrics-exporter")
		}
		return m.pkgStatus + "\t" + m.pkgVersion, nil
	case name == "dpkg-deb" && len(args) > 1 && args[0] == "--field":
		data, err := os.ReadFile(args[1]) // the test packages are bare control files
		return strings.TrimSpace(string(data)), err
	case name == "systemctl" && len(args) == 3 && args[0] == "show" && strings.HasPrefix(args[1], "--property="):
		if m.showErr != nil {
			return "", m.showErr
		}
		var all map[string]string
		switch args[2] {
		case serviceName:
			all = m.unit.props()
		case timerName:
			all = m.timer.props()
		case updateServiceName:
			all = m.lastCheck
		default:
			m.t.Fatalf("unexpected command: %s", line)
		}
		// In an order of systemd's own, not the one asked for.
		props := strings.Split(strings.TrimPrefix(args[1], "--property="), ",")
		slices.Sort(props)
		var lines []string
		for _, p := range props {
			lines = append(lines, p+"="+all[p])
		}
		return strings.Join(lines, "\n"), nil
	case name == "systemctl" && len(args) == 3 && args[1] == "--now" && args[2] == timerName:
		switch args[0] {
		case "enable":
			m.timer.UnitFileState, m.timer.ActiveState, m.timer.SubState = "enabled", "active", "waiting"
			return "", nil
		case "disable":
			m.timer.UnitFileState, m.timer.ActiveState, m.timer.SubState = "disabled", "inactive", "dead"
			return "", nil
		}
	case name == "systemctl" && len(args) == 4 && args[0] == "list-timers" && args[3] == timerName:
		next := "n/a                         n/a     "
		if m.timer.ActiveState == "active" {
			next = "Fri 2026-11-20 05:27:12 UTC 7h left "
		}
		return "NEXT                        LEFT     LAST PASSED UNIT                              ACTIVATES\n" +
			next + "n/a  n/a    gpu-metrics-exporter-update.timer gpu-metrics-exporter-update.service\n\n1 timers listed.", nil
	case name == "systemctl" && len(args) == 2 && args[1] == serviceName:
		if err := m.verbErr[args[0]]; err != nil {
			return "", err
		}
		switch args[0] {
		case "enable":
			m.unit.UnitFileState = "enabled"
			return "", nil
		case "disable":
			m.unit.UnitFileState = "disabled"
			return "", nil
		case "start":
			m.start()
			return "", nil
		case "stop":
			m.stop()
			return "", nil
		case "restart":
			m.stop()
			m.start()
			return "", nil
		}
	}
	m.t.Fatalf("unexpected command: %s", line)
	return "", nil
}

func (m *machine) attached(name string, args ...string) error {
	line := strings.Join(append([]string{name}, args...), " ")
	m.calls = append(m.calls, line)
	if name != "dpkg" || len(args) != 2 || args[0] != "-i" {
		m.t.Fatalf("unexpected command: %s", line)
	}
	if m.dpkgErr != nil {
		return m.dpkgErr
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	m.installed = append(m.installed, data)
	if !m.keepServiceOnUpgrade { // the old package's prerm
		m.unit.UnitFileState = "disabled"
		m.stop()
	}
	tmp := m.binaryPath + ".dpkg-new"
	if err := os.WriteFile(tmp, append([]byte("exporter "), data...), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.binaryPath); err != nil {
		return err
	}
	m.pkgStatus, m.pkgVersion = "install ok installed", controlField(data, "Version")
	if m.installAs != "" {
		m.pkgVersion = m.installAs
	}
	if !m.keepServiceOnUpgrade { // the new package's postinst
		m.unit.UnitFileState = "enabled"
		m.start()
	}
	return nil
}

func controlField(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, ": "); ok && k == key {
			return v
		}
	}
	return ""
}

// runsNewBinary reports whether the service's process runs the binary now
// installed.
func (m *machine) runsNewBinary() bool {
	if m.unit.MainPID == 0 {
		return false
	}
	exe, err1 := os.Stat(filepath.Join(m.procRoot, strconv.Itoa(m.unit.MainPID), "exe"))
	bin, err2 := os.Stat(m.binaryPath)
	return err1 == nil && err2 == nil && os.SameFile(exe, bin)
}

// ran counts the commands run that start with prefix.
func (m *machine) ran(prefix string) int {
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// pkgFile stands in for a .deb: the machine's dpkg-deb reads it as the
// package's control file.
func pkgFile(pkg, version, arch string) []byte {
	return []byte(fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\n", pkg, version, arch))
}

// fakeHost provides the same package metadata and bytes as the vsock source.
// Transport corruption is covered by pkg/exporterupdate tests; these tests keep
// the updater's dpkg/systemd state machine independent of a socket.
type fakeHost struct {
	tag         string
	pkg         []byte
	size        int64
	metadataErr error
	downloadErr error
	badDigest   bool
	apiCalls    atomic.Int32
	downloads   atomic.Int32
}

func newFakeHost(tag string) *fakeHost {
	return &fakeHost{tag: tag, pkg: pkgFile(packageName, tag, "amd64")}
}

func (f *fakeHost) GetMetadata(context.Context) (exporterupdate.Metadata, error) {
	f.apiCalls.Add(1)
	if f.metadataErr != nil {
		return exporterupdate.Metadata{}, f.metadataErr
	}
	size := f.size
	if size == 0 {
		size = int64(len(f.pkg))
	}
	sum := sha256.Sum256(f.pkg)
	if f.badDigest {
		sum[0]++
	}
	return exporterupdate.Metadata{
		Version: f.tag,
		Size:    size,
		SHA256:  sum,
	}, nil
}

func (f *fakeHost) Download(_ context.Context, _ exporterupdate.Metadata, dst io.Writer) error {
	f.downloads.Add(1)
	if f.downloadErr != nil {
		return f.downloadErr
	}
	n, err := dst.Write(f.pkg)
	if err == nil && n != len(f.pkg) {
		return io.ErrShortWrite
	}
	return err
}

func newTestUpdater(t *testing.T, m *machine, source *fakeHost) (*Updater, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &Updater{
		self:              m.pkgVersion,
		source:            source,
		cmd:               m,
		out:               out,
		geteuid:           func() int { return 0 },
		getenv:            func(string) string { return "" },
		binaryPath:        m.binaryPath,
		procRoot:          m.procRoot,
		systemdRunDir:     t.TempDir(), // a systemd-booted machine
		serviceAccountDir: filepath.Join(t.TempDir(), "absent"),
		tempDir:           t.TempDir(),
		lockPath:          filepath.Join(t.TempDir(), "update.lock"),
	}, out
}

func TestUpdateInstallsNewerRelease(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	gh := newFakeHost(newTag)
	u, out := newTestUpdater(t, m, gh)

	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if len(m.installed) != 1 || !bytes.Equal(m.installed[0], gh.pkg) {
		t.Fatalf("dpkg -i got %q, want the host package", m.installed)
	}
	if m.pkgVersion != newTag {
		t.Errorf("installed version %q, want %s", m.pkgVersion, newTag)
	}
	if m.unit.UnitFileState != "enabled" || m.unit.ActiveState != "active" || !m.runsNewBinary() {
		t.Errorf("service %s, running the new binary: %v; want enabled and running it", m.unit, m.runsNewBinary())
	}
	// The package's own scripts restarted it; nothing more was called for.
	for _, verb := range []string{"enable", "disable", "start", "stop", "restart"} {
		if n := m.ran("systemctl " + verb + " "); n != 0 {
			t.Errorf("systemctl %s ran %d times", verb, n)
		}
	}
	if left, _ := os.ReadDir(u.tempDir); len(left) != 0 {
		t.Errorf("left behind in the temp dir: %v", left)
	}
	if !strings.Contains(out.String(), "updated "+oldTag+" -> "+newTag) {
		t.Errorf("output does not report the update:\n%s", out)
	}
}

func TestUpdateRestartsServiceLeftOnOldBinary(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	m.keepServiceOnUpgrade = true
	u, out := newTestUpdater(t, m, newFakeHost(newTag))

	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if n := m.ran("systemctl restart "); n != 1 {
		t.Errorf("systemctl restart ran %d times, want 1", n)
	}
	if m.unit.ActiveState != "active" || !m.runsNewBinary() {
		t.Errorf("service %s, running the new binary: %v", m.unit, m.runsNewBinary())
	}
}

func TestUpdateKeepsServiceAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name             string
		enabled, running bool
		force            bool
		wantErr          string // refused before downloading anything
	}{
		// A Kubernetes node: the DaemonSet runs the exporter, the service is
		// disabled and stopped, and installing the package would start it.
		{name: "disabled and stopped", wantErr: "-force"},
		{name: "disabled and stopped, forced", force: true},
		{name: "enabled but stopped", enabled: true, wantErr: "-force"},
		{name: "enabled but stopped, forced", enabled: true, force: true},
		{name: "disabled but running", running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, tc.enabled, tc.running)
			before := m.unit
			gh := newFakeHost(newTag)
			u, out := newTestUpdater(t, m, gh)
			u.Force = tc.force

			err := u.Update(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Update: %v, want an error mentioning %q\n%s", err, tc.wantErr, out)
				}
				if gh.downloads.Load() != 0 || len(m.installed) != 0 || m.unit != before {
					t.Errorf("refused, yet downloaded %d, installed %d, service %s -> %s",
						gh.downloads.Load(), len(m.installed), before, m.unit)
				}
				return
			}
			if err != nil {
				t.Fatalf("Update: %v\n%s", err, out)
			}
			if m.pkgVersion != newTag {
				t.Errorf("installed version %q, want %s", m.pkgVersion, newTag)
			}
			if m.unit.UnitFileState != before.UnitFileState || m.unit.ActiveState != before.ActiveState {
				t.Errorf("service %s, want it %s/%s as before", m.unit, before.UnitFileState, before.ActiveState)
			}
			if tc.running && !m.runsNewBinary() {
				t.Error("the service does not run the new binary")
			}
		})
	}
}

func TestUpdateNothingToDo(t *testing.T) {
	for _, tc := range []struct{ name, installed, want string }{
		{"up to date", newTag, "already up to date"},
		{"built past the latest release", newTag + "-4-gabc1234", "newer than the host package"},
		{"a later release than the latest", "2026.12.01-1", "newer than the host package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, tc.installed, true, true)
			gh := newFakeHost(newTag)
			u, out := newTestUpdater(t, m, gh)
			if err := u.Update(context.Background()); err != nil {
				t.Fatalf("Update: %v\n%s", err, out)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
			if gh.downloads.Load() != 0 || len(m.installed) != 0 || m.ran("systemctl") != 0 {
				t.Errorf("nothing to do, yet downloaded %d, installed %d, ran %q", gh.downloads.Load(), len(m.installed), m.calls)
			}
		})
	}
}

func TestUpdateForceReinstalls(t *testing.T) {
	m := newMachine(t, newTag, true, true)
	u, out := newTestUpdater(t, m, newFakeHost(newTag))
	u.Force = true
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if len(m.installed) != 1 || !m.runsNewBinary() {
		t.Errorf("installed %d packages, running the new binary: %v", len(m.installed), m.runsNewBinary())
	}
}

func TestUpdateFromBuildPastAnOlderTag(t *testing.T) {
	m := newMachine(t, oldTag+"-3-gabc1234", true, true)
	u, out := newTestUpdater(t, m, newFakeHost(newTag))
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if m.pkgVersion != newTag {
		t.Errorf("installed version %q, want %s", m.pkgVersion, newTag)
	}
}

func TestUpdateIncomparableVersionNeedsForce(t *testing.T) {
	m := newMachine(t, "0.0.0-dev", true, true) // build-deb.sh's fallback
	gh := newFakeHost(newTag)
	u, out := newTestUpdater(t, m, gh)
	if err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("Update: %v, want a refusal pointing at -force\n%s", err, out)
	}
	if gh.downloads.Load() != 0 || len(m.installed) != 0 {
		t.Fatalf("refused, yet downloaded %d, installed %d", gh.downloads.Load(), len(m.installed))
	}
	u.Force = true
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update -force: %v\n%s", err, out)
	}
	if m.pkgVersion != newTag {
		t.Errorf("installed version %q, want %s", m.pkgVersion, newTag)
	}
}

// Host metadata and control-file mismatches must stop the update before dpkg.
func TestUpdateRejectsBadHostPackage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeHost)
		want  string
	}{
		{"implausible size", func(f *fakeHost) {
			f.size = exporterupdate.MaxPackageSize + 1
		}, "invalid size"},
		{"short download", func(f *fakeHost) {
			f.size = int64(len(f.pkg) + 1)
		}, "size"},
		{"digest mismatch", func(f *fakeHost) {
			f.badDigest = true
		}, "does not match"},
		{"host version is not a release", func(f *fakeHost) {
			f.tag = newTag + "-1-gabc1234"
			f.pkg = pkgFile(packageName, f.tag, "amd64")
		}, "not a release version"},
		{"transfer failed", func(f *fakeHost) {
			f.downloadErr = errors.New("vsock transfer broke")
		}, "vsock transfer broke"},
		{"package of another version", func(f *fakeHost) { f.pkg = pkgFile(packageName, oldTag, "amd64") }, "Version"},
		{"another package", func(f *fakeHost) { f.pkg = pkgFile("gpu-metrics-receiver", newTag, "amd64") }, "Package"},
		{"another architecture", func(f *fakeHost) { f.pkg = pkgFile(packageName, newTag, "arm64") }, "Architecture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			before := m.unit
			gh := newFakeHost(newTag)
			tc.setup(gh)
			u, out := newTestUpdater(t, m, gh)

			err := u.Update(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Update: %v, want an error mentioning %q\n%s", err, tc.want, out)
			}
			if len(m.installed) != 0 || m.pkgVersion != oldTag || m.unit != before {
				t.Errorf("rejected, yet installed %d, version %s, service %s -> %s", len(m.installed), m.pkgVersion, before, m.unit)
			}
			if left, _ := os.ReadDir(u.tempDir); len(left) != 0 {
				t.Errorf("left behind in the temp dir: %v", left)
			}
		})
	}
}

func TestUpdateRefusesKubernetesPod(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Updater)
	}{
		{"service host in the environment", func(_ *testing.T, u *Updater) {
			u.getenv = func(k string) string {
				if k == "KUBERNETES_SERVICE_HOST" {
					return "10.96.0.1"
				}
				return ""
			}
		}},
		{"service account mounted", func(t *testing.T, u *Updater) {
			u.serviceAccountDir = t.TempDir()
		}},
	} {
		for _, check := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, check %v", tc.name, check), func(t *testing.T) {
				m := newMachine(t, oldTag, true, true)
				gh := newFakeHost(newTag)
				u, out := newTestUpdater(t, m, gh)
				u.CheckOnly = check
				tc.setup(t, u)

				err := u.Update(context.Background())
				if err == nil || !strings.Contains(err.Error(), "DaemonSet") {
					t.Fatalf("Update: %v, want a refusal pointing at the DaemonSet\n%s", err, out)
				}
				if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
					t.Errorf("refused, yet contacted host %d times and ran %q", gh.apiCalls.Load(), m.calls)
				}
			})
		}
	}
}

func TestUpdateNeedsRootAndSystemd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Updater)
		want  string
	}{
		{"not root", func(_ *testing.T, u *Updater) { u.geteuid = func() int { return 1000 } }, "root"},
		{"no systemd", func(t *testing.T, u *Updater) { u.systemdRunDir = filepath.Join(t.TempDir(), "absent") }, "systemd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			gh := newFakeHost(newTag)
			u, out := newTestUpdater(t, m, gh)
			tc.setup(t, u)
			if err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Update: %v, want an error mentioning %q\n%s", err, tc.want, out)
			}
			if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
				t.Errorf("refused, yet contacted host %d times and ran %q", gh.apiCalls.Load(), m.calls)
			}
		})
	}
}

func TestUpdateNeedsInstalledPackage(t *testing.T) {
	for _, status := range []string{"", "deinstall ok config-files", "install ok half-configured"} {
		t.Run(fmt.Sprintf("status %q", status), func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			m.pkgStatus = status
			gh := newFakeHost(newTag)
			u, out := newTestUpdater(t, m, gh)
			if err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "Install the initial .deb") {
				t.Fatalf("Update: %v, want a refusal\n%s", err, out)
			}
			if gh.apiCalls.Load() != 0 || len(m.installed) != 0 {
				t.Errorf("refused, yet contacted host %d times, installed %d", gh.apiCalls.Load(), len(m.installed))
			}
		})
	}
}

func TestUpdateCheckOnly(t *testing.T) {
	t.Run("installed package", func(t *testing.T) {
		m := newMachine(t, oldTag, true, true)
		gh := newFakeHost(newTag)
		u, out := newTestUpdater(t, m, gh)
		u.CheckOnly = true
		u.geteuid = func() int { return 1000 } // needs no root
		if err := u.Update(context.Background()); err != nil {
			t.Fatalf("Update: %v\n%s", err, out)
		}
		if want := "update available: " + oldTag + " -> " + newTag; !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
		if gh.downloads.Load() != 0 || len(m.installed) != 0 || m.ran("systemctl") != 0 {
			t.Errorf("only checking, yet downloaded %d, installed %d, ran %q", gh.downloads.Load(), len(m.installed), m.calls)
		}
	})
	t.Run("no package: compares this binary", func(t *testing.T) {
		m := newMachine(t, oldTag, true, true)
		m.pkgStatus = ""
		u, out := newTestUpdater(t, m, newFakeHost(newTag))
		u.CheckOnly = true
		u.self = oldTag + "-2-gabc1234"
		if err := u.Update(context.Background()); err != nil {
			t.Fatalf("Update: %v\n%s", err, out)
		}
		if want := "update available: " + u.self + " -> " + newTag; !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	})
}

func TestUpdateReportsHostErrors(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	gh := newFakeHost(newTag)
	gh.metadataErr = errors.New("host update service unavailable")
	u, out := newTestUpdater(t, m, gh)
	err := u.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "host update service unavailable") {
		t.Fatalf("Update error %v\n%s", err, out)
	}
	if gh.downloads.Load() != 0 || len(m.installed) != 0 {
		t.Fatalf("metadata failed, yet downloaded %d and installed %d", gh.downloads.Load(), len(m.installed))
	}
}

func TestUpdateDpkgFailure(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	m.dpkgErr = errors.New("dpkg -i: exit status 1")
	u, out := newTestUpdater(t, m, newFakeHost(newTag))
	err := u.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dpkg --audit") {
		t.Fatalf("Update: %v, want the dpkg failure\n%s", err, out)
	}
	// What decides whether the auto-update unit shows as failed.
	var ie *InstallError
	if !errors.As(err, &ie) {
		t.Errorf("a dpkg failure is not an InstallError: %v", err)
	}
	// Whatever dpkg left, the update does not guess at it.
	for _, verb := range []string{"enable", "disable", "start", "stop", "restart"} {
		if n := m.ran("systemctl " + verb + " "); n != 0 {
			t.Errorf("systemctl %s ran %d times after dpkg failed", verb, n)
		}
	}
}

// What fails once dpkg has run may have changed the system, and must read as
// an InstallError, or `update -auto` would exit ExitTempFail and the unit
// would show a broken install as a success.
func TestUpdateFailuresAfterDpkgAreInstallErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*machine)
		want  string
	}{
		{"service not put back", func(m *machine) {
			m.keepServiceOnUpgrade = true // so it has to be restarted
			m.verbErr = map[string]error{"restart": errors.New("systemctl restart: exit status 1")}
		}, "putting the service back failed"},
		{"dpkg records another version", func(m *machine) { m.installAs = oldTag }, "dpkg reports"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			tc.setup(m)
			u, out := newTestUpdater(t, m, newFakeHost(newTag))
			u.Auto = true
			err := u.Update(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Update: %v, want an error mentioning %q\n%s", err, tc.want, out)
			}
			var ie *InstallError
			if !errors.As(err, &ie) {
				t.Errorf("%v is not an InstallError, yet dpkg had run", err)
			}
		})
	}
}

func TestAutoUpdateStatusRefusesKubernetesPod(t *testing.T) {
	m := newMachine(t, newTag, true, true)
	u, out := newTestUpdater(t, m, newFakeHost(newTag))
	u.getenv = func(k string) string {
		if k == "KUBERNETES_SERVICE_HOST" {
			return "10.96.0.1"
		}
		return ""
	}
	if err := u.AutoUpdateStatus(); err == nil || !strings.Contains(err.Error(), "DaemonSet") {
		t.Fatalf("AutoUpdateStatus: %v, want a refusal pointing at the DaemonSet\n%s", err, out)
	}
	if len(m.calls) != 0 {
		t.Errorf("refused, yet ran %q", m.calls)
	}
}

// Everything that fails before dpkg runs leaves the system as it was, and
// must not read as an InstallError: `update -auto` exits ExitTempFail on it.
func TestUpdateFailuresBeforeDpkgAreNotInstallErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*machine, *fakeHost)
	}{
		{"host unreachable", func(_ *machine, f *fakeHost) { f.metadataErr = errors.New("host unreachable") }},
		{"transfer failed", func(_ *machine, f *fakeHost) { f.downloadErr = errors.New("transfer failed") }},
		{"package of another version", func(_ *machine, f *fakeHost) { f.pkg = pkgFile(packageName, oldTag, "amd64") }},
		{"no package installed", func(m *machine, _ *fakeHost) { m.pkgStatus = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			gh := newFakeHost(newTag)
			tc.setup(m, gh)
			u, out := newTestUpdater(t, m, gh)
			u.Auto = true
			err := u.Update(context.Background())
			if err == nil {
				t.Fatalf("Update succeeded\n%s", out)
			}
			var ie *InstallError
			if errors.As(err, &ie) {
				t.Errorf("%v is an InstallError, yet dpkg never ran", err)
			}
		})
	}
}

func TestUpdateAutoLeavesStoppedServiceAlone(t *testing.T) {
	m := newMachine(t, oldTag, false, false) // a Kubernetes node: the DaemonSet runs the exporter
	before := m.unit
	gh := newFakeHost(newTag)
	u, out := newTestUpdater(t, m, gh)
	u.Auto = true
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v, want a quiet skip\n%s", err, out)
	}
	if !strings.Contains(out.String(), "leaves a stopped exporter alone") {
		t.Errorf("output does not say why it skipped:\n%s", out)
	}
	if gh.downloads.Load() != 0 || len(m.installed) != 0 || m.unit != before {
		t.Errorf("skipped, yet downloaded %d, installed %d, service %s -> %s", gh.downloads.Load(), len(m.installed), before, m.unit)
	}
}

func TestUpdateAutoInstallsLikeUpdate(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	u, out := newTestUpdater(t, m, newFakeHost(newTag))
	u.Auto = true
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if m.pkgVersion != newTag || !m.runsNewBinary() {
		t.Errorf("installed %s, running the new binary: %v", m.pkgVersion, m.runsNewBinary())
	}
}

func TestUpdateAutoTakesNoForceOrCheck(t *testing.T) {
	for _, set := range []func(*Updater){
		func(u *Updater) { u.Force = true },
		func(u *Updater) { u.CheckOnly = true },
	} {
		m := newMachine(t, oldTag, false, false)
		gh := newFakeHost(newTag)
		u, out := newTestUpdater(t, m, gh)
		u.Auto = true
		set(u)
		if err := u.Update(context.Background()); err == nil {
			t.Fatalf("Update succeeded\n%s", out)
		}
		if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
			t.Errorf("refused, yet contacted host %d times and ran %q", gh.apiCalls.Load(), m.calls)
		}
	}
}

// A manual update and the timer's must not run at once.
func TestUpdateHoldsTheLock(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	gh := newFakeHost(newTag)
	u, out := newTestUpdater(t, m, gh)

	// Another update holds the lock: a second open file, as another process has.
	f, err := os.OpenFile(u.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "another update is running") {
		t.Fatalf("Update: %v, want it locked out\n%s", err, out)
	}
	if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
		t.Errorf("locked out, yet contacted host %d times and ran %q", gh.apiCalls.Load(), m.calls)
	}

	// Checking changes nothing, so it does not wait for the lock.
	u.CheckOnly = true
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update -check: %v\n%s", err, out)
	}

	// Once the other update is done, this one goes ahead.
	_ = f.Close()
	u.CheckOnly = false
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if m.pkgVersion != newTag {
		t.Errorf("installed %s, want %s", m.pkgVersion, newTag)
	}
}

func TestSetAutoUpdate(t *testing.T) {
	m := newMachine(t, newTag, true, true)
	u, out := newTestUpdater(t, m, newFakeHost(newTag))

	if err := u.SetAutoUpdate(false); err != nil {
		t.Fatalf("SetAutoUpdate(false): %v\n%s", err, out)
	}
	if m.timer.UnitFileState != "disabled" || m.timer.ActiveState != "inactive" {
		t.Errorf("timer %s, want disabled and stopped", m.timer)
	}
	if !strings.Contains(out.String(), "auto-update: disabled") {
		t.Errorf("output does not report it disabled:\n%s", out)
	}

	out.Reset()
	if err := u.SetAutoUpdate(true); err != nil {
		t.Fatalf("SetAutoUpdate(true): %v\n%s", err, out)
	}
	if m.timer.UnitFileState != "enabled" || m.timer.ActiveState != "active" {
		t.Errorf("timer %s, want enabled and started", m.timer)
	}
	if !strings.Contains(out.String(), "auto-update: enabled") || !strings.Contains(out.String(), "7h left") {
		t.Errorf("output does not report it enabled, with the next check:\n%s", out)
	}
	// The exporter's own service is none of its business.
	if n := m.ran("systemctl enable " + serviceName); n != 0 {
		t.Errorf("touched %s", serviceName)
	}
}

func TestSetAutoUpdateRefuses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *machine, *Updater)
		want  string
	}{
		{"in a Kubernetes pod", func(_ *testing.T, _ *machine, u *Updater) {
			u.getenv = func(k string) string {
				if k == "KUBERNETES_SERVICE_HOST" {
					return "10.96.0.1"
				}
				return ""
			}
		}, "DaemonSet"},
		{"not root", func(_ *testing.T, _ *machine, u *Updater) { u.geteuid = func() int { return 1000 } }, "sudo gpu-metrics-exporter auto-update"},
		{"no systemd", func(t *testing.T, _ *machine, u *Updater) {
			u.systemdRunDir = filepath.Join(t.TempDir(), "absent")
		}, "systemd"},
		{"package predates auto-update", func(_ *testing.T, m *machine, _ *Updater) {
			m.timer = unitState{LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}
		}, "predates auto-update"},
	} {
		for _, enable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, enable %v", tc.name, enable), func(t *testing.T) {
				m := newMachine(t, newTag, true, true)
				u, out := newTestUpdater(t, m, newFakeHost(newTag))
				tc.setup(t, m, u)
				err := u.SetAutoUpdate(enable)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("SetAutoUpdate(%v): %v, want an error mentioning %q\n%s", enable, err, tc.want, out)
				}
				if n := m.ran("systemctl enable") + m.ran("systemctl disable"); n != 0 {
					t.Errorf("refused, yet ran %q", m.calls)
				}
			})
		}
	}
}

func TestAutoUpdateStatus(t *testing.T) {
	const at = "Thu 2026-11-19 21:13:00 UTC"
	for _, tc := range []struct {
		name      string
		timer     unitState
		lastCheck map[string]string
		want      []string
	}{
		{"not installed", unitState{LoadState: "not-found", ActiveState: "inactive"}, nil,
			[]string{"auto-update: not installed", "predates it"}},
		{"enabled, not run since boot",
			unitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active", SubState: "waiting"},
			map[string]string{"Result": "success", "ExecMainStatus": "0", "ExecMainExitTimestamp": ""},
			[]string{"auto-update: enabled", "NEXT", "7h left", "last check: none since boot", "journalctl -u " + updateServiceName}},
		{"disabled", unitState{LoadState: "loaded", UnitFileState: "disabled", ActiveState: "inactive", SubState: "dead"},
			map[string]string{"Result": "success", "ExecMainStatus": "0", "ExecMainExitTimestamp": "n/a"},
			[]string{"auto-update: disabled", "last check: none since boot"}},
		{"last check completed",
			unitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active", SubState: "waiting"},
			map[string]string{"Result": "success", "ExecMainStatus": "0", "ExecMainExitTimestamp": at},
			[]string{"last check: " + at + ": completed"}},
		{"last check could not finish",
			unitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active", SubState: "waiting"},
			map[string]string{"Result": "success", "ExecMainStatus": "75", "ExecMainExitTimestamp": at},
			[]string{"last check: " + at + ": could not finish, and changed nothing"}},
		{"last check failed",
			unitState{LoadState: "loaded", UnitFileState: "enabled", ActiveState: "active", SubState: "waiting"},
			map[string]string{"Result": "exit-code", "ExecMainStatus": "1", "ExecMainExitTimestamp": at},
			[]string{"last check: " + at + ": failed (exit-code, exit status 1)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, newTag, true, true)
			m.timer = tc.timer
			if tc.lastCheck != nil {
				m.lastCheck = tc.lastCheck
			}
			u, out := newTestUpdater(t, m, newFakeHost(newTag))
			u.geteuid = func() int { return 1000 } // needs no root
			if err := u.AutoUpdateStatus(); err != nil {
				t.Fatalf("AutoUpdateStatus: %v\n%s", err, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out.String(), "timers listed") {
				t.Errorf("output keeps systemctl's footer:\n%s", out)
			}
		})
	}
}

// Without root, systemctl goes through D-Bus; where there is none, say what
// would work rather than leave the user with "Failed to connect to bus".
func TestAutoUpdateStatusWithoutDBus(t *testing.T) {
	for _, tc := range []struct {
		euid     int
		wantHint bool
	}{{1000, true}, {0, false}} {
		m := newMachine(t, newTag, true, true)
		m.showErr = errors.New("systemctl show: exit status 1: Failed to connect to bus: No such file or directory")
		u, _ := newTestUpdater(t, m, newFakeHost(newTag))
		u.geteuid = func() int { return tc.euid }
		err := u.AutoUpdateStatus()
		if err == nil {
			t.Fatalf("euid %d: AutoUpdateStatus succeeded", tc.euid)
		}
		if got := strings.Contains(err.Error(), "try it with sudo"); got != tc.wantHint {
			t.Errorf("euid %d: %q; want the sudo hint: %v", tc.euid, err, tc.wantHint)
		}
	}
}

func TestUpdateOverRealUpdateProtocol(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	u, out := newTestUpdater(t, m, newFakeHost(newTag))
	pkgBytes := pkgFile(packageName, newTag, "amd64")
	path := filepath.Join(t.TempDir(), "exporter.deb")
	if err := os.WriteFile(path, pkgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := exporterupdate.OpenPackage(path, newTag)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pkg.Close() })
	server := &exporterupdate.Server{Package: pkg}
	u.source = exporterupdate.NewClientWithDial(func() (*common.VsockConn, error) {
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			return nil, err
		}
		clientConn := common.NewVsockConn(fds[0])
		serverConn := common.NewVsockConn(fds[1])
		done := make(chan error, 1)
		go func() {
			defer func() { _ = serverConn.Close() }()
			first, err := serverConn.RecvData()
			if err != nil {
				done <- err
				return
			}
			done <- server.Serve(serverConn, first, 2*time.Second, 2*time.Second)
		}()
		t.Cleanup(func() {
			_ = clientConn.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("real update protocol server: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("real update protocol server did not stop")
			}
		})
		return clientConn, nil
	})

	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update over real protocol: %v\n%s", err, out)
	}
	if len(m.installed) != 1 || !bytes.Equal(m.installed[0], pkgBytes) {
		t.Fatalf("dpkg installed %q, want host package bytes", m.installed)
	}
	if m.pkgVersion != newTag || !m.runsNewBinary() {
		t.Fatalf("installed %s, running new binary %v", m.pkgVersion, m.runsNewBinary())
	}
}
