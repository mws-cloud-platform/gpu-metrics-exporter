package gpumetricsexporter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

const (
	// maxNVMLLibraryProblems bounds the problem list on the wire: a process
	// that trips more than this is past needing every line of it.
	maxNVMLLibraryProblems = 32
	// maxProblemValueLen keeps an injected LD_* value or preload list from
	// bloating the payload.
	maxProblemValueLen = 256
	deletedSuffix      = " (deleted)"
)

// systemLibDirs is where the C runtime the exporter links against lives, and
// the first place libnvidia-ml is looked for.
var systemLibDirs = []string{
	"/usr/lib/x86_64-linux-gnu", "/lib/x86_64-linux-gnu",
	"/usr/lib64", "/lib64", "/usr/lib", "/lib",
}

// systemLibPatterns are the libraries a healthy exporter maps besides itself
// and libnvidia-ml: the dynamic loader and glibc's pieces under both their
// soname and the libc-2.31.so-style file name that older glibc resolves it
// to, libgcc_s, and the NSS modules a lookup may pull in.
var systemLibPatterns = []string{
	"ld-linux-x86-64.so.2", "ld-*.so",
	"libc.so.6", "libc-*.so",
	"libm.so.6", "libm-*.so",
	"libpthread.so.0", "libpthread-*.so",
	"libdl.so.2", "libdl-*.so",
	"librt.so.1", "librt-*.so",
	"libresolv.so.2", "libresolv-*.so",
	"libgcc_s.so.1",
	"libnss_*",
}

// nvmlLibraryInspector measures the libnvidia-ml mapped into this process and
// whatever could be intercepting it, for gpumetrics.NVMLLibrary. It judges
// nothing: what it finds goes on the wire for the host to weigh. Every check
// must stay quiet on a healthy guest -- a field that is noisy everywhere is
// one nobody reads when it matters -- which is why it looks only at
// executable mappings, knows glibc's older file names, and does not flag a
// library replaced on disk since it was loaded (a driver upgrade does that).
type nvmlLibraryInspector struct {
	procSelf    string // /proc/self
	preloadFile string // /etc/ld.so.preload
	getenv      func(string) string
	exe         string          // this executable, as the kernel names it
	systemDirs  map[string]bool // where the C runtime may come from
	nvmlDirs    map[string]bool // where libnvidia-ml may come from
	rootUID     uint32          // owner the library and its directories must have
	chainTop    string          // the ownership walk up from the library stops here
}

// newNVMLLibraryInspector inspects this process. libnvidia-ml may come from the
// system library directories or from the places initNVML itself looks: the
// container and host-root candidates, and a configured NvmlLibPath.
func newNVMLLibraryInspector(cfg GpuMetricsExporterConfig) *nvmlLibraryInspector {
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	nvmlDirs := slices.Clone(systemLibDirs)
	for _, p := range ContainerCandidatePaths {
		nvmlDirs = append(nvmlDirs, filepath.Dir(p))
	}
	if cfg.HostRoot != "" && cfg.HostRoot != "/" {
		for _, p := range HostCandidatePaths {
			nvmlDirs = append(nvmlDirs, filepath.Dir(filepath.Join(cfg.HostRoot, p)))
		}
	}
	if cfg.NvmlLibPath != "" {
		nvmlDirs = append(nvmlDirs, filepath.Dir(FindNVMLLibraryPath(cfg.NvmlLibPath, cfg.HostRoot)))
	}
	return &nvmlLibraryInspector{
		procSelf:    "/proc/self",
		preloadFile: "/etc/ld.so.preload",
		getenv:      os.Getenv,
		exe:         exe,
		systemDirs:  dirSet(systemLibDirs),
		nvmlDirs:    dirSet(nvmlDirs),
		rootUID:     0,
		chainTop:    "/",
	}
}

// dirSet holds each directory both as given and with symlinks resolved: the
// kernel names a mapping by its resolved path, and /lib/x86_64-linux-gnu is a
// link to /usr/lib/x86_64-linux-gnu on a merged-/usr system.
func dirSet(dirs []string) map[string]bool {
	set := make(map[string]bool, 2*len(dirs))
	for _, d := range dirs {
		d = filepath.Clean(d)
		set[d] = true
		if resolved, err := filepath.EvalSymlinks(d); err == nil {
			set[resolved] = true
		}
	}
	return set
}

