package gpumetricsexporter

import (
	"os"
	"path/filepath"
	"strings"
)

// ContainerCandidatePaths lists well-known candidate paths where libnvidia-ml.so
// can be located inside a container (e.g. NVIDIA GPU Operator driver containers
// or NVIDIA Container Toolkit mounts).
var ContainerCandidatePaths = []string{
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
}

// HostCandidatePaths lists well-known candidate paths relative to hostRoot
// where libnvidia-ml.so can be located when running with host root filesystem mounted.
var HostCandidatePaths = []string{
	// NVIDIA GPU Operator driver container under hostRoot
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libnvidia-ml.so",
	"/run/nvidia/driver/usr/lib64/libnvidia-ml.so.1",
	"/run/nvidia/driver/usr/lib64/libnvidia-ml.so",
	// Standard host distribution libraries under hostRoot
	"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
	"/usr/lib/x86_64-linux-gnu/libnvidia-ml.so",
	"/usr/lib64/libnvidia-ml.so.1",
	"/usr/lib64/libnvidia-ml.so",
	// NVIDIA driver mounts under hostRoot
	"/usr/local/nvidia/lib64/libnvidia-ml.so.1",
	"/usr/local/nvidia/lib64/libnvidia-ml.so",
	"/usr/local/nvidia/lib/libnvidia-ml.so.1",
	"/usr/local/nvidia/lib/libnvidia-ml.so",
}

// KnownNVMLLibraryPaths contains all candidate paths for backward compatibility.
var KnownNVMLLibraryPaths = append(append([]string{}, ContainerCandidatePaths...), HostCandidatePaths...)

// FindNVMLLibraryPath resolves the NVML shared library path.
//
//  1. If customPath is provided, it checks customPath and hostRoot+customPath.
//     If either exists, it returns it; otherwise it returns customPath directly.
//  2. Probes ContainerCandidatePaths directly in the current container filesystem.
//  3. If hostRoot is configured (e.g. "/host"), probes HostCandidatePaths
//     prefixed with hostRoot.
//  4. Returns empty string "" if none are found on disk, indicating that default
//     dynamic linker search (standard dlopen) should be used.
func FindNVMLLibraryPath(customPath string, hostRoot string) string {
	return FindNVMLLibraryPathWithFS(customPath, hostRoot, ContainerCandidatePaths, HostCandidatePaths, fileExists)
}

// FindNVMLLibraryPathWithFS resolves candidate paths using custom candidate lists and existence check.
func FindNVMLLibraryPathWithFS(customPath string, hostRoot string, containerPaths, hostPaths []string, exists func(string) bool) string {
	if strings.TrimSpace(customPath) != "" {
		trimmed := strings.TrimSpace(customPath)
		if exists(trimmed) {
			return trimmed
		}
		if hostRoot != "" && hostRoot != "/" {
			prefixed := filepath.Join(hostRoot, trimmed)
			if exists(prefixed) {
				return prefixed
			}
		}
		// Return customPath directly if specified by the user
		return trimmed
	}

	// 1. Check known container paths
	for _, p := range containerPaths {
		if exists(p) {
			return p
		}
	}

	// 2. Check host-prefixed paths if hostRoot is specified
	if hostRoot != "" && hostRoot != "/" {
		for _, p := range hostPaths {
			prefixed := filepath.Join(hostRoot, p)
			if exists(prefixed) {
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
