package gpumetricsexporter

import (
	"os"
	"path/filepath"
	"strings"
)

// KnownNVMLLibraryPaths lists well-known candidate paths where libnvidia-ml.so
// can be located in NVIDIA GPU Operator driver containers, NVIDIA Container
// Toolkit mounts, and standard Linux distributions.
var KnownNVMLLibraryPaths = []string{
	// NVIDIA GPU Operator driver container (Debian/Ubuntu)
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libnvidia-ml.so",
	// NVIDIA GPU Operator driver container (RHEL/CentOS/Rocky)
	"/run/nvidia/driver/usr/lib64/libnvidia-ml.so.1",
	"/run/nvidia/driver/usr/lib64/libnvidia-ml.so",
	// NVIDIA Container Toolkit / CUDA driver mounts
	"/usr/local/nvidia/lib64/libnvidia-ml.so.1",
	"/usr/local/nvidia/lib64/libnvidia-ml.so",
	"/usr/local/nvidia/lib/libnvidia-ml.so.1",
	"/usr/local/nvidia/lib/libnvidia-ml.so",
	// Standard host paths
	"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so",
	"/usr/lib64/libnvidia-ml.so.1",
	"/usr/lib64/libnvidia-ml.so",
}

// FindNVMLLibraryPath resolves the NVML shared library path.
//
//  1. If customPath is provided, it checks customPath and hostRoot+customPath.
//     If either exists, it returns it; otherwise it returns customPath directly.
//  2. Probes KnownNVMLLibraryPaths directly in the current container filesystem.
//  3. If hostRoot is configured (e.g. "/host"), probes KnownNVMLLibraryPaths
//     prefixed with hostRoot.
//  4. Returns empty string "" if none are found on disk, which indicates
//     that default dynamic linker search (standard dlopen) should be used.
func FindNVMLLibraryPath(customPath string, hostRoot string) string {
	if strings.TrimSpace(customPath) != "" {
		trimmed := strings.TrimSpace(customPath)
		if fileExists(trimmed) {
			return trimmed
		}
		if hostRoot != "" && hostRoot != "/" {
			prefixed := filepath.Join(hostRoot, trimmed)
			if fileExists(prefixed) {
				return prefixed
			}
		}
		// Return customPath directly if specified by the user
		return trimmed
	}

	// 1. Check known container paths
	for _, p := range KnownNVMLLibraryPaths {
		if fileExists(p) {
			return p
		}
	}

	// 2. Check host-prefixed paths if hostRoot is specified
	if hostRoot != "" && hostRoot != "/" {
		for _, p := range KnownNVMLLibraryPaths {
			prefixed := filepath.Join(hostRoot, p)
			if fileExists(prefixed) {
				return prefixed
			}
		}
	}

	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