// mapping is one executable, file-backed line of /proc/self/maps.
type mapping struct {
	addr string // the address range, as /proc/self/map_files names it
	path string // as the kernel prints it, " (deleted)" included
	name string // path without " (deleted)"
}

func (m mapping) deleted() bool { return m.name != m.path }

// executableMappings parses /proc/self/maps, keeping the file-backed mappings
// that can run code: a library that maps no code cannot override a function.
func executableMappings(maps []byte) []mapping {
	var out []mapping
	sc := bufio.NewScanner(bytes.NewReader(maps))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// address           perms offset  dev   inode  pathname
		// 7f2c4e600000-7f2c4e628000 r-xp 00028000 fe:01 1234 /usr/lib/.../libc.so.6
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || len(fields[1]) < 3 || fields[1][2] != 'x' ||
			!strings.HasPrefix(fields[5], "/") {
			continue // not executable, or anonymous, [vdso] and the like
		}
		p := strings.Join(fields[5:], " ")
		out = append(out, mapping{addr: fields[0], path: p, name: strings.TrimSuffix(p, deletedSuffix)})
	}
	return out
}

func isNVMLLib(base string) bool { return strings.HasPrefix(base, "libnvidia-ml.so") }

func matchesAny(base string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

// inspectNVMLLibrary measures the NVML library for this tick's payload, and
// logs what it found when that changes: once at Warn for a problem, not on
// every tick, and once at Info for a clean measurement so the digest of the
// library in use is on record.
func (e *GpuMetricsExporter) inspectNVMLLibrary() gpumetrics.NVMLLibrary {
	if e.nvmlLib == nil {
		return gpumetrics.NVMLLibrary{}
	}
	lib := e.nvmlLib.inspect()
	report := strings.Join(append([]string{lib.Error, lib.Path, lib.SHA256}, lib.Problems...), "\x00")
	if report != e.lastNVMLLibraryReport {
		e.lastNVMLLibraryReport = report
		fields := []zap.Field{
			zap.String("path", lib.Path),
			zap.String("sha256", lib.SHA256),
			zap.String("hashed_from", lib.HashedFrom),
		}
		if lib.Error != "" || len(lib.Problems) > 0 {
			e.log.Warn("nvml library check", append(fields,
				zap.String("error", lib.Error), zap.Strings("problems", lib.Problems))...)
		} else {
			e.log.Info("nvml library check", fields...)
		}
	}
	return lib
}

// inspect measures the process as it stands. It runs every tick, whether or
// not NVML loaded: a preload or a tracer matters either way. The result is
// named so that the deferred cap lands in what is returned.
func (in *nvmlLibraryInspector) inspect() (lib gpumetrics.NVMLLibrary) {
	var problems []string
	defer func() { lib.Problems = capProblems(problems) }()

	for _, v := range []string{"LD_PRELOAD", "LD_AUDIT", "LD_LIBRARY_PATH"} {
		if val := in.getenv(v); val != "" {
			problems = append(problems, fmt.Sprintf("%s is set: %s", v, truncate(val)))
		}
	}
	if entries := preloadEntries(in.preloadFile); len(entries) > 0 {
		problems = append(problems, "/etc/ld.so.preload is not empty: "+truncate(strings.Join(entries, " ")))
	}
	if pid := tracerPid(filepath.Join(in.procSelf, "status")); pid != 0 {
		problems = append(problems, fmt.Sprintf("traced by pid %d", pid))
	}

	maps, err := os.ReadFile(filepath.Join(in.procSelf, "maps"))
	if err != nil {
		lib.Error = fmt.Sprintf("reading the process mappings: %v", err)
		return lib
	}
	mappings := executableMappings(maps)

	// libnvidia-ml first: its directory vouches for its libnvidia-* siblings.
	var nvml []mapping
	nvmlLibDirs := map[string]bool{}
	for _, m := range mappings {
		if isNVMLLib(path.Base(m.name)) {
			nvml = append(nvml, m)
			nvmlLibDirs[path.Dir(m.name)] = true
		}
	}
	var unexpected []string
	for _, m := range mappings {
		dir, base := path.Dir(m.name), path.Base(m.name)
		switch {
		case m.name == in.exe, isNVMLLib(base):
		case in.systemDirs[dir] && matchesAny(base, systemLibPatterns):
		case nvmlLibDirs[dir] && strings.HasPrefix(base, "libnvidia-"):
		default:
			if !slices.Contains(unexpected, m.path) {
				unexpected = append(unexpected, m.path)
			}
		}
	}
	slices.Sort(unexpected)
	for _, p := range unexpected {
		problems = append(problems, "unexpected library mapped: "+p)
	}

	if len(nvml) == 0 {
		return lib // NVML did not load this tick; init_nvml_error says why
	}
	var names []string
	for _, m := range nvml {
		if !slices.Contains(names, m.name) {
			names = append(names, m.name)
		}
	}
	if len(names) > 1 {
		slices.Sort(names)
		problems = append(problems, "several libnvidia-ml mapped: "+strings.Join(names, ", "))
	}
	m := nvml[0]
	lib.Path = m.path
	if !in.nvmlDirs[path.Dir(m.name)] {
		problems = append(problems, "libnvidia-ml loaded from outside the system library directories: "+m.name)
	}
	problems = append(problems, in.ownershipProblems(m.name)...)

	sum, size, from, err := in.hashMapping(m)
	if err != nil {
		lib.Error = err.Error()
		return lib
	}
	lib.SHA256, lib.Size, lib.HashedFrom = sum, size, from
	return lib
}

// ownershipProblems walks from the library up to chainTop and reports every
// step that someone other than root owns or may write to: whoever can write
// any of them can put another library in its place.
func (in *nvmlLibraryInspector) ownershipProblems(p string) []string {
	var problems []string
	for {
		if fi, err := os.Lstat(p); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != in.rootUID {
				problems = append(problems, fmt.Sprintf("not owned by root: %s (uid %d)", p, st.Uid))
			}
			if perm := fi.Mode().Perm(); perm&0o022 != 0 {
				problems = append(problems, fmt.Sprintf("writable by group or others: %s (%#o)", p, perm))
			}
		}
		parent := filepath.Dir(p)
		if p == in.chainTop || parent == p {
			return problems
		}
		p = parent
	}
}

