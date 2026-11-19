package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	testRepo = "owner/repo"
	oldTag   = "2026.10.06-1"
	newTag   = "2026.11.20-1"
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

// fakeGitHub serves the latest-release API and the release's download URLs,
// which -- as on github.com -- redirect to the asset storage.
type fakeGitHub struct {
	tag          string
	pkg          []byte                  // the exporter .deb's bytes
	mutate       func(*release, *string) // edits the release before it is served; gets the request host
	status       int                     // answer the API with this instead
	header       map[string]string
	redirectHTTP bool // send the download to plain http
	apiCalls     atomic.Int32
	downloads    atomic.Int32
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+testRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.apiCalls.Add(1)
		if !strings.HasPrefix(r.UserAgent(), "gpu-metrics-exporter/") {
			t.Errorf("User-Agent %q", r.UserAgent())
		}
		for k, v := range f.header {
			w.Header().Set(k, v)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"message":"something went wrong"}`))
			return
		}
		sum := sha256.Sum256(f.pkg)
		receiver := []byte("receiver")
		rsum := sha256.Sum256(receiver)
		name := "gpu-metrics-exporter_" + f.tag + "_amd64.deb"
		rel := release{
			TagName: f.tag,
			HTMLURL: "https://" + r.Host + "/" + testRepo + "/releases/tag/" + f.tag,
			Assets: []asset{{
				Name: "gpu-metrics-receiver_" + f.tag + "_amd64.deb", State: "uploaded", Size: int64(len(receiver)),
				Digest:             "sha256:" + hex.EncodeToString(rsum[:]),
				BrowserDownloadURL: "https://" + r.Host + "/download/gpu-metrics-receiver_" + f.tag + "_amd64.deb",
			}, {
				Name: name, State: "uploaded", Size: int64(len(f.pkg)),
				Digest:             "sha256:" + hex.EncodeToString(sum[:]),
				BrowserDownloadURL: "https://" + r.Host + "/download/" + name,
			}},
		}
		if f.mutate != nil {
			f.mutate(&rel, &r.Host)
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		scheme := "https://"
		if f.redirectHTTP {
			scheme = "http://"
		}
		http.Redirect(w, r, scheme+r.Host+"/objects/"+path.Base(r.URL.Path), http.StatusFound)
	})
	mux.HandleFunc("/objects/", func(w http.ResponseWriter, r *http.Request) {
		f.downloads.Add(1)
		if strings.Contains(r.URL.Path, "receiver") {
			_, _ = w.Write([]byte("receiver"))
			return
		}
		_, _ = w.Write(f.pkg)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newGitHub(tag string) *fakeGitHub {
	return &fakeGitHub{tag: tag, pkg: pkgFile(packageName, tag, "amd64")}
}

func newTestUpdater(t *testing.T, m *machine, srv *httptest.Server) (*Updater, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &Updater{
		self:              m.pkgVersion,
		repo:              testRepo,
		apiBase:           srv.URL,
		http:              newHTTPClient(srv.Client().Transport),
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
	gh := newGitHub(newTag)
	u, out := newTestUpdater(t, m, gh.serve(t))

	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if len(m.installed) != 1 || !bytes.Equal(m.installed[0], gh.pkg) {
		t.Fatalf("dpkg -i got %q, want the release's package", m.installed)
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
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))

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
			gh := newGitHub(newTag)
			u, out := newTestUpdater(t, m, gh.serve(t))
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
		{"built past the latest release", newTag + "-4-gabc1234", "newer than the latest release"},
		{"a later release than the latest", "2026.12.01-1", "newer than the latest release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, tc.installed, true, true)
			gh := newGitHub(newTag)
			u, out := newTestUpdater(t, m, gh.serve(t))
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
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if m.pkgVersion != newTag {
		t.Errorf("installed version %q, want %s", m.pkgVersion, newTag)
	}
}

func TestUpdateIncomparableVersionNeedsForce(t *testing.T) {
	m := newMachine(t, "0.0.0-dev", true, true) // build-deb.sh's fallback
	gh := newGitHub(newTag)
	u, out := newTestUpdater(t, m, gh.serve(t))
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

// Every way the release can fail to be the package it claims must stop the
// update before dpkg sees the file.
func TestUpdateRejectsBadRelease(t *testing.T) {
	exporterAsset := func(rel *release) *asset { return &rel.Assets[1] }
	for _, tc := range []struct {
		name  string
		setup func(*fakeGitHub)
		want  string
	}{
		{"digest mismatch", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) {
				sum := sha256.Sum256([]byte("something else"))
				exporterAsset(rel).Digest = "sha256:" + hex.EncodeToString(sum[:])
			}
		}, "does not match"},
		{"no digest", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { exporterAsset(rel).Digest = "" }
		}, "no sha256 digest"},
		{"another digest algorithm", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) {
				exporterAsset(rel).Digest = "sha1:0123456789abcdef0123456789abcdef01234567"
			}
		}, "no sha256 digest"},
		{"shorter than listed", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { exporterAsset(rel).Size += 10 }
		}, "GitHub lists"},
		{"longer than listed", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { exporterAsset(rel).Size-- }
		}, "GitHub lists"},
		{"implausible size", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { exporterAsset(rel).Size = maxPackageSize + 1 }
		}, "implausible size"},
		{"http download URL", func(f *fakeGitHub) {
			f.mutate = func(rel *release, host *string) {
				exporterAsset(rel).BrowserDownloadURL = "http://" + *host + "/objects/" + exporterAsset(rel).Name
			}
		}, "not an https URL"},
		{"redirect to http", func(f *fakeGitHub) { f.redirectHTTP = true }, "not https"},
		{"no exporter package", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { rel.Assets = rel.Assets[:1] }
		}, "has no gpu-metrics-exporter_" + newTag + "_amd64.deb"},
		{"upload unfinished", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { exporterAsset(rel).State = "open" }
		}, "not uploaded"},
		{"draft", func(f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { rel.Draft = true }
		}, "draft"},
		{"tag is not a release version", func(f *fakeGitHub) {
			f.tag = newTag + "-1-gabc1234"
			f.pkg = pkgFile(packageName, f.tag, "amd64")
		}, "not a release version"},
		// An old, genuine package republished under a newer tag: its digest
		// matches, its control file gives it away.
		{"package of another version", func(f *fakeGitHub) { f.pkg = pkgFile(packageName, oldTag, "amd64") }, "Version"},
		{"another package", func(f *fakeGitHub) { f.pkg = pkgFile("gpu-metrics-receiver", newTag, "amd64") }, "Package"},
		{"another architecture", func(f *fakeGitHub) { f.pkg = pkgFile(packageName, newTag, "arm64") }, "Architecture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			before := m.unit
			gh := newGitHub(newTag)
			tc.setup(gh)
			u, out := newTestUpdater(t, m, gh.serve(t))

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
				gh := newGitHub(newTag)
				u, out := newTestUpdater(t, m, gh.serve(t))
				u.CheckOnly = check
				tc.setup(t, u)

				err := u.Update(context.Background())
				if err == nil || !strings.Contains(err.Error(), "DaemonSet") {
					t.Fatalf("Update: %v, want a refusal pointing at the DaemonSet\n%s", err, out)
				}
				if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
					t.Errorf("refused, yet queried GitHub %d times and ran %q", gh.apiCalls.Load(), m.calls)
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
			gh := newGitHub(newTag)
			u, out := newTestUpdater(t, m, gh.serve(t))
			tc.setup(t, u)
			if err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Update: %v, want an error mentioning %q\n%s", err, tc.want, out)
			}
			if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
				t.Errorf("refused, yet queried GitHub %d times and ran %q", gh.apiCalls.Load(), m.calls)
			}
		})
	}
}

func TestUpdateNeedsInstalledPackage(t *testing.T) {
	for _, status := range []string{"", "deinstall ok config-files", "install ok half-configured"} {
		t.Run(fmt.Sprintf("status %q", status), func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			m.pkgStatus = status
			gh := newGitHub(newTag)
			u, out := newTestUpdater(t, m, gh.serve(t))
			if err := u.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "Install the .deb") {
				t.Fatalf("Update: %v, want a refusal\n%s", err, out)
			}
			if gh.apiCalls.Load() != 0 || len(m.installed) != 0 {
				t.Errorf("refused, yet queried GitHub %d times, installed %d", gh.apiCalls.Load(), len(m.installed))
			}
		})
	}
}

func TestUpdateCheckOnly(t *testing.T) {
	t.Run("installed package", func(t *testing.T) {
		m := newMachine(t, oldTag, true, true)
		gh := newGitHub(newTag)
		u, out := newTestUpdater(t, m, gh.serve(t))
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
		u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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

func TestUpdateReportsGitHubErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		want   []string
	}{
		{"rate limited", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1790000000"},
			[]string{"rate limit", "resets at 2026-09-21T"}},
		{"forbidden", http.StatusForbidden, nil, []string{"403", "something went wrong"}},
		{"no release", http.StatusNotFound, nil, []string{"has no published release"}},
		{"server error", http.StatusBadGateway, nil, []string{"502"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			gh := newGitHub(newTag)
			gh.status, gh.header = tc.status, tc.header
			u, out := newTestUpdater(t, m, gh.serve(t))
			err := u.Update(context.Background())
			if err == nil {
				t.Fatalf("Update succeeded\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

func TestUpdateDpkgFailure(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	m.dpkgErr = errors.New("dpkg -i: exit status 1")
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
			u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
		setup func(*machine, *fakeGitHub)
	}{
		{"GitHub unreachable", func(_ *machine, f *fakeGitHub) { f.status = http.StatusBadGateway }},
		{"digest mismatch", func(_ *machine, f *fakeGitHub) {
			f.mutate = func(rel *release, _ *string) { rel.Assets[1].Digest = "sha256:" + strings.Repeat("00", 32) }
		}},
		{"package of another version", func(_ *machine, f *fakeGitHub) { f.pkg = pkgFile(packageName, oldTag, "amd64") }},
		{"no package installed", func(m *machine, _ *fakeGitHub) { m.pkgStatus = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, oldTag, true, true)
			gh := newGitHub(newTag)
			tc.setup(m, gh)
			u, out := newTestUpdater(t, m, gh.serve(t))
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
	gh := newGitHub(newTag)
	u, out := newTestUpdater(t, m, gh.serve(t))
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
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
		gh := newGitHub(newTag)
		u, out := newTestUpdater(t, m, gh.serve(t))
		u.Auto = true
		set(u)
		if err := u.Update(context.Background()); err == nil {
			t.Fatalf("Update succeeded\n%s", out)
		}
		if gh.apiCalls.Load() != 0 || len(m.calls) != 0 {
			t.Errorf("refused, yet queried GitHub %d times and ran %q", gh.apiCalls.Load(), m.calls)
		}
	}
}

// A manual update and the timer's must not run at once.
func TestUpdateHoldsTheLock(t *testing.T) {
	m := newMachine(t, oldTag, true, true)
	gh := newGitHub(newTag)
	u, out := newTestUpdater(t, m, gh.serve(t))

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
		t.Errorf("locked out, yet queried GitHub %d times and ran %q", gh.apiCalls.Load(), m.calls)
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
	u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))

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
				u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
			u, out := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
		u, _ := newTestUpdater(t, m, newGitHub(newTag).serve(t))
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
