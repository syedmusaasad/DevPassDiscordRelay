package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestDpmuxUpdate_ChecksOutLatestTag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bare := filepath.Join(t.TempDir(), "remote.git")
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, "init", "--bare", bare)
	runGit(t, "init", seed)
	runGitAt(t, seed, "config", "user.email", "test@example.invalid")
	runGitAt(t, seed, "config", "user.name", "Test")
	writeUpdateFixture(t, seed, "latest")
	runGitAt(t, seed, "add", ".")
	runGitAt(t, seed, "commit", "-m", "release")
	latest := strings.TrimSpace(runGitAt(t, seed, "rev-parse", "HEAD"))
	runGitAt(t, seed, "tag", "latest")
	writeUpdateFixture(t, seed, "branch-tip")
	runGitAt(t, seed, "add", ".")
	runGitAt(t, seed, "commit", "-m", "newer branch tip")
	runGitAt(t, seed, "remote", "add", "origin", bare)
	runGitAt(t, seed, "push", "origin", "HEAD:master", "--tags")

	checkout := filepath.Join(t.TempDir(), "checkout")
	runGit(t, "clone", bare, checkout)
	marker := filepath.Join(home, ".config", "devpass-discord-relay", "install-source")
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(checkout+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", updateTestPath(t))

	if err := runUpdate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitAt(t, checkout, "rev-parse", "HEAD")); got != latest {
		t.Fatalf("HEAD after update = %s, want latest tag commit %s", got, latest)
	}
}

func updateTestPath(t *testing.T) string {
	t.Helper()
	tools := t.TempDir()
	for _, name := range []string{"git", "make", "mkdir"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("locate %s: %v", name, err)
		}
		if err := os.Symlink(bin, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	return tools
}

func writeUpdateFixture(t *testing.T, root, version string) {
	t.Helper()
	makefile := "build:\n\tmkdir -p bin\n\tprintf '" + version + "' > bin/relay\n\tprintf '" + version + "' > bin/dpmux\n"
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(makefile), 0644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	return runGitAt(t, "", args...)
}

func runGitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