// hashMapping digests the library's file. Through /proc/self/map_files it is
// the very inode mapped, whatever the path holds now; that needs
// CAP_SYS_ADMIN (or CAP_CHECKPOINT_RESTORE), which the exporter normally has
// as root or as a privileged pod. Without it the file at the path is the
// next best thing -- unless that file has been replaced since it was mapped,
// when it would be a measurement of the wrong file.
func (in *nvmlLibraryInspector) hashMapping(m mapping) (string, int64, string, error) {
	from := "mapping"
	f, err := os.Open(filepath.Join(in.procSelf, "map_files", m.addr))
	if err != nil {
		if m.deleted() {
			return "", 0, "", errors.New("libnvidia-ml was replaced on disk since it was " +
				"loaded, and /proc/self/map_files is not readable to hash the mapped one")
		}
		from = "path"
		if f, err = os.Open(m.name); err != nil {
			return "", 0, "", fmt.Errorf("reading the mapped libnvidia-ml: %w", err)
		}
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, "", fmt.Errorf("reading the mapped libnvidia-ml: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, from, nil
}

// preloadEntries is the libraries /etc/ld.so.preload loads into every
// process, comments and blank lines aside. A missing file is the normal case.
func preloadEntries(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var entries []string
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		entries = append(entries, strings.Fields(line)...)
	}
	return entries
}

// tracerPid is the pid ptrace-attached to this process, 0 when none.
func tracerPid(statusFile string) int {
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "TracerPid:"); ok {
			pid, _ := strconv.Atoi(strings.TrimSpace(v))
			return pid
		}
	}
	return 0
}

func truncate(s string) string {
	if len(s) <= maxProblemValueLen {
		return s
	}
	return s[:maxProblemValueLen] + "..."
}

func capProblems(problems []string) []string {
	if len(problems) <= maxNVMLLibraryProblems {
		return problems
	}
	more := len(problems) - (maxNVMLLibraryProblems - 1)
	return append(problems[:maxNVMLLibraryProblems-1], fmt.Sprintf("and %d more", more))
}
