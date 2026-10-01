package main

import (
	"testing"
)

func TestGetEnv(t *testing.T) {
	primary := "GPU_METRICS_EXPORTER_TEST_STR"
	fallback := "TEST_STR_FALLBACK"

	// Neither set -> returns default
	if got := getEnv(primary, fallback, "default"); got != "default" {
		t.Fatalf("expected default, got %s", got)
	}

	// Fallback set -> returns fallback
	t.Setenv(fallback, "fallback_val")
	if got := getEnv(primary, fallback, "default"); got != "fallback_val" {
		t.Fatalf("expected fallback_val, got %s", got)
	}

	// Primary set -> returns primary
	t.Setenv(primary, "primary_val")
	if got := getEnv(primary, fallback, "default"); got != "primary_val" {
		t.Fatalf("expected primary_val, got %s", got)
	}
}

func TestGetEnvInt(t *testing.T) {
	primary := "GPU_METRICS_EXPORTER_TEST_INT"
	fallback := "TEST_INT_FALLBACK"

	// Neither set -> returns default
	got, err := getEnvInt(primary, fallback, 1234)
	if err != nil || got != 1234 {
		t.Fatalf("expected 1234, nil, got %d, %v", got, err)
	}

	// Fallback set -> returns fallback int
	t.Setenv(fallback, "5678")
	got, err = getEnvInt(primary, fallback, 1234)
	if err != nil || got != 5678 {
		t.Fatalf("expected 5678, nil, got %d, %v", got, err)
	}

	// Primary set -> takes precedence over fallback
	t.Setenv(primary, "9999")
	got, err = getEnvInt(primary, fallback, 1234)
	if err != nil || got != 9999 {
		t.Fatalf("expected 9999, nil, got %d, %v", got, err)
	}

	// Invalid integer string returns error
	t.Setenv(primary, "invalid_num")
	if _, err := getEnvInt(primary, fallback, 1234); err == nil {
		t.Fatal("expected error for invalid number, got nil")
	}
}
