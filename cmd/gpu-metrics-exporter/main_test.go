package main

import (
	"os"
	"testing"
)

func TestGetEnv(t *testing.T) {
	key := "TEST_GPU_METRICS_STR"
	if err := os.Setenv(key, "custom_val"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(key)

	if got := getEnv(key, "default"); got != "custom_val" {
		t.Fatalf("expected custom_val, got %s", got)
	}

	if got := getEnv("NON_EXISTENT_VAR", "default"); got != "default" {
		t.Fatalf("expected default, got %s", got)
	}
}

func TestGetEnvInt(t *testing.T) {
	key := "TEST_GPU_METRICS_INT"
	if err := os.Setenv(key, "1234"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(key)

	if got := getEnvInt(key, 9999); got != 1234 {
		t.Fatalf("expected 1234, got %d", got)
	}

	// Invalid int string returns default
	if err := os.Setenv(key, "invalid_num"); err != nil {
		t.Fatal(err)
	}
	if got := getEnvInt(key, 9999); got != 9999 {
		t.Fatalf("expected 9999, got %d", got)
	}

	if got := getEnvInt("NON_EXISTENT_INT", 9999); got != 9999 {
		t.Fatalf("expected 9999, got %d", got)
	}
}
