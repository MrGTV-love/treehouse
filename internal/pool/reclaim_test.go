package pool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/process"
)

func TestAcquire_ForeignReclamationPreservesPausedOperation(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, operation := range []string{"merge", "rebase"} {
			t.Run(fmt.Sprintf("leased=%t/%s", leased, operation), func(t *testing.T) {
				foreign, caller, poolDir := setupSharedClonePool(t)
				path, err := Acquire(foreign, poolDir, 1, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := Release(poolDir, path); err != nil {
					t.Fatal(err)
				}
				runGit(t, path, "checkout", "-b", "operation-target")
				runGit(t, path, "commit", "--allow-empty", "-m", "remotely backed empty commit")
				runGit(t, path, "push", "origin", "operation-target")
				if operation == "merge" {
					runGit(t, path, "checkout", "--detach", "HEAD^")
					runGit(t, path, "merge", "--no-commit", "--no-ff", "operation-target")
				} else {
					cmd := exec.Command("git", "rev-parse", "HEAD")
					cmd.Dir = path
					head, err := cmd.Output()
					if err != nil {
						t.Fatal(err)
					}
					cmd = exec.Command("git", "rebase", "--interactive", "--keep-empty", "HEAD^")
					cmd.Dir = path
					// Git runs the sequence editor through its shell on every
					// platform and appends the todo filename after this redirect.
					cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR=echo edit "+strings.TrimSpace(string(head))+" >")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("pause rebase: %v\n%s", err, out)
					}
				}
				cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
				cmd.Dir = path
				if out, err := cmd.Output(); err != nil || len(out) != 0 {
					t.Fatalf("paused operation must have a clean index and checkout: %q (%v)", out, err)
				}
				before, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				if leased {
					_, err = AcquireLeaseInfoWithOptions(caller, poolDir, 1, nil, "caller", AcquireOptions{SkipFetch: true})
				} else {
					_, err = AcquireWithOptions(caller, poolDir, 1, nil, AcquireOptions{SkipFetch: true})
				}
				if err == nil {
					t.Fatal("reclaimed a clean foreign checkout with an operation in progress")
				}
				after, err := ReadState(poolDir)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed pool state: %#v -> %#v (%v)", before, after, err)
				}
				// The real consumer must still recognize the operation, not
				// merely find that the checkout's files happen to survive.
				runGit(t, path, operation, "--abort")
			})
		}
	}
}

