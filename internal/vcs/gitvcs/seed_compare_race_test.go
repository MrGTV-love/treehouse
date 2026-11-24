package gitvcs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// F6: an edit made while the judgment is comparing seed bytes must fail closed.
// The seeded file is larger than one 32 KiB chunk and the seam fires after the
// first chunks were read, so the edit lands in an already-read region. The
// unchanged control drains (nil error).
func TestSeedCompareFailsClosedWhenEitherFileChangesWhileCompared(t *testing.T) {
	seed := bytes.Repeat([]byte("A"), 65537)
	for _, tc := range []struct {
		name       string
		edit       func(t *testing.T, source, slotFile string)
		wantRefuse bool
	}{
		{name: "unchanged-control-drains"},
		{name: "destination-edit-mtime-restored", wantRefuse: true, edit: func(t *testing.T, source, slotFile string) {
			info, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			editFirstByte(t, slotFile)
			if err := os.Chtimes(slotFile, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "destination-edit", wantRefuse: true, edit: func(t *testing.T, source, slotFile string) { editFirstByte(t, slotFile) }},
		{name: "source-edit-mtime-restored", wantRefuse: true, edit: func(t *testing.T, source, slotFile string) {
			info, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			editFirstByte(t, source)
			if err := os.Chtimes(source, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			pool := filepath.Join(base, "pool")
			repo := filepath.Join(base, "repo")
			slot := filepath.Join(pool, "1", "repo")
			git := func(dir string, args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			git(base, "init", "--initial-branch=main", repo)
			git(repo, "config", "user.email", "t@t")
			git(repo, "config", "user.name", "t")
			if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.ignored\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(repo, "add", ".gitignore")
			git(repo, "commit", "-m", "init")
			if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
				t.Fatal(err)
			}
			git(repo, "worktree", "add", "--detach", slot)
			source := filepath.Join(repo, "seed.ignored")
			slotFile := filepath.Join(slot, "seed.ignored")
			for _, path := range []string{source, slotFile} {
				if err := os.WriteFile(path, seed, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Make the seed's change time older than the edit's on coarse clocks.
			time.Sleep(20 * time.Millisecond)
			if tc.edit != nil {
				fired := false
				seedCompareAfterChunk = func(string) {
					if !fired {
						fired = true
						tc.edit(t, source, slotFile)
					}
				}
				t.Cleanup(func() { seedCompareAfterChunk = nil })
			}
			err = RequireCleanRemovalAllowed(repo, pool, slot, []string{"seed.ignored"})
			if tc.wantRefuse && err == nil {
				t.Errorf("an edit made during the comparison was not detected: the judgment allowed removal")
			}
			if !tc.wantRefuse && err != nil {
				t.Errorf("unchanged seeded file was refused: %v", err)
			}
			if tc.edit != nil {
				data, readErr := os.ReadFile(slotFile)
				if readErr != nil || len(data) != len(seed) {
					t.Errorf("slot file damaged by the test itself: %v", readErr)
				}
			}
		})
	}
}

func editFirstByte(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte("X"), 0); err != nil {
		t.Fatal(err)
	}
}
