package exporterupdate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenPackageValidation(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.deb")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(dir, "large.deb")
	if err := os.WriteFile(large, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, MaxPackageSize+1); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "valid.deb")
	if err := os.WriteFile(valid, []byte("deb"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		version string
	}{
		{"no path", "", "2026.11.24-1"},
		{"no version", valid, ""},
		{"long version", valid, strings.Repeat("v", MaxVersionSize+1)},
		{"directory", dir, "2026.11.24-1"},
		{"empty", empty, "2026.11.24-1"},
		{"too large", large, "2026.11.24-1"},
		{"missing", filepath.Join(dir, "missing"), "2026.11.24-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := OpenPackage(tc.path, tc.version); err == nil {
				t.Fatal("OpenPackage accepted invalid input")
			}
		})
	}
}

func TestOpenPackageKeepsSnapshotAcrossPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exporter.deb")
	if err := os.WriteFile(path, []byte("old package"), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := OpenPackage(path, "2026.11.24-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pkg.Close() }()
	replacement := filepath.Join(dir, "replacement.deb")
	if err := os.WriteFile(replacement, []byte("new package"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, pkg.Metadata().Size)
	if _, err := pkg.file.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("old package")) {
		t.Fatalf("open snapshot changed to %q", got)
	}
}

func TestClientReportsUnavailableHost(t *testing.T) {
	c := testClient(t, &Server{})
	_, err := c.GetMetadata(t.Context())
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Status != StatusUnavailable {
		t.Fatalf("GetMetadata error %v, want unavailable RemoteError", err)
	}
}
