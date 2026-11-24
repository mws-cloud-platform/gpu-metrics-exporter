package exporterupdate

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// Package is an open, immutable-by-path snapshot of the exporter .deb served
// to guests. ReadAt does not change shared file offset and is safe concurrently.
type Package struct {
	file     *os.File
	metadata Metadata
}

func OpenPackage(path, version string) (*Package, error) {
	if path == "" || version == "" {
		return nil, fmt.Errorf("both exporter package path and version are required")
	}
	if len(version) > MaxVersionSize {
		return nil, fmt.Errorf("exporter package version is %d bytes, maximum %d", len(version), MaxVersionSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open exporter package: %w", err)
	}
	closeOnError := func(err error) (*Package, error) {
		_ = f.Close()
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("stat exporter package: %w", err))
	}
	if !fi.Mode().IsRegular() {
		return closeOnError(fmt.Errorf("exporter package %s is not a regular file", path))
	}
	meta := Metadata{Version: version, Size: fi.Size()}
	if err := validateMetadata(meta); err != nil {
		return closeOnError(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return closeOnError(fmt.Errorf("hash exporter package: %w", err))
	}
	if n != meta.Size {
		return closeOnError(fmt.Errorf("exporter package changed while hashing: read %d bytes, initially %d", n, meta.Size))
	}
	copy(meta.SHA256[:], h.Sum(nil))
	return &Package{file: f, metadata: meta}, nil
}

func (p *Package) Metadata() Metadata {
	return p.metadata
}

func (p *Package) Close() error {
	return p.file.Close()
}
