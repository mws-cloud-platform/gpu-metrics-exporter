package main

import (
	"testing"
)

func TestGetEnv(t *testing.T) {
	key := "TEST_GPU_METRICS_STR"
	t.Setenv(key, "custom_val")

	if got := getEnv(key, "default"); got != "custom_val" {
		t.Fatalf("expected custom_val, got %s", got)
	}

	if got := getEnv("NON_EXISTENT_VAR", "default"); got != "default" {
		t.Fatalf("expected default, got %s", got)
	}
}

func TestGetEnvInt(t *testing.T) {
	key := "TEST_GPU_METRICS_INT"
	t.Setenv(key, "1234")

	if got := getEnvInt(key, 9999); got != 1234 {
		t.Fatalf("expected 1234, got %d", got)
	}

	// Invalid int string returns default
	t.Setenv(key, "invalid_num")
	if got := getEnvInt(key, 9999); got != 9999 {
		t.Fatalf("expected 9999, got %d", got)
	}

	if got := getEnvInt("NON_EXISTENT_INT", 9999); got != 9999 {
		t.Fatalf("expected 9999, got %d", got)
	}
}
