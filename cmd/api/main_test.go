package main

import (
	"testing"
	"time"
)

func TestResolvePort(t *testing.T) {
	if got, err := resolvePort(""); err != nil || got != "8080" {
		t.Fatalf("empty: got (%q, %v), want (8080, nil)", got, err)
	}

	if got, err := resolvePort("8081"); err != nil || got != "8081" {
		t.Fatalf("8081: got (%q, %v)", got, err)
	}

	for _, bad := range []string{"0", "65536", "-1", "abc", "80x", "8O8O"} {
		if _, err := resolvePort(bad); err == nil {
			t.Fatalf("%q: accepted, want rejection", bad)
		}
	}
}

func TestResolveDuration(t *testing.T) {
	if got, err := resolveDuration("", "30m"); err != nil || got != 30*time.Minute {
		t.Fatalf("empty: got (%v, %v), want (30m, nil)", got, err)
	}

	if got, err := resolveDuration("90s", "30m"); err != nil || got != 90*time.Second {
		t.Fatalf("90s: got (%v, %v)", got, err)
	}

	for _, bad := range []string{"0s", "-1m", "soon", "10"} {
		if _, err := resolveDuration(bad, "30m"); err == nil {
			t.Fatalf("%q: accepted, want rejection", bad)
		}
	}
}
