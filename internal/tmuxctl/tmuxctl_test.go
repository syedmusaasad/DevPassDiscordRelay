package tmuxctl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveTmuxBin(t *testing.T) {
	dir := t.TempDir()
	override := writeTmuxStub(t, filepath.Join(dir, "override-tmux"))
	local := writeTmuxStub(t, filepath.Join(dir, "local-tmux"))
	pathDir := t.TempDir()
	pathBin := writeTmuxStub(t, filepath.Join(pathDir, "tmux"))

	originalLocal := localTmuxBin
	localTmuxBin = local
	t.Cleanup(func() { localTmuxBin = originalLocal })
	t.Setenv("PATH", pathDir)
	t.Setenv("RELAY_TMUX_BIN", override)
	if got, err := ResolveTmuxBin(); err != nil || got != override {
		t.Fatalf("override resolution = %q, %v; want %q", got, err, override)
	}

	t.Setenv("RELAY_TMUX_BIN", "")
	if got, err := ResolveTmuxBin(); err != nil || got != local {
		t.Fatalf("managed fallback = %q, %v; want %q", got, err, local)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveTmuxBin(); err != nil || got != pathBin {
		t.Fatalf("PATH fallback = %q, %v; want %q", got, err, pathBin)
	}
}

func writeTmuxStub(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = \"-V\" ]\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
