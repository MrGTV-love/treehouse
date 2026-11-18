package pool

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setupIsolatedPruneEnv(t *testing.T, poolDir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("TREEHOUSE_ROOT", filepath.Dir(poolDir))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func commitAndPush(t *testing.T, repo, message string) {
	t.Helper()
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", message)
	runGit(t, repo, "push", "origin", "main")
}

// idleAndKnownEmpty clears the owner reservations and, for the named slots,
// forces the authenticated known-empty inventory a pre-3.0 adopted slot carries
// (SeedInventoryKnown with no paths); WriteState re-authenticates it.
func idleAndKnownEmpty(t *testing.T, poolDir string, legacy ...string) {
	t.Helper()
	if err := WithStateLock(poolDir, func() error {
		state, err := ReadState(poolDir)
		if err != nil {
			return err
		}
		for i := range state.Worktrees {
			wt := &state.Worktrees[i]
			wt.OwnerPID, wt.OwnerStartedAt = 0, 0
			for _, name := range legacy {
				if wt.Path == name {
					wt.SeededPaths, wt.SeedInventoryKnown, wt.SeedBackend, wt.SeedAuthIdentity = nil, true, "", ""
				}
			}
		}
		return WriteState(poolDir, state)
	}); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range state.Worktrees {
		for _, name := range legacy {
			if wt.Path == name && (!wt.SeedInventoryKnown || len(wt.SeededPaths) != 0) {
				t.Fatalf("legacy known-empty inventory was not established: %#v", wt)
			}
		}
	}
}

func registeredPaths(t *testing.T, poolDir string) []string {
	t.Helper()
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, wt := range state.Worktrees {
		paths = append(paths, wt.Path)
	}
	return paths
}

// N4 / X4: a pre-3.0 adopted slot with a known-empty inventory counts EVERY
// ignored path as unseeded, even when a seeded companion slot in the same pool
// lists the same relative path and the legacy file, the companion's file and the
// seed source all hold equal bytes. A legacy control slot without the file is
// removed. "final" injects the same file into the control after the plan step
// (a real pre-destroy hook) and the final check must refuse.
func TestPruneLegacyKnownEmptySlotBesideSeededCompanion(t *testing.T) {
	const payload = "payload\n"
	build := func(t *testing.T) (repo, poolDir, legacyFile, legacyControl, companion string) {
		repo, poolDir = setupRepo(t)
		setupIsolatedPruneEnv(t, poolDir)
		writeTestFile(t, filepath.Join(repo, ".gitignore"), "*.ignored\n")
		commitAndPush(t, repo, "ignore fixture")
		var err error
		if legacyFile, err = Acquire(repo, poolDir, 3, nil); err != nil {
			t.Fatal(err)
		}
		if legacyControl, err = Acquire(repo, poolDir, 3, nil); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(repo, ".worktreeinclude"), "x.ignored\n")
		writeTestFile(t, filepath.Join(repo, "x.ignored"), payload)
		commitAndPush(t, repo, "seed manifest")
		if companion, err = Acquire(repo, poolDir, 3, nil); err != nil {
			t.Fatal(err)
		}
		idleAndKnownEmpty(t, poolDir, legacyFile, legacyControl)
		state, err := ReadState(poolDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, wt := range state.Worktrees {
			if wt.Path == companion && !reflect.DeepEqual(wt.SeededPaths, []string{"x.ignored"}) {
				t.Fatalf("companion is not seeded with the same relative path: %#v", wt)
			}
		}
		return
	}
	t.Run("plan", func(t *testing.T) {
		repo, poolDir, legacyFile, legacyControl, companion := build(t)
		file := filepath.Join(legacyFile, "x.ignored")
		writeTestFile(t, file, payload)
		if status := gitOut(t, legacyFile, "status", "--porcelain", "--untracked-files=all"); status != "" {
			t.Fatalf("fixture is not ignored-only: %q", status)
		}
		dry, err := Prune(repo, poolDir, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(dry.Candidates) != 2 || len(dry.Skipped) != 1 || dry.Skipped[0].Path != legacyFile || dry.Skipped[0].Category != PruneSkipIgnoredFiles {
			t.Fatalf("plan did not refuse exactly the legacy slot holding the file: %+v", dry)
		}
		real, err := Prune(repo, poolDir, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(real.Pruned) != 2 || len(real.Skipped) != 1 {
			t.Fatalf("expected the control and the companion pruned and the legacy slot kept: %+v", real)
		}
		assertFileContents(t, file, payload)
		if got := registeredPaths(t, poolDir); !reflect.DeepEqual(got, []string{legacyFile}) {
			t.Errorf("registrations after prune: %v", got)
		}
		for _, gone := range []string{legacyControl, companion} {
			if _, err := os.Lstat(gone); !os.IsNotExist(err) {
				t.Errorf("regenerable or clean slot %s was not removed: %v", gone, err)
			}
		}
	})
	t.Run("final", func(t *testing.T) {
		repo, poolDir, legacyFile, legacyControl, companion := build(t)
		_ = legacyFile
		injected := filepath.Join(legacyControl, "x.ignored")
		// A real hook runs after planning and reservation, before the final check.
		hook := "printf 'payload\\n' > " + quoteForShell(injected)
		result, err := Prune(repo, poolDir, false, []string{hook})
		if err != nil {
			t.Fatal(err)
		}
		refused := false
		for _, skipped := range result.Skipped {
			if skipped.Path == legacyControl && skipped.Category == PruneSkipIgnoredFiles {
				refused = true
			}
		}
		if !refused {
			t.Fatalf("final check did not refuse the late file in the legacy control: %+v", result)
		}
		assertFileContents(t, injected, payload)
		// The late file is removed; the next prune drains the control with no stale reservation.
		if err := os.Remove(injected); err != nil {
			t.Fatal(err)
		}
		retry, err := Prune(repo, poolDir, false, nil)
		if err != nil || len(retry.Skipped) != 0 {
			t.Fatalf("resolved condition left a skip or stale reservation: %+v (%v)", retry, err)
		}
		for _, path := range registeredPaths(t, poolDir) {
			if path == legacyControl || path == companion {
				t.Errorf("slot %s left registered after the resolved retry", path)
			}
		}
	})
}

// N4 / X10 Run C: the real prune removes the control through native `git
// worktree remove` (GIT_TRACE: git itself writes the record, so the real
// operation always runs) and through removeWorktreeDirectory, observed by a
// test-only wrapper that records the call and delegates to the real function.
// The refused modified slot reaches neither. The bypass subtest proves the
// observer discriminates: with the real call skipped, the slot parent remains.
func TestPruneRemovalOperationsAreObserved(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		name := "delegating"
		if bypass {
			name = "bypassed-directory-removal"
		}
		t.Run(name, func(t *testing.T) {
			repo, poolDir := setupRepo(t)
			setupIsolatedPruneEnv(t, poolDir)
			writeTestFile(t, filepath.Join(repo, ".gitignore"), "*.ignored\n")
			writeTestFile(t, filepath.Join(repo, ".worktreeinclude"), "seed.ignored\n")
			writeTestFile(t, filepath.Join(repo, "seed.ignored"), "original\n")
			commitAndPush(t, repo, "seed fixture")
			modified, err := Acquire(repo, poolDir, 2, nil)
			if err != nil {
				t.Fatal(err)
			}
			control, err := Acquire(repo, poolDir, 2, nil)
			if err != nil {
				t.Fatal(err)
			}
			idleAndKnownEmpty(t, poolDir)
			writeTestFile(t, filepath.Join(modified, "seed.ignored"), "modified\n")
			trace := filepath.Join(t.TempDir(), "git-trace.log")
			t.Setenv("GIT_TRACE", trace)
			var calls []string
			original := pruneRemoveWorktreeDirectory
			pruneRemoveWorktreeDirectory = func(pool, path string) error {
				calls = append(calls, path)
				if bypass {
					return nil
				}
				return original(pool, path)
			}
			t.Cleanup(func() { pruneRemoveWorktreeDirectory = original })
			result, err := Prune(repo, poolDir, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Pruned) != 1 || len(result.Skipped) != 1 || result.Skipped[0].Path != modified {
				t.Fatalf("expected the control pruned and the modified slot skipped: %+v", result)
			}
			assertFileContents(t, filepath.Join(modified, "seed.ignored"), "modified\n")
			if !reflect.DeepEqual(calls, []string{control}) {
				t.Errorf("removeWorktreeDirectory calls = %v, want only the control %q", calls, control)
			}
			traceData, _ := os.ReadFile(trace)
			removed := false
			for _, line := range strings.Split(string(traceData), "\n") {
				if strings.Contains(line, "worktree remove") && strings.Contains(line, control) {
					removed = true
				}
				if strings.Contains(line, "worktree remove") && strings.Contains(line, modified) {
					t.Errorf("native removal reached the refused slot: %s", line)
				}
			}
			if !removed {
				t.Errorf("no native git worktree remove of the control in the trace:\n%s", traceData)
			}
			_, parentErr := os.Lstat(filepath.Dir(control))
			if bypass {
				if parentErr != nil {
					t.Errorf("bypassed directory removal still removed the slot parent: %v", parentErr)
				}
			} else if !os.IsNotExist(parentErr) {
				t.Errorf("delegating observer: slot parent not removed: %v", parentErr)
			}
		})
	}
}

// N2: the seed source's owner repository lies INSIDE the pool directory. The
// source is a different file (distinct inode) with equal bytes and its parent
// is an ordinary directory, so rooted parent and inode checks alone allow it;
// only the pool boundary refuses. A boundary-only mutant prunes the slot and
// drops its registration. The state's other entries (a recovered owner entry,
// if recovery creates one) must be left exactly as they were.
func TestPruneSeedSourceInsidePoolIsRefused(t *testing.T) {
	repo, _ := setupRepo(t)
	poolDir := filepath.Dir(repo) // the owner repository now lies inside the pool directory
	setupIsolatedPruneEnv(t, poolDir)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "*.ignored\n")
	writeTestFile(t, filepath.Join(repo, ".worktreeinclude"), "seed.ignored\n")
	writeTestFile(t, filepath.Join(repo, "seed.ignored"), "original\n")
	commitAndPush(t, repo, "seed fixture")
	slot, err := Acquire(repo, poolDir, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Clear only the acquired slot's lease/owner; leave every other entry alone.
	var others []WorktreeEntry
	if err := WithStateLock(poolDir, func() error {
		state, err := ReadState(poolDir)
		if err != nil {
			return err
		}
		for i := range state.Worktrees {
			wt := &state.Worktrees[i]
			if wt.Path == slot {
				wt.Leased, wt.LeaseID, wt.LeaseHolder, wt.OwnerPID, wt.OwnerStartedAt = false, "", "", 0, 0
			} else {
				others = append(others, *wt)
			}
		}
		return WriteState(poolDir, state)
	}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(slot, "seed.ignored")
	assertFileContents(t, file, "original\n")
	result, err := Prune(repo, poolDir, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pruned) != 0 {
		t.Fatalf("slot whose seed source lies inside the pool was pruned: %+v", result)
	}
	assertFileContents(t, file, "original\n")
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	var after []WorktreeEntry
	for _, wt := range state.Worktrees {
		if wt.Path == slot {
			found = true
		} else {
			after = append(after, wt)
		}
	}
	if !found {
		t.Errorf("refused slot lost its registration: %+v", state.Worktrees)
	}
	if !reflect.DeepEqual(others, after) {
		t.Errorf("other pool entries changed: before=%+v after=%+v", others, after)
	}
}
