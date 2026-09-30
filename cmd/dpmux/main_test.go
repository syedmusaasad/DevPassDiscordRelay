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

// updateRepo is a temp remote plus a clone registered as HOME's install source.
type updateRepo struct {
	home, seed, checkout string
	latest               string // commit the latest tag initially points at
}

func newUpdateRepo(t *testing.T) *updateRepo {
	t.Helper()
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
	return &updateRepo{home: home, seed: seed, checkout: checkout, latest: latest}
}

func TestDpmuxUpdate_ChecksOutLatestTag(t *testing.T) {
	r := newUpdateRepo(t)
	if err := runUpdate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitAt(t, r.checkout, "rev-parse", "HEAD")); got != r.latest {
		t.Fatalf("HEAD after update = %s, want latest tag commit %s", got, r.latest)
	}
}

// TestDpmuxUpdate_FollowsMovedLatestTag moves the latest tag in the remote after
// the clone already holds the old tag; a plain `git fetch --tags` would keep it.
func TestDpmuxUpdate_FollowsMovedLatestTag(t *testing.T) {
	r := newUpdateRepo(t)
	if err := runUpdate(); err != nil {
		t.Fatal(err)
	}
	writeUpdateFixture(t, r.seed, "moved")
	runGitAt(t, r.seed, "add", ".")
	runGitAt(t, r.seed, "commit", "-m", "moved release")
	moved := strings.TrimSpace(runGitAt(t, r.seed, "rev-parse", "HEAD"))
	if moved == r.latest {
		t.Fatal("test setup: moved commit equals original")
	}
	runGitAt(t, r.seed, "tag", "-f", "latest")
	runGitAt(t, r.seed, "push", "--force", "origin", "HEAD:master", "refs/tags/latest")

	if err := runUpdate(); err != nil {
		t.Fatalf("update after tag moved: %v", err)
	}
	if got := strings.TrimSpace(runGitAt(t, r.checkout, "rev-parse", "HEAD")); got != moved {
		t.Fatalf("HEAD after update = %s, want moved latest tag commit %s", got, moved)
	}
	got, err := os.ReadFile(filepath.Join(r.home, ".local", "bin", "relay"))
	if err != nil || string(got) != "moved" {
		t.Fatalf("installed relay = %q, %v; want rebuilt %q", got, err, "moved")
	}
}

// TestDpmuxUpdate_ReplacesRunningBinary keeps copies of real executables running
// from the install paths while the update runs. Truncating a running executable
// fails with ETXTBSY; temp-file-plus-rename must succeed.
func TestDpmuxUpdate_ReplacesRunningBinary(t *testing.T) {
	// Resolve a real, non-multicall executable before newUpdateRepo narrows PATH.
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("sh not found: %v", err)
	}
	if shPath, err = filepath.EvalSymlinks(shPath); err != nil {
		t.Fatal(err)
	}
	r := newUpdateRepo(t)
	src, err := os.ReadFile(shPath)
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(r.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay", "dpmux"} {
		dest := filepath.Join(binDir, name)
		if err := os.WriteFile(dest, src, 0755); err != nil {
			t.Fatal(err)
		}
		// The copy blocks reading stdin, so it stays running for the whole update.
		cmd := exec.Command(dest, "-c", "read _")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start old %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
		// Precondition: the running binary really cannot be opened for writing.
		if f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, 0); err == nil {
			f.Close()
			t.Fatalf("precondition failed: running %s was writable, test would not prove ETXTBSY safety", name)
		}
	}
	if err := runUpdate(); err != nil {
		t.Fatalf("update with running binaries: %v", err)
	}
	for _, name := range []string{"relay", "dpmux"} {
		got, err := os.ReadFile(filepath.Join(binDir, name))
		if err != nil || string(got) != "latest" {
			t.Fatalf("installed %s = %q, %v; want rebuilt %q", name, got, err, "latest")
		}
		info, _ := os.Stat(filepath.Join(binDir, name))
		if info.Mode().Perm() != 0755 {
			t.Fatalf("%s mode = %v, want 0755", name, info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(binDir)
	if len(entries) != 2 {
		t.Fatalf("leftover temp files in %s: %v", binDir, entries)
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