func TestAcquire_ForeignReclamationRefusesUnsafeSlots(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, risk := range []string{"dirty", "untracked", "hidden-edits", "local-only-commits", "leased", "owner", "process", "late-process", "late-hidden-edits", "process-scan-failure", "head-locked", "stale-remote-ref", "unreachable-remote", "damaged"} {
			t.Run(fmt.Sprintf("leased=%t/%s", leased, risk), func(t *testing.T) {
				foreign, caller, poolDir := setupSharedClonePool(t)
				path, err := Acquire(foreign, poolDir, 1, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := Release(poolDir, path); err != nil {
					t.Fatal(err)
				}
				switch risk {
				case "dirty", "hidden-edits":
					if risk == "hidden-edits" {
						runGit(t, path, "update-index", "--assume-unchanged", "README.md")
					}
					if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("keep my edits\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				case "untracked":
					runGit(t, path, "config", "status.showUntrackedFiles", "no")
					if err := os.WriteFile(filepath.Join(path, "notes.txt"), []byte("keep my notes\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				case "local-only-commits":
					if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("local work\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					runGit(t, path, "add", "README.md")
					runGit(t, path, "commit", "-m", "unpublished work")
					// Even an explicitly recorded local base is not a remote backup.
					runGit(t, path, "update-ref", "refs/heads/main", "HEAD")
					state, err := ReadState(poolDir)
					if err != nil {
						t.Fatal(err)
					}
					state.Worktrees[0].BaseBranch = "main"
					if err := WriteState(poolDir, state); err != nil {
						t.Fatal(err)
					}
				case "leased":
					if _, err := LeaseExisting(poolDir, "1", "protected foreign home"); err != nil {
						t.Fatal(err)
					}
				case "owner":
					state, err := ReadState(poolDir)
					if err != nil {
						t.Fatal(err)
					}
					if err := reserveOwner(&state.Worktrees[0]); err != nil {
						t.Fatal(err)
					}
					if err := WriteState(poolDir, state); err != nil {
						t.Fatal(err)
					}
				case "process", "late-process", "late-hidden-edits", "process-scan-failure":
					oldScan := findProcessesInWorktree
					calls := 0
					findProcessesInWorktree = func(candidate string) ([]process.ProcessInfo, error) {
						if candidate != path {
							return oldScan(candidate)
						}
						calls++
						if risk == "process-scan-failure" {
							return nil, errors.New("process table unavailable")
						}
						if risk == "late-process" && calls == 1 {
							return nil, nil
						}
						if risk == "late-hidden-edits" {
							if calls == 2 {
								runGit(t, path, "update-index", "--assume-unchanged", "README.md")
								if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("late writer's work\n"), 0o644); err != nil {
									t.Fatal(err)
								}
							}
							return nil, nil
						}
						return []process.ProcessInfo{{PID: 42, Name: "foreign worker"}}, nil
					}
					t.Cleanup(func() { findProcessesInWorktree = oldScan })
				case "head-locked":
					cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-path", "HEAD")
					cmd.Dir = path
					head, err := cmd.Output()
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(strings.TrimSpace(string(head))+".lock", []byte("another Git writer\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				case "stale-remote-ref":
					remote := filepath.Join(filepath.Dir(foreign), "remote.git")
					runGit(t, remote, "config", "receive.denyDeleteCurrent", "ignore")
					runGit(t, foreign, "push", "origin", "--delete", "main")
					// A's tracking ref stays stale; no-fetch must not authorize deletion from it.
					runGit(t, foreign, "update-ref", "refs/remotes/origin/main", "HEAD")
				case "unreachable-remote":
					runGit(t, foreign, "remote", "set-url", "origin", filepath.Join(filepath.Dir(foreign), "missing.git"))
				case "damaged":
					if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: missing.git\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				before, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				readme, err := os.ReadFile(filepath.Join(path, "README.md"))
				if err != nil {
					t.Fatal(err)
				}
				var got string
				if leased {
					lease, acquireErr := AcquireLeaseInfoWithOptions(caller, poolDir, 1, nil, "caller", AcquireOptions{SkipFetch: true})
					got, err = lease.Path, acquireErr
				} else {
					got, err = AcquireWithOptions(caller, poolDir, 1, nil, AcquireOptions{SkipFetch: true})
				}
				if err == nil || got != "" {
					t.Fatalf("unsafe foreign slot must not be reclaimed or handed out: path=%q err=%v", got, err)
				}
				after, err := ReadState(poolDir)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed foreign state: %#v -> %#v (%v)", before, after, err)
				}
				if risk == "late-hidden-edits" {
					readme = []byte("late writer's work\n")
				}
				if content, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(content) != string(readme) {
					t.Fatalf("refusal removed or changed foreign work: %q (%v)", content, err)
				}
				if risk == "untracked" {
					if notes, err := os.ReadFile(filepath.Join(path, "notes.txt")); err != nil || string(notes) != "keep my notes\n" {
						t.Fatalf("untracked work was removed: %q (%v)", notes, err)
					}
				}
			})
		}
	}
}

func TestAcquire_ForeignReclamationAcceptsRemoteFeatureBranch(t *testing.T) {
	foreign, caller, poolDir := setupSharedClonePool(t)
	path, err := Acquire(foreign, poolDir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(poolDir, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("landed feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, path, "add", "README.md")
	runGit(t, path, "commit", "-m", "landed feature")
	runGit(t, path, "push", "origin", "HEAD:refs/heads/landed-feature")
	got, err := Acquire(caller, poolDir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCloneCommonDir(t, got, caller)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("remotely backed foreign slot was not removed: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(got, "README.md")); err != nil || string(content) != "hi\n" {
		t.Fatalf("caller did not get a fresh checkout of its base: %q (%v)", content, err)
	}
}

func TestAcquire_ForeignReclamationSkipsHeldSlotAndKeepsSiblingData(t *testing.T) {
	foreign, caller, poolDir := setupSharedClonePool(t)
	held, err := AcquireLeaseInfo(foreign, poolDir, 2, nil, "keep this lease")
	if err != nil {
		t.Fatal(err)
	}
	idle, err := Acquire(foreign, poolDir, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(poolDir, idle); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(idle), "keep.txt")
	if err := os.WriteFile(sibling, []byte("not worktree contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Acquire(caller, poolDir, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCloneCommonDir(t, got, caller)
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 2 || state.Worktrees[0].LeaseID != held.LeaseID || state.Worktrees[0].Path != held.Path {
		t.Fatalf("reclamation changed the held foreign slot: %#v (%v)", state, err)
	}
	if _, err := os.Stat(held.Path); err != nil {
		t.Fatalf("held foreign tree disappeared: %v", err)
	}
	if _, err := os.Stat(idle); !os.IsNotExist(err) {
		t.Fatalf("idle foreign tree was not reclaimed: %v", err)
	}
	if content, err := os.ReadFile(sibling); err != nil || string(content) != "not worktree contents\n" {
		t.Fatalf("reclamation deleted data beside the worktree: %q (%v)", content, err)
	}
}

func TestAcquire_ForeignReclamationStateWriteFailureStopsBeforeCreation(t *testing.T) {
	foreign, caller, poolDir := setupSharedClonePool(t)
	path, err := Acquire(foreign, poolDir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(poolDir, path); err != nil {
		t.Fatal(err)
	}
	oldWrite := writeState
	writeState = func(string, State) error { return errors.New("state write failed") }
	t.Cleanup(func() { writeState = oldWrite })
	got, err := Acquire(caller, poolDir, 1, nil)
	if err == nil || got != "" {
		t.Fatalf("state write failure published a replacement: %q (%v)", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fixture did not reach post-removal state failure: %v", err)
	}
	writeState = oldWrite
	got, err = Acquire(caller, poolDir, 1, nil)
	if err != nil {
		t.Fatalf("next acquisition could not heal the removed entry: %v", err)
	}
	assertCloneCommonDir(t, got, caller)
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Path != got {
		t.Fatalf("healing failed to restore shared capacity: %#v (%v)", state, err)
	}
}
