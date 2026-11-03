package pool

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/process"
)

// A recovered entry is a persistent quarantine: no command frees it, moves its
// contents, or clears its lease as a side effect of another command. These
// tests pin that, and the one way out: a return that names the slot.

func recoveredFixture(t *testing.T) (string, string, string) {
	t.Helper()
	repo, poolDir := setupLocalRepo(t)
	path := idleSlots(t, repo, poolDir, 1)[0]
	writeRawState(t, poolDir, State{Version: stateVersion, Worktrees: []WorktreeEntry{{Name: "1", Path: path, Leased: true, LeaseHolder: RecoveredLeaseHolder}}})
	return repo, poolDir, path
}

// quarantine marks the named idle slots as recovered, the way ReadState does
// for an entry whose seed inventory fails to authenticate, and leaves every
// other entry validly signed and idle.
func quarantine(t *testing.T, poolDir string, names ...string) {
	t.Helper()
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range state.Worktrees {
		wt := &state.Worktrees[i]
		for _, name := range names {
			if wt.Name == name {
				wt.Leased, wt.LeaseHolder = true, RecoveredLeaseHolder
				setSeedInventory(wt, nil, false)
			}
		}
	}
	if err := WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func backupFolders(t *testing.T, poolDir string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(filepath.Dir(poolDir), "treehouse-recovered-backup-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func commitInSlot(t *testing.T, path, file string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, file), []byte("commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", file}, {"commit", "-m", "unlanded " + file}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// stillQuarantined fails unless the entry is exactly as writeRawState left it.
func stillQuarantined(t *testing.T, poolDir, path string) {
	t.Helper()
	wt := entryFor(t, poolDir, path)
	if !wt.Leased || wt.LeaseHolder != RecoveredLeaseHolder || wt.SeedInventoryKnown || wt.LeaseID != "" {
		t.Fatalf("recovered entry changed: %#v", wt)
	}
}

func TestWithStateLockRunsNoRecovery(t *testing.T) {
	_, poolDir, path := recoveredFixture(t)
	file := filepath.Join(path, "scratch", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := WithStateLock(poolDir, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("callback called = %v, err = %v", called, err)
	}
	after, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("WithStateLock rewrote the state file:\nbefore: %s\nafter:  %s", before, after)
	}
	assertFileContents(t, file, "keep me\n")
	if dirs := backupFolders(t, poolDir); len(dirs) != 0 {
		t.Fatalf("backup folders created: %v", dirs)
	}
}

// TestRecoveredLeaseSurvivesEveryCommand runs every command that takes the
// state lock, in a fresh pool for each, against a recovered entry whose root is
// in each condition the old sweep used to judge: clean and landed (which it
// freed), tracked-dirty, unlanded, with untracked files, and with a nested
// repository. The entry, its files and the pool's parent directory must be
// exactly as they were.
func TestRecoveredLeaseSurvivesEveryCommand(t *testing.T) {
	conditions := map[string]func(t *testing.T, path string) (file, want string){
		"clean and landed": func(t *testing.T, path string) (string, string) { return "README.md", "hi\n" },
		"tracked-dirty": func(t *testing.T, path string) (string, string) {
			if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return "README.md", "changed\n"
		},
		"unlanded head": func(t *testing.T, path string) (string, string) {
			commitInSlot(t, path, "new.txt")
			return "new.txt", "commit\n"
		},
		"untracked files": func(t *testing.T, path string) (string, string) {
			if err := os.MkdirAll(filepath.Join(path, "scratch"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "scratch", "notes.txt"), []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return filepath.Join("scratch", "notes.txt"), "keep\n"
		},
		"nested repository with an unlanded commit": func(t *testing.T, path string) (string, string) {
			nested := filepath.Join(path, ".claude", "worktrees", "live")
			runGit(t, "", "clone", "-q", "--no-hardlinks", path, nested)
			runGit(t, nested, "config", "user.email", "test@test.com")
			runGit(t, nested, "config", "user.name", "Test")
			commitInSlot(t, nested, "owned.txt")
			return filepath.Join(".claude", "worktrees", "live", "owned.txt"), "commit\n"
		},
	}
	commands := map[string]func(t *testing.T, repo, poolDir, recovered, free string){
		"status": func(t *testing.T, repo, poolDir, recovered, free string) {
			if _, err := List(poolDir); err != nil {
				t.Fatal(err)
			}
		},
		"lease of the entry itself is refused": func(t *testing.T, repo, poolDir, recovered, free string) {
			if _, err := LeaseExisting(poolDir, "1", "someone"); err == nil || !strings.Contains(err.Error(), "already leased") {
				t.Fatalf("LeaseExisting = %v, want already leased", err)
			}
		},
		"lease of a free sibling": func(t *testing.T, repo, poolDir, recovered, free string) {
			if _, err := LeaseExisting(poolDir, "2", "someone"); err != nil {
				t.Fatal(err)
			}
		},
		"get takes the free sibling": func(t *testing.T, repo, poolDir, recovered, free string) {
			got, err := AcquireLease(repo, poolDir, 2, nil, "new-task")
			if err != nil {
				t.Fatal(err)
			}
			if samePath(got, recovered) {
				t.Fatalf("get returned the recovered slot %s", got)
			}
		},
		"failed conditional return of a sibling": func(t *testing.T, repo, poolDir, recovered, free string) {
			wrong := "not-the-holder"
			err := ReleaseConditional(poolDir, free, "", ReleasePreconditions{ExpectedLeaseHolder: &wrong}, nil)
			if err == nil {
				t.Fatal("conditional return with a wrong holder succeeded")
			}
		},
		"failed conditional return of the entry itself": func(t *testing.T, repo, poolDir, recovered, free string) {
			wrong := "not-the-holder"
			if err := ReleaseConditional(poolDir, recovered, "", ReleasePreconditions{ExpectedLeaseHolder: &wrong}, nil); err == nil {
				t.Fatal("conditional return with a wrong holder succeeded")
			}
		},
	}
	for cname, setup := range conditions {
		for aname, act := range commands {
			t.Run(cname+"/"+aname, func(t *testing.T) {
				repo, poolDir := setupLocalRepo(t)
				paths := idleSlots(t, repo, poolDir, 2)
				recovered := paths[0]
				quarantine(t, poolDir, "1")
				file, want := setup(t, recovered)
				act(t, repo, poolDir, recovered, paths[1])
				stillQuarantined(t, poolDir, recovered)
				assertFileContents(t, filepath.Join(recovered, file), want)
				if dirs := backupFolders(t, poolDir); len(dirs) != 0 {
					t.Fatalf("backup folders created: %v", dirs)
				}
			})
		}
	}
}

func TestRecoveredLeaseSurvivesWhateverProcessState(t *testing.T) {
	for name, procs := range map[string][]process.ProcessInfo{
		"no process":   nil,
		"live process": {{PID: 42, Name: "zsh"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, poolDir, path := recoveredFixture(t)
			oldScan, oldFilter := findProcessesInWorktree, dropProtectedProcesses
			findProcessesInWorktree = func(string) ([]process.ProcessInfo, error) { return procs, nil }
			dropProtectedProcesses = func(p []process.ProcessInfo) ([]process.ProcessInfo, error) { return p, nil }
			t.Cleanup(func() { findProcessesInWorktree, dropProtectedProcesses = oldScan, oldFilter })
			for range 2 {
				if st := statusOf(t, poolDir, path); st.Status != StatusLeased || st.LeaseHolder != RecoveredLeaseHolder {
					t.Fatalf("status = %+v", st)
				}
			}
			stillQuarantined(t, poolDir, path)
		})
	}
}

// Returning a sibling used to free every recovered entry that looked safe.
func TestReturnOfSiblingLeavesRecoveredEntryQuarantined(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 2)
	writeRawState(t, poolDir, State{Version: stateVersion, Worktrees: []WorktreeEntry{
		{Name: "1", Path: paths[0], Leased: true, LeaseHolder: RecoveredLeaseHolder},
		{Name: "2", Path: paths[1], Leased: true, LeaseID: "0123456789abcdef0123456789abcdef", LeaseHolder: "agent"},
	}})
	if err := Release(poolDir, paths[1]); err != nil {
		t.Fatal(err)
	}
	if wt := entryFor(t, poolDir, paths[1]); wt.Leased {
		t.Fatalf("returned slot still leased: %#v", wt)
	}
	stillQuarantined(t, poolDir, paths[0])
}

func TestAcquireSkipsRecoveredEntry(t *testing.T) {
	t.Run("a free slot is chosen, and only it is reset", func(t *testing.T) {
		repo, poolDir := setupLocalRepo(t)
		paths := idleSlots(t, repo, poolDir, 2)
		quarantine(t, poolDir, "1")
		dirty := filepath.Join(paths[0], "README.md")
		if err := os.WriteFile(dirty, []byte("recovered work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Advance main so the reset of the candidate is observable.
		runGit(t, repo, "commit", "--allow-empty", "-m", "advance main")
		tip := gitOutput(t, repo, "rev-parse", "main")
		got, err := AcquireLease(repo, poolDir, 2, nil, "new-task")
		if err != nil {
			t.Fatal(err)
		}
		if !samePath(got, paths[1]) {
			t.Fatalf("get returned %s, want the free slot %s", got, paths[1])
		}
		assertFileContents(t, dirty, "recovered work\n")
		if head := gitOutput(t, paths[1], "rev-parse", "HEAD"); head != tip {
			t.Fatalf("the candidate is at %s, want it reset to main at %s", head, tip)
		}
		stillQuarantined(t, poolDir, paths[0])
	})
	t.Run("at capacity it reports exhaustion and frees nothing", func(t *testing.T) {
		repo, poolDir, path := recoveredFixture(t)
		file := filepath.Join(path, "scratch", "notes.txt")
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := AcquireLease(repo, poolDir, 1, nil, "new-task")
		if err == nil || !strings.Contains(err.Error(), "max_trees = 1") {
			t.Fatalf("get = %v, want a capacity error", err)
		}
		stillQuarantined(t, poolDir, path)
		assertFileContents(t, file, "keep\n")
		if dirs := backupFolders(t, poolDir); len(dirs) != 0 {
			t.Fatalf("backup folders created: %v", dirs)
		}
	})
}

// The way out of the quarantine is a return that names the slot. It must leave
// the entry trusted: the next state read must not quarantine it again, and the
// slot must be allocatable.
func TestNamedReturnReleasesRecoveredEntryForReuse(t *testing.T) {
	repo, poolDir, path := recoveredFixture(t)
	holder := RecoveredLeaseHolder
	if err := ReleaseConditional(poolDir, path, "", ReleasePreconditions{ExpectedLeaseHolder: &holder}, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if st := statusOf(t, poolDir, path); st.Status != StatusAvailable || st.LeaseHolder != "" || st.RecoveryReason != "" {
			t.Fatalf("returned slot reads %+v", st)
		}
	}
	if wt := entryFor(t, poolDir, path); wt.Leased || !wt.SeedInventoryKnown {
		t.Fatalf("returned entry reads back as %#v", wt)
	}
	got, err := AcquireLease(repo, poolDir, 1, nil, "next")
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(got, path) {
		t.Fatalf("get returned %s, want the returned slot %s", got, path)
	}
}

func TestRecoveryReasonClearedByNamedReturn(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	path := idleSlots(t, repo, poolDir, 1)[0]
	writeRawState(t, poolDir, State{Version: stateVersion, Worktrees: []WorktreeEntry{{Name: "1", Path: path, Leased: true, LeaseHolder: RecoveredLeaseHolder, RecoveryReason: "a reason an earlier version recorded"}}})
	if st := statusOf(t, poolDir, path); st.RecoveryReason != "a reason an earlier version recorded" {
		t.Fatalf("status = %+v", st)
	}
	if err := Release(poolDir, path); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, poolDir, path); st.Status != StatusAvailable || st.RecoveryReason != "" {
		t.Fatalf("returned slot reads %+v", st)
	}
}

func TestStatusExplainsPersistentQuarantine(t *testing.T) {
	_, poolDir, path := recoveredFixture(t)
	st := statusOf(t, poolDir, path)
	if st.Status != StatusLeased || st.RecoveryReason != recoveredQuarantineReason {
		t.Fatalf("status = %+v", st)
	}
	if wt := entryFor(t, poolDir, path); wt.RecoveryReason != "" {
		t.Fatalf("status persisted a reason: %#v", wt)
	}
}

func TestDamagedRecoveredEntryStaysQuarantined(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	path := idleSlots(t, repo, poolDir, 1)[0]
	writeRawState(t, poolDir, State{Version: stateVersion, Worktrees: []WorktreeEntry{{Name: "1", Path: path, Leased: true, LeaseHolder: RecoveredLeaseHolder, RecoveryError: "marker loop"}}})
	st := statusOf(t, poolDir, path)
	if st.Status != StatusDamaged || st.LeaseHolder != RecoveredLeaseHolder || st.BranchErr != "marker loop" {
		t.Fatalf("status = %+v", st)
	}
}

// Backup folders an earlier version made are still reported, never created or
// written by this one.
func TestStatusReportsBackupFolderOfEarlierVersion(t *testing.T) {
	_, poolDir, path := recoveredFixture(t)
	backup := recoveryBackupDir(poolDir, "1")
	if st := statusOf(t, poolDir, path); st.RecoveryBackup != "" {
		t.Fatalf("status reports a backup that does not exist: %+v", st)
	}
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, poolDir, path); st.RecoveryBackup != "" {
		t.Fatalf("status reports an empty backup folder: %+v", st)
	}
	if err := os.WriteFile(filepath.Join(backup, "notes.txt"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := statusOf(t, poolDir, path); st.RecoveryBackup != backup {
		t.Fatalf("status = %+v, want backup %s", st, backup)
	}
	assertFileContents(t, filepath.Join(backup, "notes.txt"), "kept\n")
}

// A conditional return must judge its condition under the state lock, after
// anything that changed the lease while it waited: a stale condition refuses
// and leaves the entry, whoever now holds it, and every sibling alone.
func TestConditionalReturnRefusesHolderChangedWhileWaitingForTheLock(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 2)
	quarantine(t, poolDir, "2")
	if _, err := LeaseExisting(poolDir, "1", "original"); err != nil {
		t.Fatal(err)
	}
	holder := "original"
	locked, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = WithStateLock(poolDir, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	done := make(chan error, 1)
	go func() {
		done <- ReleaseConditional(poolDir, paths[0], "", ReleasePreconditions{ExpectedLeaseHolder: &holder}, nil)
	}()
	// The writer below only runs once the lock is free, so the return is
	// already queued behind it; change the holder first, then let both go.
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range state.Worktrees {
		if state.Worktrees[i].Name == "1" {
			state.Worktrees[i].LeaseHolder = "someone-else"
			state.Worktrees[i].LeaseID = "0123456789abcdef0123456789abcdef"
		}
	}
	if err := WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrLeasePreconditionFailed) {
		t.Fatalf("ReleaseConditional = %v, want ErrLeasePreconditionFailed", err)
	}
	wt := entryFor(t, poolDir, paths[0])
	if !wt.Leased || wt.LeaseHolder != "someone-else" || wt.LeaseID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("the new holder's lease was disturbed: %#v", wt)
	}
	if sib := entryFor(t, poolDir, paths[1]); !sib.Leased || sib.LeaseHolder != RecoveredLeaseHolder {
		t.Fatalf("a sibling changed: %#v", sib)
	}
}
