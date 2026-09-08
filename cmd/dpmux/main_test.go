package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateSourceUsesInstallMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "checkout")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, ".config", "devpass-discord-relay", "install-source")
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(root+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := updateSource()
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("source = %q, want %q", got, root)
	}
}
