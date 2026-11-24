//go:build windows

package gitvcs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This fixture is meant to be OBSERVED by the Windows CI job. It states no
// result: until windows-latest runs it, native Windows behavior of volume
// aliases is unverified (the cross-build proves compilation only).
//
// The owner repository (the seed source) physically lies inside the pool
// directory, but is reached through a SUBST drive letter, so its path carries a
// different volume name than the pool's. A source physically inside the pool
// must be refused whatever spelling reaches it.
func TestRequireCleanRemovalRefusesSeedSourceReachedThroughSubstVolume(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := filepath.Join(base, "pool")
	repo := filepath.Join(pool, "repo")
	slot := filepath.Join(pool, "1", "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatal(err)
	}
	run(base, "init", "--initial-branch=main", repo)
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	for name, content := range map[string]string{".gitignore": "*.ignored\n", "seed.ignored": "original\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(repo, "add", ".gitignore")
	run(repo, "commit", "-m", "init")
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		t.Fatal(err)
	}
	run(repo, "worktree", "add", "--detach", slot)
	if err := os.WriteFile(filepath.Join(slot, "seed.ignored"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Map the POOL directory to a free drive letter; the repository is then
	// reachable as <letter>:\repo, another volume spelling of the same files.
	var letter string
	for c := 'Z'; c >= 'G'; c-- {
		candidate := string(c) + ":"
		if _, err := os.Stat(candidate + `\`); os.IsNotExist(err) {
			if out, err := exec.Command("subst", candidate, pool).CombinedOutput(); err != nil {
				t.Fatalf("subst %s %s: %v\n%s", candidate, pool, err, out)
			}
			letter = candidate
			break
		}
	}
	if letter == "" {
		t.Fatal("no free drive letter for the subst alias")
	}
	t.Cleanup(func() { exec.Command("subst", letter, "/D").Run() })
	aliasRepo := letter + `\repo`
	if got, err := os.ReadFile(filepath.Join(aliasRepo, "seed.ignored")); err != nil || strings.TrimSpace(string(got)) != "original" {
		t.Fatalf("alias does not reach the seed source: %q (%v)", got, err)
	}
	if err := RequireCleanRemovalAllowed(aliasRepo, pool, slot, []string{"seed.ignored"}); err == nil {
		t.Errorf("a seed source physically inside the pool was accepted through a different volume spelling")
	}
}
