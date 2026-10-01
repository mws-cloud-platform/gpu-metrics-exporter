package gpumetricsexporter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindNVMLLibraryPath(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("returns empty when no candidate exists", func(t *testing.T) {
		got := FindNVMLLibraryPath("", tempDir)
		if got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})

	t.Run("returns custom path when it exists", func(t *testing.T) {
		customFile := filepath.Join(tempDir, "libnvidia-ml.so.1")
		if err := os.WriteFile(customFile, []byte("fake"), 0644); err != nil {
			t.Fatal(err)
		}

		got := FindNVMLLibraryPath(customFile, "")
		if got != customFile {
			t.Fatalf("expected %q, got %q", customFile, got)
		}
	})

	t.Run("resolves custom path relative to hostRoot", func(t *testing.T) {
		hostRoot := filepath.Join(tempDir, "host")
		targetDir := filepath.Join(hostRoot, "opt", "nvidia")
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			t.Fatal(err)
		}
		targetFile := filepath.Join(targetDir, "libnvidia-ml.so.1")
		if err := os.WriteFile(targetFile, []byte("fake"), 0644); err != nil {
			t.Fatal(err)
		}

		got := FindNVMLLibraryPath("/opt/nvidia/libnvidia-ml.so.1", hostRoot)
		if got != targetFile {
			t.Fatalf("expected %q, got %q", targetFile, got)
		}
	})

	t.Run("discovers GPU Operator driver container path under hostRoot", func(t *testing.T) {
		hostRoot := filepath.Join(tempDir, "k8s-node-host")
		driverDir := filepath.Join(hostRoot, "run", "nvidia", "driver", "usr", "lib", "x86_64-linux-gnu")
		if err := os.MkdirAll(driverDir, 0755); err != nil {
			t.Fatal(err)
		}
		targetFile := filepath.Join(driverDir, "libnvidia-ml.so.1")
		if err := os.WriteFile(targetFile, []byte("fake"), 0644); err != nil {
			t.Fatal(err)
		}

		got := FindNVMLLibraryPath("", hostRoot)
		if got != targetFile {
			t.Fatalf("expected %q, got %q", targetFile, got)
		}
	})

	t.Run("returns user specified path even if not on disk yet", func(t *testing.T) {
		nonExistent := "/non/existent/path/libnvidia-ml.so"
		got := FindNVMLLibraryPath(nonExistent, "")
		if got != nonExistent {
			t.Fatalf("expected %q, got %q", nonExistent, got)
		}
	})

	t.Run("finds path using mocked candidates and file existence without touching host", func(t *testing.T) {
		containerCandidates := []string{"/mock/container/libnvidia-ml.so.1"}
		hostCandidates := []string{"/mock/host/libnvidia-ml.so.1"}

		existing := map[string]bool{
			"/custom/host/mock/host/libnvidia-ml.so.1": true,
		}
		mockExists := func(path string) bool {
			return existing[path]
		}

		got := FindNVMLLibraryPathWithFS("", "/custom/host", containerCandidates, hostCandidates, mockExists)
		if want := "/custom/host/mock/host/libnvidia-ml.so.1"; got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	})
}
