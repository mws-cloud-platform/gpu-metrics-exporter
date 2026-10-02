package gpumetricsexporter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeProcess lays out, in a temp dir, what nvmlLibraryInspector reads from a
// real one: /proc/self/{maps,status,map_files}, /etc/ld.so.preload, and the
// files the mappings name. Everything is owned by the test's user, which the
// inspector is told to take for root, and the ownership walk stops at the
// temp dir.
type fakeProcess struct {
	t      *testing.T
	root   string
	proc   string
	sysDir string // stands in for /usr/lib/x86_64-linux-gnu
	nvDir  string // stands in for the directory libnvidia-ml comes from
	exe    string
	nvml   string // the libnvidia-ml file
	env    map[string]string
	maps   []string
	nextAt uint64
}

const genuineNVML = "the genuine libnvidia-ml"

func newFakeProcess(t *testing.T) *fakeProcess {
	t.Helper()
	root := t.TempDir()
	p := &fakeProcess{
		t:      t,
		root:   root,
		proc:   filepath.Join(root, "proc", "self"),
		sysDir: filepath.Join(root, "usr", "lib"),
		nvDir:  filepath.Join(root, "nvidia"),
		exe:    filepath.Join(root, "bin", "gpu-metrics-exporter"),
		env:    map[string]string{},
		nextAt: 0x7f0000000000,
	}
	for _, d := range []string{p.proc, p.sysDir, p.nvDir, filepath.Dir(p.exe), filepath.Join(root, "etc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p.write(filepath.Join(p.proc, "status"), "Name:\tgpu-metrics-exp\nTracerPid:\t0\n", 0o644)
	p.nvml = filepath.Join(p.nvDir, "libnvidia-ml.so.550.54.15")
	p.write(p.nvml, genuineNVML, 0o644)
	p.write(p.exe, "exporter", 0o755)
	p.write(filepath.Join(p.sysDir, "libc.so.6"), "libc", 0o644)
	p.write(filepath.Join(p.sysDir, "ld-linux-x86-64.so.2"), "ld.so", 0o644)
	// A healthy exporter process: itself, the C runtime, libnvidia-ml, and
	// the usual non-code mappings around them.
	p.mapFile("r-xp", p.exe)
	p.mapFile("r-xp", filepath.Join(p.sysDir, "libc.so.6"))
	p.mapFile("r--p", filepath.Join(p.sysDir, "libc.so.6"))
	p.mapFile("r-xp", filepath.Join(p.sysDir, "ld-linux-x86-64.so.2"))
	p.mapFile("r-xp", p.nvml)
	p.mapFile("r--p", "/usr/lib/locale/locale-archive")
	p.maps = append(p.maps,
		"7ffd00000000-7ffd00002000 r-xp 00000000 00:00 0                          [vdso]",
		"c000000000-c004000000 rw-p 00000000 00:00 0 ")
	return p
}

func (p *fakeProcess) write(name, content string, mode os.FileMode) {
	p.t.Helper()
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil { // past the umask
		p.t.Fatal(err)
	}
}

// mapFile adds a mapping of name and returns its address range.
func (p *fakeProcess) mapFile(perms, name string) string {
	addr := fmt.Sprintf("%x-%x", p.nextAt, p.nextAt+0x1000)
	p.nextAt += 0x10000
	p.maps = append(p.maps, fmt.Sprintf("%s %s 00000000 fe:01 %d %s", addr, perms, len(p.maps)+1, name))
	return addr
}

// linkMapFile makes /proc/self/map_files/<addr> lead to target, as the
// kernel's link leads to the inode actually mapped.
func (p *fakeProcess) linkMapFile(addr, target string) {
	p.t.Helper()
	dir := filepath.Join(p.proc, "map_files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, addr)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *fakeProcess) inspector() *nvmlLibraryInspector {
	p.t.Helper()
	p.write(filepath.Join(p.proc, "maps"), strings.Join(p.maps, "\n")+"\n", 0o644)
	return &nvmlLibraryInspector{
		procSelf:    p.proc,
		preloadFile: filepath.Join(p.root, "etc", "ld.so.preload"),
		getenv:      func(k string) string { return p.env[k] },
		exe:         p.exe,
		systemDirs:  map[string]bool{p.sysDir: true},
		nvmlDirs:    map[string]bool{p.nvDir: true},
		rootUID:     uint32(os.Getuid()),
		chainTop:    p.root,
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hasProblem(problems []string, prefix string) bool {
	return slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, prefix) })
}

// TestNVMLLibraryClean is a healthy process: the library is measured and
// nothing is flagged.
func TestNVMLLibraryClean(t *testing.T) {
	p := newFakeProcess(t)
	lib := p.inspector().inspect()

	if lib.Error != "" || len(lib.Problems) != 0 {
		t.Fatalf("healthy process: error %q, problems %q", lib.Error, lib.Problems)
	}
	if lib.Path != p.nvml {
		t.Errorf("path = %q, want %q", lib.Path, p.nvml)
	}
	if lib.SHA256 != digest(genuineNVML) || lib.Size != int64(len(genuineNVML)) {
		t.Errorf("sha256 %s size %d, want the library file's", lib.SHA256, lib.Size)
	}
	if lib.HashedFrom != "path" {
		t.Errorf("hashed_from = %q without map_files, want path", lib.HashedFrom)
	}
}

// TestNVMLLibraryHashesTheMappedInode: through map_files the digest is of what
// is mapped, even after the path got a different file -- and that is no
// problem in itself, since a driver upgrade does exactly that.
func TestNVMLLibraryHashesTheMappedInode(t *testing.T) {
	p := newFakeProcess(t)
	mapped := filepath.Join(p.root, "mapped-inode")
	p.write(mapped, genuineNVML, 0o644)
	p.write(p.nvml, "a newer build on disk", 0o644)
	p.maps = slices.DeleteFunc(p.maps, func(l string) bool { return strings.HasSuffix(l, p.nvml) })
	addr := p.mapFile("r-xp", p.nvml+deletedSuffix)
	p.linkMapFile(addr, mapped)

	lib := p.inspector().inspect()
	if lib.Error != "" || len(lib.Problems) != 0 {
		t.Fatalf("replaced on disk: error %q, problems %q", lib.Error, lib.Problems)
	}
	if lib.SHA256 != digest(genuineNVML) || lib.HashedFrom != "mapping" {
		t.Errorf("sha256 %s from %q, want the mapped inode's, from mapping", lib.SHA256, lib.HashedFrom)
	}
	if lib.Path != p.nvml+deletedSuffix {
		t.Errorf("path = %q, want the kernel's name with (deleted)", lib.Path)
	}
}

// TestNVMLLibraryWillNotHashTheWrongFile: replaced on disk and no map_files
// means the only file at hand is not the one mapped -- an error, not a digest.
func TestNVMLLibraryWillNotHashTheWrongFile(t *testing.T) {
	p := newFakeProcess(t)
	p.maps = slices.DeleteFunc(p.maps, func(l string) bool { return strings.HasSuffix(l, p.nvml) })
	p.mapFile("r-xp", p.nvml+deletedSuffix)

	lib := p.inspector().inspect()
	if lib.Error == "" || lib.SHA256 != "" {
		t.Fatalf("error %q, sha256 %q: want an error and no digest", lib.Error, lib.SHA256)
	}
}

// TestNVMLLibraryFlagsInterposition is the attack this check exists for: the
// genuine library at its proper path, and a preloaded one overriding its
// functions. The path alone looks fine; the process does not.
func TestNVMLLibraryFlagsInterposition(t *testing.T) {
	p := newFakeProcess(t)
	evil := filepath.Join(p.root, "tmp", "libevil.so")
	p.env["LD_PRELOAD"] = evil
	p.mapFile("r-xp", evil)

	lib := p.inspector().inspect()
	if !hasProblem(lib.Problems, "LD_PRELOAD is set: "+evil) {
		t.Errorf("LD_PRELOAD not flagged: %q", lib.Problems)
	}
	if !hasProblem(lib.Problems, "unexpected library mapped: "+evil) {
		t.Errorf("the preloaded library not flagged: %q", lib.Problems)
	}
	if lib.SHA256 != digest(genuineNVML) {
		t.Errorf("the genuine library is still measured: sha256 %s", lib.SHA256)
	}
}

// TestNVMLLibraryFlagsLoaderLevers covers the other ways in that leave the
// process looking ordinary: a system-wide preload list, LD_AUDIT, a search
// path, a tracer.
func TestNVMLLibraryFlagsLoaderLevers(t *testing.T) {
	p := newFakeProcess(t)
	p.write(filepath.Join(p.root, "etc", "ld.so.preload"), "# keep\n\n/opt/hook.so  # why not\n", 0o644)
	p.env["LD_AUDIT"] = "/opt/audit.so"
	p.env["LD_LIBRARY_PATH"] = "/opt/lib"
	p.write(filepath.Join(p.proc, "status"), "Name:\tx\nTracerPid:\t4321\n", 0o644)

	lib := p.inspector().inspect()
	for _, want := range []string{
		"LD_AUDIT is set: /opt/audit.so",
		"LD_LIBRARY_PATH is set: /opt/lib",
		"/etc/ld.so.preload is not empty: /opt/hook.so",
		"traced by pid 4321",
	} {
		if !hasProblem(lib.Problems, want) {
			t.Errorf("missing %q in %q", want, lib.Problems)
		}
	}

	// A preload file with only comments loads nothing.
	p.write(filepath.Join(p.root, "etc", "ld.so.preload"), "# nothing here\n", 0o644)
	if lib := p.inspector().inspect(); hasProblem(lib.Problems, "/etc/ld.so.preload") {
		t.Errorf("comment-only ld.so.preload flagged: %q", lib.Problems)
	}
}

// TestNVMLLibraryFlagsPlacement covers where the library comes from: outside
// the known directories, or anywhere a non-root user could swap it.
func TestNVMLLibraryFlagsPlacement(t *testing.T) {
	t.Run("outside the library directories", func(t *testing.T) {
		p := newFakeProcess(t)
		elsewhere := filepath.Join(p.root, "opt", "libnvidia-ml.so.1")
		if err := os.MkdirAll(filepath.Dir(elsewhere), 0o755); err != nil {
			t.Fatal(err)
		}
		p.write(elsewhere, genuineNVML, 0o644)
		p.maps = slices.DeleteFunc(p.maps, func(l string) bool { return strings.HasSuffix(l, p.nvml) })
		p.mapFile("r-xp", elsewhere)

		lib := p.inspector().inspect()
		if !hasProblem(lib.Problems, "libnvidia-ml loaded from outside the system library directories: "+elsewhere) {
			t.Errorf("placement not flagged: %q", lib.Problems)
		}
	})
	t.Run("writable by others", func(t *testing.T) {
		p := newFakeProcess(t)
		p.write(p.nvml, genuineNVML, 0o666)
		if err := os.Chmod(p.nvDir, 0o775); err != nil {
			t.Fatal(err)
		}
		lib := p.inspector().inspect()
		for _, want := range []string{
			fmt.Sprintf("writable by group or others: %s (0666)", p.nvml),
			fmt.Sprintf("writable by group or others: %s (0775)", p.nvDir),
		} {
			if !hasProblem(lib.Problems, want) {
				t.Errorf("missing %q in %q", want, lib.Problems)
			}
		}
	})
	t.Run("not owned by root", func(t *testing.T) {
		p := newFakeProcess(t)
		in := p.inspector()
		in.rootUID = uint32(os.Getuid()) + 1
		lib := in.inspect()
		for _, f := range []string{p.nvml, p.nvDir, p.root} {
			if !hasProblem(lib.Problems, "not owned by root: "+f+" ") {
				t.Errorf("%s not flagged: %q", f, lib.Problems)
			}
		}
	})
}

// TestNVMLLibraryQuietOnHealthyVariants pins what must not be flagged, since a
// problem that shows on healthy guests is one nobody reads: older glibc file
// names, NSS modules, the driver libraries beside libnvidia-ml (libcuda among
// them, which NVML loads itself), non-code mappings of any file, and a C
// runtime upgraded under the running process.
func TestNVMLLibraryQuietOnHealthyVariants(t *testing.T) {
	p := newFakeProcess(t)
	p.mapFile("r-xp", filepath.Join(p.sysDir, "libc-2.31.so"))
	p.mapFile("r-xp", filepath.Join(p.sysDir, "ld-2.31.so"))
	p.mapFile("r-xp", filepath.Join(p.sysDir, "libpthread.so.0"))
	p.mapFile("r-xp", filepath.Join(p.sysDir, "libnss_files.so.2"))
	p.mapFile("r-xp", filepath.Join(p.sysDir, "libm.so.6")+deletedSuffix)
	p.mapFile("r-xp", filepath.Join(p.nvDir, "libnvidia-cfg.so.550.54.15"))
	p.mapFile("r-xp", filepath.Join(p.nvDir, "libcuda.so.550.54.15"))
	p.mapFile("r--p", filepath.Join(p.root, "tmp", "some-data-file"))

	if lib := p.inspector().inspect(); len(lib.Problems) != 0 {
		t.Fatalf("healthy variants flagged: %q", lib.Problems)
	}
}

// TestNVMLLibraryFlagsDriverLibrariesElsewhere: libcuda and the libnvidia-*
// family are expected for the directory they share with libnvidia-ml, not for
// their names -- from anywhere else they are flagged like any other library.
func TestNVMLLibraryFlagsDriverLibrariesElsewhere(t *testing.T) {
	p := newFakeProcess(t)
	elsewhere := []string{
		filepath.Join(p.root, "opt", "libcuda.so.550.54.15"),
		filepath.Join(p.root, "opt", "libnvidia-cfg.so.550.54.15"),
	}
	for _, f := range elsewhere {
		p.mapFile("r-xp", f)
	}

	lib := p.inspector().inspect()
	for _, f := range elsewhere {
		if !hasProblem(lib.Problems, "unexpected library mapped: "+f) {
			t.Errorf("%s not flagged: %q", f, lib.Problems)
		}
	}
}

// TestNVMLLibraryNotLoaded: with NVML down there is nothing to measure, but
// the process-wide levers are still worth reporting.
func TestNVMLLibraryNotLoaded(t *testing.T) {
	p := newFakeProcess(t)
	p.maps = slices.DeleteFunc(p.maps, func(l string) bool { return strings.HasSuffix(l, p.nvml) })
	p.env["LD_PRELOAD"] = "/tmp/x.so"

	lib := p.inspector().inspect()
	if lib.Path != "" || lib.SHA256 != "" || lib.Error != "" {
		t.Errorf("no library mapped: path %q sha256 %q error %q", lib.Path, lib.SHA256, lib.Error)
	}
	if !hasProblem(lib.Problems, "LD_PRELOAD is set") {
		t.Errorf("LD_PRELOAD not reported without NVML: %q", lib.Problems)
	}
}

// TestNVMLLibraryMapsUnreadable keeps what was found before the failure.
func TestNVMLLibraryMapsUnreadable(t *testing.T) {
	p := newFakeProcess(t)
	in := p.inspector()
	if err := os.Remove(filepath.Join(p.proc, "maps")); err != nil {
		t.Fatal(err)
	}
	p.env["LD_PRELOAD"] = "/tmp/x.so"

	lib := in.inspect()
	if lib.Error == "" {
		t.Error("no error without /proc/self/maps")
	}
	if !hasProblem(lib.Problems, "LD_PRELOAD is set") {
		t.Errorf("problems found before the failure lost: %q", lib.Problems)
	}
}

// TestNVMLLibrarySeveralCopies flags a second libnvidia-ml in the process.
func TestNVMLLibrarySeveralCopies(t *testing.T) {
	p := newFakeProcess(t)
	other := filepath.Join(p.sysDir, "libnvidia-ml.so.1")
	p.write(other, "another copy", 0o644)
	p.mapFile("r-xp", other)

	lib := p.inspector().inspect()
	if !hasProblem(lib.Problems, "several libnvidia-ml mapped: ") {
		t.Errorf("second copy not flagged: %q", lib.Problems)
	}
}

func TestCapProblems(t *testing.T) {
	var many []string
	for i := 0; i < maxNVMLLibraryProblems+8; i++ {
		many = append(many, fmt.Sprint(i))
	}
	got := capProblems(many)
	if len(got) != maxNVMLLibraryProblems {
		t.Fatalf("len = %d, want %d", len(got), maxNVMLLibraryProblems)
	}
	if want := "and 9 more"; got[len(got)-1] != want {
		t.Errorf("last = %q, want %q", got[len(got)-1], want)
	}
}

// TestQueryMetricsReportsNVMLLibrary pins the wiring: every tick carries the
// measurement -- also when NVML failed to load -- and a finding is logged when
// it appears, not again on every tick after.
func TestQueryMetricsReportsNVMLLibrary(t *testing.T) {
	p := newFakeProcess(t)
	p.env["LD_PRELOAD"] = "/tmp/x.so"
	core, logs := observer.New(zap.InfoLevel)
	e := NewGpuMetricsExporter(GpuMetricsExporterConfig{
		Log:            zap.New(core),
		TickPeriod:     time.Hour,
		InstanceIDPath: filepath.Join(p.root, "instance-id"),
	})
	e.initNVMLFn = func() error { return errors.New("no NVML here") }
	e.nvmlLib = p.inspector()

	for tick := 0; tick < 3; tick++ {
		m, err := e.queryMetrics()
		if err != nil {
			t.Fatalf("queryMetrics: %v", err)
		}
		if !hasProblem(m.NVMLLibrary.Problems, "LD_PRELOAD is set") {
			t.Fatalf("tick %d: nvml_library = %+v", tick, m.NVMLLibrary)
		}
	}
	if n := logs.FilterMessage("nvml library check").Len(); n != 1 {
		t.Errorf("finding logged %d times over 3 ticks, want once", n)
	}
}
