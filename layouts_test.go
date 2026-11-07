package main

import (
	"path/filepath"
	"testing"
)

func TestProfileFile(t *testing.T) {
	if p, err := profileFile(""); err != nil || p != layoutPath {
		t.Errorf("empty name: got %q, %v; want layout.json", p, err)
	}
	if p, err := profileFile("Default"); err != nil || p != layoutPath {
		t.Errorf("Default: got %q, %v; want layout.json", p, err)
	}
	if p, err := profileFile("Shooter 2-b_c"); err != nil || p != filepath.Join(profilesDir, "Shooter 2-b_c.json") {
		t.Errorf("valid name: got %q, %v", p, err)
	}
	for _, bad := range []string{"../etc", "a/b", ".hidden", "", "x/", "way-too-long-name-that-exceeds-thirty-two-chars"} {
		if bad == "" {
			continue // empty means Default, covered above
		}
		if _, err := profileFile(bad); err == nil {
			t.Errorf("name %q should be rejected", bad)
		}
	}
}
