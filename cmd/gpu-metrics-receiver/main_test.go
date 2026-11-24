package main

import (
	"testing"
)

func TestExporterUpdateConfig(t *testing.T) {
	for _, tc := range []struct {
		name      string
		path      string
		version   string
		wantError bool
	}{
		{name: "disabled"},
		{name: "configured", path: "/packages/exporter.deb", version: "2026.11.24-1"},
		{name: "path only", path: "/packages/exporter.deb", wantError: true},
		{name: "version only", version: "2026.11.24-1", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key == exporterDebPathEnv {
					return tc.path
				}
				if key == exporterVersionEnv {
					return tc.version
				}
				return ""
			}
			path, version, err := exporterUpdateConfig(getenv)
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v, wantError %v", err, tc.wantError)
			}
			if !tc.wantError && (path != tc.path || version != tc.version) {
				t.Fatalf("got %q/%q, want %q/%q", path, version, tc.path, tc.version)
			}
		})
	}
}
