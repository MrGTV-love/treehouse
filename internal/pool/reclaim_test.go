package pool

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/config"
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
		for _, risk := range []string{"dirty", "untracked", "hidden-edits", "skip-worktree-edits", "local-only-commits", "leased", "owner", "process", "late-process", "late-hidden-edits", "late-skip-worktree-edits", "process-scan-failure", "head-locked", "stale-remote-ref", "unreachable-remote", "damaged"} {
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
				case "dirty", "hidden-edits", "skip-worktree-edits":
					switch risk {
					case "hidden-edits":
						runGit(t, path, "update-index", "--assume-unchanged", "README.md")
					case "skip-worktree-edits":
						runGit(t, path, "update-index", "--skip-worktree", "README.md")
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
				case "process", "late-process", "late-hidden-edits", "late-skip-worktree-edits", "process-scan-failure":
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
						if risk == "late-hidden-edits" || risk == "late-skip-worktree-edits" {
							// Selection now uses one shared snapshot. This per-slot scan
							// is the final pre-removal check, after that snapshot.
							if calls == 1 {
								flag := "--assume-unchanged"
								if risk == "late-skip-worktree-edits" {
									flag = "--skip-worktree"
								}
								runGit(t, path, "update-index", flag, "README.md")
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
				if risk == "late-hidden-edits" || risk == "late-skip-worktree-edits" {
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
	assertFileContents(t, filepath.Join(got, "README.md"), "hi\n")
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
	writeState = func(pool string, state State) error {
		// Claim writes now precede screening. Fail the deletion commit itself,
		// which also claims the freed capacity, so it must abort before creation.
		for _, wt := range state.Worktrees {
			if wt.Path == path {
				return oldWrite(pool, state)
			}
		}
		return errors.New("state write failed")
	}
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

func acquireReclamationCaller(caller, poolDir string, cap int, leased, skipFetch bool) (string, error) {
	opts := AcquireOptions{SkipFetch: skipFetch}
	if leased {
		lease, err := AcquireLeaseInfoWithOptions(caller, poolDir, cap, nil, "caller", opts)
		return lease.Path, err
	}
	return AcquireWithOptions(caller, poolDir, cap, nil, opts)
}

func TestAcquire_ForeignReclamationRefusesRewrittenAncestry(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, rewrite := range []string{"replace", "info-grafts", "env-grafts"} {
			t.Run(fmt.Sprintf("leased=%t/%s", leased, rewrite), func(t *testing.T) {
				foreign, caller, poolDir := setupSharedClonePool(t)
				path := idleSlots(t, foreign, poolDir, 1)[0]
				if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("unpublished fork\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, path, "add", "README.md")
				runGit(t, path, "commit", "-m", "unpublished fork H")
				head := gitOut(t, path, "rev-parse", "HEAD")
				runGit(t, foreign, "commit", "--allow-empty", "-m", "remote fork R")
				runGit(t, foreign, "push", "origin", "main")
				remoteHead := gitOut(t, foreign, "rev-parse", "HEAD")
				var graftPath string
				switch rewrite {
				case "replace":
					runGit(t, foreign, "replace", "--graft", remoteHead, head)
				case "info-grafts":
					graftPath = gitOut(t, path, "rev-parse", "--path-format=absolute", "--git-path", "info/grafts")
				case "env-grafts":
					graftPath = filepath.Join(path, ".git-grafts")
					runGit(t, path, "config", "core.excludesFile", filepath.Join(path, ".git-grafts-ignore"))
					if err := os.WriteFile(filepath.Join(path, ".git-grafts-ignore"), []byte(".git-grafts\n.git-grafts-ignore\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if graftPath != "" {
					if err := os.MkdirAll(filepath.Dir(graftPath), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(graftPath, []byte(remoteHead+" "+head+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if rewrite == "env-grafts" {
					t.Setenv("GIT_GRAFT_FILE", ".git-grafts")
				}
				if remaining := gitOut(t, path, "rev-list", head, "^"+remoteHead, "--"); remaining != "" {
					t.Fatalf("fixture did not forge remote ancestry: %s", remaining)
				}
				if status := gitOut(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
					t.Fatalf("rewritten-ancestry fixture must be clean: %s", status)
				}
				before, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				got, err := acquireReclamationCaller(caller, poolDir, 1, leased, true)
				if err == nil || got != "" {
					t.Fatalf("reclaimed unpublished HEAD using rewritten ancestry: path=%q err=%v", got, err)
				}
				after, err := ReadState(poolDir)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed pool state: %#v -> %#v (%v)", before, after, err)
				}
				if got := gitOut(t, path, "rev-parse", "HEAD"); got != head {
					t.Fatalf("refusal changed foreign HEAD: %s != %s", got, head)
				}
				assertCloneCommonDir(t, path, foreign)
				assertFileContents(t, filepath.Join(path, "README.md"), "unpublished fork\n")
				if rewrite == "replace" {
					if got := gitOut(t, path, "rev-parse", "refs/replace/"+remoteHead); got == "" {
						t.Fatal("refusal removed the replacement ref")
					}
				} else {
					assertFileContents(t, graftPath, remoteHead+" "+head+"\n")
				}
			})
		}
	}
}

func TestAcquire_ForeignReclamationPreservesReplacementTree(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, replacement := range []string{"default", "custom", "late-default", "late-custom"} {
			t.Run(fmt.Sprintf("leased=%t/%s", leased, replacement), func(t *testing.T) {
				foreign, caller, poolDir := setupSharedClonePool(t)
				path := idleSlots(t, foreign, poolDir, 1)[0]
				base := gitOut(t, path, "rev-parse", "HEAD")
				if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("unpublished replacement tree\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, path, "add", "README.md")
				runGit(t, path, "commit", "-m", "unpublished different tree D")
				different := gitOut(t, path, "rev-parse", "HEAD")
				namespace := "refs/replace/"
				if strings.HasSuffix(replacement, "custom") {
					namespace = "refs/treehouse-replacements/"
				}
				t.Setenv("GIT_REPLACE_REF_BASE", namespace)
				calls := 0
				if strings.HasPrefix(replacement, "late-") {
					runGit(t, path, "reset", "--hard", base)
					oldScan := findProcessesInWorktree
					findProcessesInWorktree = func(candidate string) ([]process.ProcessInfo, error) {
						if candidate != path {
							return oldScan(candidate)
						}
						calls++
						// Selection now uses one shared snapshot. This per-slot scan
						// is the final pre-removal check, after that snapshot.
						if calls == 1 {
							runGit(t, path, "replace", base, different)
							runGit(t, path, "read-tree", "--reset", "-u", base)
						}
						return nil, nil
					}
					t.Cleanup(func() { findProcessesInWorktree = oldScan })
				} else {
					runGit(t, path, "replace", base, different)
					runGit(t, path, "reset", "--hard", base)
					if status := gitOut(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
						t.Fatalf("replacement fixture must appear clean: %s", status)
					}
					assertFileContents(t, filepath.Join(path, "README.md"), "unpublished replacement tree\n")
				}
				before, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				got, err := acquireReclamationCaller(caller, poolDir, 1, leased, true)
				if err == nil || got != "" {
					t.Fatalf("reclaimed remote HEAD with an unpublished replacement tree: path=%q err=%v", got, err)
				}
				after, err := ReadState(poolDir)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed pool state: %#v -> %#v (%v)", before, after, err)
				}
				assertCloneCommonDir(t, path, foreign)
				assertFileContents(t, filepath.Join(path, "README.md"), "unpublished replacement tree\n")
				if got := gitOut(t, path, "rev-parse", "HEAD"); got != base {
					t.Fatalf("refusal changed foreign HEAD: %s != %s", got, base)
				}
				if got := gitOut(t, path, "rev-parse", namespace+base); got != different {
					t.Fatalf("refusal changed replacement ref: %s != %s", got, different)
				}
				if strings.HasPrefix(replacement, "late-") && calls != 1 {
					t.Fatalf("replacement missed final preflight: scans=%d", calls)
				}
			})
		}
	}
}

func TestAcquire_ForeignReclamationAuthenticatesIgnoredPaths(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, risk := range []string{"empty", "late-empty", "seeded", "beside", "prefix", "nested", "late-extra"} {
			t.Run(fmt.Sprintf("leased=%t/%s", leased, risk), func(t *testing.T) {
				foreign, caller, poolDir := setupSharedClonePool(t)
				if err := os.WriteFile(filepath.Join(foreign, ".gitignore"), []byte(".env.local\nlocal/\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				seeded := risk != "empty" && risk != "late-empty"
				if seeded {
					if err := os.WriteFile(filepath.Join(foreign, ".worktreeinclude"), []byte("local/config.env\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Join(foreign, "local"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(foreign, "local", "config.env"), []byte("authenticated seed\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					runGit(t, foreign, "add", ".worktreeinclude")
				}
				runGit(t, foreign, "add", ".gitignore")
				runGit(t, foreign, "commit", "-m", "ignored local configuration")
				runGit(t, foreign, "push", "origin", "main")
				path, err := Acquire(foreign, poolDir, 1, nil)
				if err != nil {
					t.Fatal(err)
				}
				clearOwnerReservation(t, poolDir, path)
				before, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				wantInventory := []string(nil)
				if seeded {
					wantInventory = []string{"local/config.env"}
					assertFileContents(t, filepath.Join(path, "local", "config.env"), "authenticated seed\n")
				}
				if !before.Worktrees[0].SeedInventoryKnown || !reflect.DeepEqual(before.Worktrees[0].SeededPaths, wantInventory) {
					t.Fatalf("fixture inventory is not authenticated and exact: %#v", before.Worktrees[0])
				}
				extra := ".env.local"
				switch risk {
				case "beside", "late-extra":
					extra = "local/private.env"
				case "prefix":
					extra = "local/config.env.extra"
				case "nested":
					if err := os.Remove(filepath.Join(path, "local", "config.env")); err != nil {
						t.Fatal(err)
					}
					extra = "local/config.env/private.env"
				}
				writeExtra := func() {
					t.Helper()
					full := filepath.Join(path, filepath.FromSlash(extra))
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(full, []byte("unique ignored work\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				if strings.HasPrefix(risk, "late-") {
					oldScan := findProcessesInWorktree
					findProcessesInWorktree = func(candidate string) ([]process.ProcessInfo, error) {
						if candidate != path {
							return oldScan(candidate)
						}
						calls++
						// Selection now uses one shared snapshot. This per-slot scan
						// is the final pre-removal check, after that snapshot.
						if calls == 1 {
							writeExtra()
						}
						return nil, nil
					}
					t.Cleanup(func() { findProcessesInWorktree = oldScan })
				} else if risk != "seeded" {
					writeExtra()
				}
				if status := gitOut(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
					t.Fatalf("ignored fixture must appear clean: %s", status)
				}
				got, err := acquireReclamationCaller(caller, poolDir, 1, leased, true)
				if risk == "seeded" {
					if err != nil {
						t.Fatalf("authenticated ignored seed blocked reclaim: %v", err)
					}
					assertCloneCommonDir(t, got, caller)
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("seeded foreign slot was not reclaimed: %v", err)
					}
					after, err := ReadState(poolDir)
					if err != nil || len(after.Worktrees) != 1 || after.Worktrees[0].Path != got {
						t.Fatalf("seeded reclaim did not replace foreign state: %#v (%v)", after, err)
					}
					return
				}
				if err == nil || got != "" {
					t.Fatalf("reclaimed unseeded ignored data: path=%q err=%v", got, err)
				}
				after, err := ReadState(poolDir)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed pool state: %#v -> %#v (%v)", before, after, err)
				}
				assertCloneCommonDir(t, path, foreign)
				assertFileContents(t, filepath.Join(path, filepath.FromSlash(extra)), "unique ignored work\n")
				if seeded && risk != "nested" {
					assertFileContents(t, filepath.Join(path, "local", "config.env"), "authenticated seed\n")
				}
				if strings.HasPrefix(risk, "late-") && calls != 1 {
					t.Fatalf("ignored writer missed final preflight: scans=%d", calls)
				}
			})
		}
	}
}

func TestAcquire_ForeignReclamationSkipsUnseededIgnoredSlot(t *testing.T) {
	for _, leased := range []bool{false, true} {
		t.Run(fmt.Sprintf("leased=%t", leased), func(t *testing.T) {
			foreign, caller, poolDir := setupSharedClonePool(t)
			if err := os.WriteFile(filepath.Join(foreign, ".gitignore"), []byte(".env.local\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, foreign, "add", ".gitignore")
			runGit(t, foreign, "commit", "-m", "ignore local configuration")
			runGit(t, foreign, "push", "origin", "main")
			paths := idleSlots(t, foreign, poolDir, 2)
			first, second := paths[0], paths[1]
			if err := os.WriteFile(filepath.Join(first, ".env.local"), []byte("unique ignored work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}
			got, err := acquireReclamationCaller(caller, poolDir, 2, leased, true)
			if err != nil {
				t.Fatalf("ignored-data refusal blocked disposable sibling: %v", err)
			}
			assertCloneCommonDir(t, got, caller)
			assertCloneCommonDir(t, first, foreign)
			assertFileContents(t, filepath.Join(first, ".env.local"), "unique ignored work\n")
			if _, err := os.Stat(second); !os.IsNotExist(err) {
				t.Fatalf("second foreign slot was not reclaimed: %v", err)
			}
			after, err := ReadState(poolDir)
			if err != nil || len(after.Worktrees) != 2 || !reflect.DeepEqual(before.Worktrees[0], after.Worktrees[0]) || after.Worktrees[1].Path != got {
				t.Fatalf("reclamation changed protected state instead of replacing sibling: %#v (%v)", after, err)
			}
		})
	}
}

func TestAcquire_ForeignReclamationSkipsGitRemovalRefusals(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, risk := range []string{"locked", "initialized-submodule", "retained-modules", "late-lock", "late-init"} {
			t.Run(fmt.Sprintf("leased=%t/%s", leased, risk), func(t *testing.T) {
				foreign, caller, poolDir := setupSharedClonePool(t)
				paths := idleSlots(t, foreign, poolDir, 2)
				first, second := paths[0], paths[1]
				gitDir := gitOut(t, first, "rev-parse", "--absolute-git-dir")
				if risk == "initialized-submodule" || risk == "late-init" {
					runGit(t, first, "-c", "protocol.file.allow=always", "submodule", "add", foreign, "dependency")
					runGit(t, first, "commit", "-m", "remotely backed submodule")
					runGit(t, first, "push", "origin", "HEAD:refs/heads/submodule-slot")
					if risk == "late-init" {
						runGit(t, first, "submodule", "deinit", "--force", "dependency")
						if err := os.RemoveAll(filepath.Join(gitDir, "modules")); err != nil {
							t.Fatal(err)
						}
					}
				}
				switch risk {
				case "locked":
					runGit(t, foreign, "worktree", "lock", "--reason", "keep this worktree", first)
				case "retained-modules":
					if err := os.MkdirAll(filepath.Join(gitDir, "modules"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if status := gitOut(t, first, "status", "--porcelain", "--untracked-files=all"); status != "" {
					t.Fatalf("first foreign slot must be clean: %s", status)
				}
				head := gitOut(t, first, "rev-parse", "HEAD")
				calls := 0
				if risk == "late-lock" || risk == "late-init" {
					oldScan := findProcessesInWorktree
					findProcessesInWorktree = func(candidate string) ([]process.ProcessInfo, error) {
						if candidate != first {
							return oldScan(candidate)
						}
						calls++
						// Selection now uses one shared snapshot. This per-slot scan
						// is the final pre-removal check, after that snapshot.
						if calls == 1 {
							if risk == "late-lock" {
								runGit(t, foreign, "worktree", "lock", "--reason", "late protection", first)
							} else {
								runGit(t, first, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "dependency")
							}
						}
						return nil, nil
					}
					t.Cleanup(func() { findProcessesInWorktree = oldScan })
				}
				before, err := ReadState(poolDir)
				if err != nil {
					t.Fatal(err)
				}
				got, err := acquireReclamationCaller(caller, poolDir, 2, leased, true)
				if err != nil {
					t.Fatalf("first slot's safety refusal blocked removable sibling: %v", err)
				}
				assertCloneCommonDir(t, got, caller)
				assertCloneCommonDir(t, first, foreign)
				if got := gitOut(t, first, "rev-parse", "HEAD"); got != head {
					t.Fatalf("preserved slot HEAD changed: %s != %s", got, head)
				}
				assertFileContents(t, filepath.Join(first, "README.md"), "hi\n")
				if _, err := os.Stat(second); !os.IsNotExist(err) {
					t.Fatalf("second foreign slot was not reclaimed: %v", err)
				}
				after, err := ReadState(poolDir)
				if err != nil || len(after.Worktrees) != 2 || !reflect.DeepEqual(before.Worktrees[0], after.Worktrees[0]) || after.Worktrees[1].Path != got {
					t.Fatalf("reclamation did not preserve first entry and replace second: %#v (%v)", after, err)
				}
				switch risk {
				case "locked", "late-lock":
					if _, err := os.Stat(filepath.Join(gitDir, "locked")); err != nil {
						t.Fatalf("worktree lock was not preserved: %v", err)
					}
				case "initialized-submodule", "late-init":
					assertFileContents(t, filepath.Join(first, "dependency", "README.md"), "hi\n")
					if _, err := os.Stat(filepath.Join(first, "dependency", ".git")); err != nil {
						t.Fatalf("initialized submodule was not preserved: %v", err)
					}
				case "retained-modules":
					if _, err := os.Stat(filepath.Join(gitDir, "modules")); err != nil {
						t.Fatalf("retained modules metadata was not preserved: %v", err)
					}
				}
				if strings.HasPrefix(risk, "late-") && calls != 1 {
					t.Fatalf("late protection missed final preflight: scans=%d", calls)
				}
			})
		}
	}
}

func TestAcquire_ForeignReclamationResolvesRelativeRemoteFromOwner(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, skipFetch := range []bool{false, true} {
			t.Run(fmt.Sprintf("leased=%t/skip-fetch=%t", leased, skipFetch), func(t *testing.T) {
				t.Setenv("TREEHOUSE_VCS", "git")
				seed, _ := setupRepo(t)
				base := filepath.Dir(seed)
				foreign := filepath.Join(base, "first", "myrepo")
				caller := filepath.Join(base, "second", "myrepo")
				for _, clone := range []string{foreign, caller} {
					runGit(t, "", "clone", filepath.Join(base, "remote.git"), clone)
					runGit(t, clone, "remote", "set-url", "origin", "../../remote.git")
				}
				poolRoot := filepath.Join(base, "pools", "nested", "deep")
				poolDir, err := config.ResolvePoolDir(foreign, poolRoot)
				if err != nil {
					t.Fatal(err)
				}
				otherPool, err := config.ResolvePoolDir(caller, poolRoot)
				if err != nil || otherPool != poolDir {
					t.Fatalf("nested same-origin clones must share a pool: %q != %q (%v)", otherPool, poolDir, err)
				}
				path := idleSlots(t, foreign, poolDir, 1)[0]
				got, err := acquireReclamationCaller(caller, poolDir, 1, leased, skipFetch)
				if err != nil {
					t.Fatalf("owner-relative remote prevented safe reclamation: %v", err)
				}
				assertCloneCommonDir(t, got, caller)
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("remotely backed foreign slot survived: %v", err)
				}
				assertFileContents(t, filepath.Join(got, "README.md"), "hi\n")
				state, err := ReadState(poolDir)
				if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Path != got {
					t.Fatalf("replacement was not recorded for requesting clone: %#v (%v)", state, err)
				}
			})
		}
	}
}

func TestAcquire_ForeignReclamationAcceptsLiveTipInAnyNamespace(t *testing.T) {
	for _, leased := range []bool{false, true} {
		for _, skipFetch := range []bool{false, true} {
			for _, namespace := range []string{"refs/heads/fetched-feature", "refs/treehouse/live-feature"} {
				t.Run(fmt.Sprintf("leased=%t/skip-fetch=%t/%s", leased, skipFetch, namespace), func(t *testing.T) {
					foreign, caller, poolDir := setupSharedClonePool(t)
					runGit(t, foreign, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
					path := idleSlots(t, foreign, poolDir, 1)[0]
					if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("published feature H\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					runGit(t, path, "add", "README.md")
					runGit(t, path, "commit", "-m", "published feature H")
					head := gitOut(t, path, "rev-parse", "HEAD")
					runGit(t, path, "push", "origin", "HEAD:refs/heads/feature")
					runGit(t, caller, "fetch", "origin", "feature")
					runGit(t, caller, "checkout", "--detach", "FETCH_HEAD")
					runGit(t, caller, "config", "user.email", "test@test.com")
					runGit(t, caller, "config", "user.name", "Test")
					runGit(t, caller, "commit", "--allow-empty", "-m", "remote feature R")
					remoteHead := gitOut(t, caller, "rev-parse", "HEAD")
					runGit(t, caller, "push", "origin", "HEAD:refs/heads/feature")
					runGit(t, caller, "checkout", "main")
					runGit(t, foreign, "fetch", "origin", "refs/heads/feature:"+namespace)
					runGit(t, foreign, "update-ref", "-d", "refs/remotes/origin/feature")
					if got := gitOut(t, foreign, "rev-parse", namespace); got != remoteHead {
						t.Fatalf("live tip was not fetched into selected namespace: %s != %s", got, remoteHead)
					}
					if refs := gitOut(t, foreign, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/feature", "refs/tags"); refs != "" {
						t.Fatalf("fixture accidentally retained tracking or tag evidence: %s", refs)
					}
					if got := gitOut(t, path, "rev-parse", "HEAD"); got != head || got == remoteHead {
						t.Fatalf("fixture lost the strict ancestor slot: H=%s R=%s slot=%s", head, remoteHead, got)
					}
					got, err := acquireReclamationCaller(caller, poolDir, 1, leased, skipFetch)
					if err != nil {
						t.Fatalf("live advertised descendant in %s did not back foreign HEAD: %v", namespace, err)
					}
					assertCloneCommonDir(t, got, caller)
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("remotely backed ancestor slot was not reclaimed: %v", err)
					}
					assertFileContents(t, filepath.Join(got, "README.md"), "hi\n")
					state, err := ReadState(poolDir)
					if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Path != got {
						t.Fatalf("replacement did not belong to the caller: %#v (%v)", state, err)
					}
				})
			}
		}
	}
}

func TestAcquire_ForeignReclamationTemplatedFreshNameReusesRemovedName(t *testing.T) {
	// A template beside the requester must not inherit an enclosing managed
	// pool from the test runner's scratch directory. Use the OS temp default.
	for _, name := range []string{"GOTMPDIR", "TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, "")
	}
	foreign, caller, poolDir := setupSharedClonePool(t)
	path, err := Acquire(foreign, poolDir, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Release(poolDir, path); err != nil {
		t.Fatal(err)
	}
	got, err := AcquireWithOptions(caller, poolDir, 1, nil, AcquireOptions{WorktreePath: "{repo_parent}/{repo}-{slot}"})
	if err != nil {
		t.Fatalf("templated reclamation failed: %v", err)
	}
	if want := filepath.Join(filepath.Dir(caller), filepath.Base(caller)+"-1"); got != want {
		t.Fatalf("acquired %s, want %s", got, want)
	}
	assertCloneCommonDir(t, got, caller)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("foreign slot was not reclaimed: %v", err)
	}
	state, err := ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Name != "1" || state.Worktrees[0].Path != got {
		t.Fatalf("templated reclamation state: %#v (%v)", state, err)
	}
}

func TestAcquireStaleCapacityHelper(t *testing.T) {
	if os.Getenv("TREEHOUSE_STALE_CAPACITY_HELPER") != "1" {
		t.Skip("helper process")
	}
	acquisitionStage = func(stage, path string) {
		if stage == "snapshot" {
			fmt.Println("HELD")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}
	lease, err := AcquireLeaseInfoWithOptions(os.Getenv("TREEHOUSE_STALE_REPO"), os.Getenv("TREEHOUSE_STALE_POOL"), 2, nil, "stale capacity", AcquireOptions{SkipFetch: true})
	if err != nil {
		fmt.Println("ERR " + err.Error())
		return
	}
	fmt.Println("PATH " + lease.Path)
}

type staleCapacityGet struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *strings.Builder
}

func startStaleCapacityGet(t *testing.T, caller, poolDir string) *staleCapacityGet {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireStaleCapacityHelper$")
	cmd.Env = append(os.Environ(), "TREEHOUSE_STALE_CAPACITY_HELPER=1", "TREEHOUSE_STALE_REPO="+caller, "TREEHOUSE_STALE_POOL="+poolDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	get := &staleCapacityGet{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}
	if line, err := get.stdout.ReadString('\n'); err != nil || line != "HELD\n" {
		t.Fatalf("get did not stop after its snapshot: %q (%v)\n%s", line, err, stderr)
	}
	return get
}

func (g *staleCapacityGet) finish(t *testing.T) string {
	t.Helper()
	if _, err := io.WriteString(g.stdin, "\n"); err != nil {
		t.Fatal(err)
	}
	var result string
	for {
		line, err := g.stdout.ReadString('\n')
		if strings.HasPrefix(line, "PATH ") || strings.HasPrefix(line, "ERR ") {
			result = strings.TrimSpace(line)
		}
		if err != nil {
			break
		}
	}
	if err := g.cmd.Wait(); err != nil {
		t.Fatalf("get process failed: %v\n%s", err, g.stderr)
	}
	return result
}

func TestAcquire_StaleCapacitySnapshotReclaimsForeignOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper processes hold a Unix-tested stdin schedule")
	}
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprintf("guarded=%t", guarded), func(t *testing.T) {
			foreign, caller, poolDir := setupSharedClonePool(t)
			x, err := Acquire(foreign, poolDir, 2, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := Release(poolDir, x); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(x, "keep.txt")
			if guarded {
				if err := os.WriteFile(keep, []byte("foreign work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before, err := ReadState(poolDir)
			if err != nil || len(before.Worktrees) != 1 {
				t.Fatalf("fixture must hold N-1 entries: %#v (%v)", before, err)
			}
			a := startStaleCapacityGet(t, caller, poolDir)
			b := startStaleCapacityGet(t, caller, poolDir)
			first := a.finish(t)
			if !strings.HasPrefix(first, "PATH ") {
				t.Fatalf("first get failed: %s\n%s", first, a.stderr)
			}
			second := b.finish(t)
			checked := strings.Count(b.stderr.String(), "treehouse: checked "+x+":")
			if checked != 1 {
				t.Fatalf("second get reported X %d times:\n%s", checked, b.stderr)
			}
			state, err := ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}
			if guarded {
				if !strings.HasPrefix(second, "ERR all 2 worktrees are in use") {
					t.Fatalf("guarded X did not end with the capacity refusal: %s\n%s", second, b.stderr)
				}
				if data, err := os.ReadFile(keep); err != nil || string(data) != "foreign work\n" {
					t.Fatalf("guarded X lost its contents: %q (%v)", data, err)
				}
				for _, wt := range state.Worktrees {
					if wt.Path == x && !reflect.DeepEqual(wt, before.Worktrees[0]) {
						t.Fatalf("second pass left a reservation on X: %#v", wt)
					}
				}
				if len(state.Worktrees) != 2 {
					t.Fatalf("state changed beyond the first get: %#v", state)
				}
				return
			}
			if !strings.HasPrefix(second, "PATH ") || second == first {
				t.Fatalf("second get did not reclaim X: %s (first %s)\n%s", second, first, b.stderr)
			}
			if _, err := os.Stat(x); !os.IsNotExist(err) {
				t.Fatalf("X was not removed: %v", err)
			}
			if strings.Contains(a.stderr.String(), "reclaimed idle foreign-clone worktree") || strings.Count(b.stderr.String(), "reclaimed idle foreign-clone worktree") != 1 {
				t.Fatalf("X was not removed exactly once:\nA:\n%s\nB:\n%s", a.stderr, b.stderr)
			}
			if len(state.Worktrees) != 2 {
				t.Fatalf("state after both gets: %#v", state)
			}
			for _, wt := range state.Worktrees {
				if wt.Path == x || !wt.Leased || wt.LeaseHolder != "stale capacity" {
					t.Fatalf("unexpected entry after both gets: %#v", wt)
				}
				assertCloneCommonDir(t, wt.Path, caller)
			}
		})
	}
}
