package pool

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/process"
)

func TestAcquireConcurrentCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix lock probe and socket instrumentation")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the Unix CLI instrumentation")
	}
	binary := filepath.Join(t.TempDir(), "treehouse")
	// Function entry counters observe reclamation passes, including passes
	// that refuse every record without starting a candidate Git subprocess.
	build := exec.Command("go", "build", "-cover", "-covermode=atomic", "-o", binary, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	cmd := exec.Command(python, "testdata/acquisition_lab.py", binary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CLI lab: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

func acquisitionFixture(t *testing.T, count int) (string, string, []string) {
	t.Helper()
	repo, pool := setupRepo(t)
	paths := make([]string, count)
	state := State{}
	for i := range paths {
		name := fmt.Sprint(i + 1)
		path := filepath.Join(pool, name, "myrepo")
		runGit(t, repo, "worktree", "add", "--detach", path, "main")
		paths[i] = path
		entry := WorktreeEntry{Name: name, Path: path, CreatedAt: time.Now()}
		setSeedInventory(&entry, nil, true)
		state.Worktrees = append(state.Worktrees, entry)
	}
	if err := os.MkdirAll(pool, 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(pool, state); err != nil {
		t.Fatal(err)
	}
	return repo, pool, paths
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func checkoutFixtureFile(t *testing.T, repo, object string) string {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "--filters", object)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git cat-file --filters %s: %v", object, err)
	}
	return string(out)
}

func probePoolLock(t *testing.T, pool string) string {
	t.Helper()
	f, err := os.OpenFile(lockFilePath(pool), os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tryLockFile(f); err != nil {
		return "LOCKED"
	}
	if err := unlockFile(f); err != nil {
		t.Fatal(err)
	}
	return "FREE"
}

func assertPoolUnlocked(t *testing.T, pool string) {
	t.Helper()
	if probePoolLock(t, pool) != "FREE" {
		t.Fatal("pool lock held at acquisition synchronization point")
	}
}

func TestAcquireSelectionSnapshotOutsidePoolLock(t *testing.T) {
	repo, pool, paths := acquisitionFixture(t, 21)
	for _, path := range paths[:20] {
		runGit(t, path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "unlanded")
		if err := os.WriteFile(filepath.Join(path, "delta"), []byte("unlanded"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit(t, path, "add", "delta")
		runGit(t, path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "unlanded content")
	}
	oldSnapshot, oldFinal := snapshotWorktreeProcesses, findProcessesInWorktree
	calls, finals, locked := 0, 0, 0
	snapshotWorktreeProcesses = func() (process.WorktreeSnapshot, error) {
		calls++
		status := probePoolLock(t, pool)
		if status != "FREE" {
			locked++
		}
		t.Logf("%s selection enumeration %d", status, calls)
		return oldSnapshot()
	}
	findProcessesInWorktree = func(path string) ([]process.ProcessInfo, error) {
		finals++
		status := probePoolLock(t, pool)
		if status != "FREE" {
			locked++
		}
		t.Logf("%s final enumeration %d", status, finals)
		return oldFinal(path)
	}
	t.Cleanup(func() { snapshotWorktreeProcesses = oldSnapshot; findProcessesInWorktree = oldFinal })
	got, err := AcquireLeaseInfoWithOptions(repo, pool, 21, nil, "selection", AcquireOptions{SkipFetch: true})
	if err != nil || got.Path != paths[20] {
		t.Fatalf("selected %q: %v", got.Path, err)
	}
	if calls != 1 || finals != 1 || locked != 0 {
		t.Fatalf("selection snapshots=%d final scans=%d locked=%d", calls, finals, locked)
	}
}

func TestAcquireForeignSelectionSnapshotOutsidePoolLock(t *testing.T) {
	repo, pool, paths := acquisitionFixture(t, 21)
	caller := filepath.Join(t.TempDir(), "myrepo")
	runGit(t, repo, "clone", filepath.Join(filepath.Dir(repo), "remote.git"), caller)
	for _, path := range paths[:20] {
		if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("unlanded foreign work\n"), 0644); err != nil {
			t.Fatal(err)
		}
		runGit(t, path, "add", "README.md")
		runGit(t, path, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "unlanded")
	}
	oldSnapshot, oldFinal, oldStage := snapshotWorktreeProcesses, findProcessesInWorktree, acquisitionStage
	ownSelections, foreignSelections, finals, locked := 0, 0, 0, 0
	inForeignWalk := false
	acquisitionStage = func(stage, path string) {
		// The walk starts before its snapshot, not at its first candidate.
		if stage == "foreign-snapshot" {
			inForeignWalk = true
		}
		oldStage(stage, path)
	}
	snapshotWorktreeProcesses = func() (process.WorktreeSnapshot, error) {
		walk := "own"
		if inForeignWalk {
			foreignSelections++
			walk = "foreign"
		} else {
			ownSelections++
		}
		status := probePoolLock(t, pool)
		if status != "FREE" {
			locked++
		}
		t.Logf("%s %s selection enumeration own=%d foreign=%d", status, walk, ownSelections, foreignSelections)
		return oldSnapshot()
	}
	findProcessesInWorktree = func(path string) ([]process.ProcessInfo, error) {
		finals++
		status := probePoolLock(t, pool)
		if status != "FREE" {
			locked++
		}
		t.Logf("%s foreign final enumeration %d slot=%s", status, finals, path)
		return oldFinal(path)
	}
	t.Cleanup(func() {
		snapshotWorktreeProcesses = oldSnapshot
		findProcessesInWorktree = oldFinal
		acquisitionStage = oldStage
	})
	got, err := AcquireLeaseInfoWithOptions(caller, pool, 21, nil, "foreign selection", AcquireOptions{SkipFetch: true})
	if err != nil || got.Path == "" {
		t.Fatalf("foreign get: %#v %v", got, err)
	}
	assertCloneCommonDir(t, got.Path, caller)
	if ownSelections != 1 || foreignSelections != 1 || finals != 1 || locked != 0 {
		t.Fatalf("foreign walk snapshot bound: own=%d foreign=%d final=%d locked=%d", ownSelections, foreignSelections, finals, locked)
	}
	t.Logf("shared-cap fixture: own enumerations=%d foreign fresh enumerations=%d final scans=%d", ownSelections, foreignSelections, finals)
}

func TestAcquireForeignSelectionRefusesSnapshotRisks(t *testing.T) {
	for _, risk := range []string{"process", "process-gone", "snapshot-error"} {
		t.Run(risk, func(t *testing.T) {
			foreign, caller, pool := setupSharedClonePool(t)
			path, err := Acquire(foreign, pool, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := Release(pool, path); err != nil {
				t.Fatal(err)
			}
			var snapshot process.WorktreeSnapshot
			var snapshotErr error
			if risk != "snapshot-error" {
				tenant := exec.Command(os.Args[0], "-test.run=^TestAcquireTenantHelper$")
				tenant.Env = append(os.Environ(), "TREEHOUSE_TENANT_HELPER=1")
				tenant.Dir = path
				out, err := tenant.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				input, err := tenant.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := tenant.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { input.Close(); tenant.Process.Kill(); tenant.Wait() })
				ready, err := bufio.NewReader(out).ReadString('\n')
				if err != nil || ready != "TENANT_READY\n" {
					t.Fatalf("selection tenant readiness: %q %v", ready, err)
				}
				snapshot, err = process.SnapshotWorktreeProcesses()
				if err != nil {
					t.Fatal(err)
				}
				matches, err := snapshot.InWorktree(path)
				found := false
				for _, proc := range matches {
					found = found || proc.PID == int32(tenant.Process.Pid)
				}
				if err != nil || !found {
					t.Fatalf("selection snapshot did not capture the fixture tenant: %#v %v", matches, err)
				}
				if risk == "process-gone" {
					if err := tenant.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					_ = tenant.Wait()
				}
			} else {
				snapshotErr = fmt.Errorf("fixture process snapshot unavailable")
			}
			oldSnapshot, oldFinal, oldStage := snapshotWorktreeProcesses, findProcessesInWorktree, acquisitionStage
			ownSelections, foreignSelections, finals := 0, 0, 0
			inForeignWalk := false
			acquisitionStage = func(stage, path string) {
				if stage == "foreign-snapshot" {
					inForeignWalk = true
				}
				oldStage(stage, path)
			}
			snapshotWorktreeProcesses = func() (process.WorktreeSnapshot, error) {
				if !inForeignWalk {
					ownSelections++
					return oldSnapshot()
				}
				foreignSelections++
				return snapshot, snapshotErr
			}
			findProcessesInWorktree = func(path string) ([]process.ProcessInfo, error) {
				finals++
				return oldFinal(path)
			}
			t.Cleanup(func() {
				snapshotWorktreeProcesses = oldSnapshot
				findProcessesInWorktree = oldFinal
				acquisitionStage = oldStage
			})
			got, err := AcquireLeaseInfoWithOptions(caller, pool, 1, nil, "selection risk", AcquireOptions{SkipFetch: true})
			if risk == "process-gone" {
				if err != nil || got.Path == "" {
					t.Fatalf("a snapshot hit that left before the fresh re-check was refused: %#v %v", got, err)
				}
				assertCloneCommonDir(t, got.Path, caller)
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("reclaimed foreign checkout still present: %v", err)
				}
				if ownSelections != 1 || foreignSelections != 1 || finals != 2 {
					t.Fatalf("stale snapshot hit was not rechecked once before the removal guard: own=%d foreign=%d scans=%d", ownSelections, foreignSelections, finals)
				}
				return
			}
			wantFinals := 0
			if risk == "process" {
				wantFinals = 1
			}
			if err == nil || got.Path != "" {
				t.Fatalf("foreign selection risk was allocated: %#v %v", got, err)
			}
			if ownSelections != 1 || foreignSelections != 1 || finals != wantFinals {
				t.Fatalf("foreign selection refusal fell through: own=%d foreign=%d final scans=%d", ownSelections, foreignSelections, finals)
			}
			assertFileContents(t, filepath.Join(path, "README.md"), "hi\n")
			state, err := ReadState(pool)
			if err != nil || len(state.Worktrees) != 1 || state.Worktrees[0].Leased || state.Worktrees[0].OwnerPID != 0 {
				t.Fatalf("foreign selection refusal changed membership/reservation: %#v %v", state, err)
			}
			t.Logf("foreign selection %s refused; own=%d foreign=%d final scans=%d; original checkout retained", risk, ownSelections, foreignSelections, finals)
		})
	}
}

func TestAcquireFenceProtectsLastComparisonThroughReset(t *testing.T) {
	for _, stage := range []string{"claimed", "reset"} {
		t.Run(stage, func(t *testing.T) {
			repo, pool, paths := acquisitionFixture(t, 2)
			original := readFixtureFile(t, filepath.Join(paths[0], "README.md"))
			// The slot is a clean ancestor with recognizable content. Resetting it
			// after a successful takeover would visibly replace that content.
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("new reset target\n"), 0644); err != nil {
				t.Fatal(err)
			}
			runGit(t, repo, "add", "README.md")
			runGit(t, repo, "commit", "-m", "advance reset target")
			old := acquisitionStage
			takeoverChecked := false
			var replacement LeaseInfo
			acquisitionStage = func(point, path string) {
				if point != stage || path != paths[0] {
					return
				}
				assertPoolUnlocked(t, pool)
				before, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				if lease, err := LeaseExisting(pool, "1", "replacement"); err == nil {
					replacement = lease
					takeoverChecked = true
					return
				}
				if err := Release(pool, path); err == nil {
					t.Fatal("return changed a fenced acquisition")
				}
				guardRan := false
				if err := ValidateReleasePreconditions(pool, path, ReleasePreconditions{}, func() error { guardRan = true; return nil }); err == nil || guardRan {
					t.Fatal("release preparation ran on a fenced acquisition")
				}
				if _, err := PruneWithOptions(repo, pool, PruneOptions{}); err != nil {
					t.Fatal(err)
				}
				result, err := DestroyWorktree(pool, path, DestroyOptions{IncludeInUse: true, IncludeLeased: true, IncludeUnlanded: true})
				if err != nil || len(result.Destroyed) != 0 {
					t.Fatalf("destroy changed fenced slot: %#v %v", result, err)
				}
				after, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				if reservationOf(before.Worktrees[0]) != reservationOf(after.Worktrees[0]) {
					t.Fatal("replacement writer changed original claim")
				}
				data, err := os.ReadFile(filepath.Join(path, "README.md"))
				if err != nil || string(data) != original {
					t.Fatalf("checkout changed: %q %v", data, err)
				}
				takeoverChecked = true
			}
			t.Cleanup(func() { acquisitionStage = old })
			got, err := AcquireWithOptions(repo, pool, 2, nil, AcquireOptions{SkipFetch: true})
			if replacement.LeaseID != "" {
				state, readErr := ReadState(pool)
				data, fileErr := os.ReadFile(filepath.Join(paths[0], "README.md"))
				if readErr != nil || fileErr != nil || string(data) != original || state.Worktrees[0].LeaseID != replacement.LeaseID || state.Worktrees[0].LeaseHolder != "replacement" || got == paths[0] {
					t.Fatalf("get acted after replacement takeover: path=%q contents=%q replacement=%#v state=%#v read=%v file=%v acquire=%v", got, data, replacement, state, readErr, fileErr, err)
				}
				return
			}
			if err != nil || got != paths[0] || !takeoverChecked {
				t.Fatalf("unchanged claim failed: %q %v checked=%t", got, err, takeoverChecked)
			}
			lease, err := LeaseExisting(pool, "1", "replacement after publication")
			if err != nil || lease.LeaseID == "" {
				t.Fatalf("legitimate takeover after publication failed: %#v %v", lease, err)
			}
			state, err := ReadState(pool)
			if err != nil || state.Worktrees[0].LeaseID != lease.LeaseID || state.Worktrees[0].LeaseHolder != "replacement after publication" {
				t.Fatalf("replacement identity lost: %#v %v", state, err)
			}
		})
	}
}

func TestAcquireForeignRemovalFence(t *testing.T) {
	repo, pool, paths := acquisitionFixture(t, 2)
	original := readFixtureFile(t, filepath.Join(paths[0], "README.md"))
	caller := filepath.Join(t.TempDir(), "myrepo")
	runGit(t, repo, "clone", filepath.Join(filepath.Dir(repo), "remote.git"), caller)
	old := acquisitionStage
	var independent LeaseInfo
	visited := false
	acquisitionStage = func(stage, path string) {
		if stage != "foreign-validated" || path != paths[0] {
			return
		}
		assertPoolUnlocked(t, pool)
		before, err := ReadState(pool)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := LeaseExisting(pool, "1", "foreign replacement"); err == nil {
			t.Fatal("foreign removal allowed lease takeover")
		}
		if err := Release(pool, path); err == nil {
			t.Fatal("foreign removal allowed return")
		}
		guardRan := false
		if err := ValidateReleasePreconditions(pool, path, ReleasePreconditions{}, func() error { guardRan = true; return nil }); err == nil || guardRan {
			t.Fatal("foreign removal allowed release preparation")
		}
		result, err := DestroyWorktree(pool, path, DestroyOptions{IncludeInUse: true, IncludeLeased: true, IncludeUnlanded: true})
		if err != nil || len(result.Destroyed) != 0 {
			t.Fatalf("foreign removal allowed destroy: %#v %v", result, err)
		}
		after, err := ReadState(pool)
		if err != nil {
			t.Fatal(err)
		}
		if reservationOf(before.Worktrees[0]) != reservationOf(after.Worktrees[0]) {
			t.Fatal("foreign claim changed")
		}
		data, err := os.ReadFile(filepath.Join(path, "README.md"))
		if err != nil || string(data) != original {
			t.Fatalf("foreign checkout changed: %q %v", data, err)
		}
		independent, err = AcquireLeaseInfoWithOptions(caller, pool, 2, nil, "independent foreign capacity", AcquireOptions{SkipFetch: true})
		if err != nil {
			t.Fatal(err)
		}
		visited = true
	}
	t.Cleanup(func() { acquisitionStage = old })
	got, err := AcquireWithOptions(caller, pool, 2, nil, AcquireOptions{SkipFetch: true})
	if err != nil || got == "" || !visited || got == independent.Path {
		t.Fatalf("foreign progress: path=%q other=%#v visited=%t error=%v", got, independent, visited, err)
	}
	state, err := ReadState(pool)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range state.Worktrees {
		if entry.Path == independent.Path {
			found = entry.Leased && entry.LeaseID == independent.LeaseID
		}
	}
	if !found {
		t.Fatal("foreign finalization lost independent lease")
	}
}

func TestAcquireLateCLILeaseTakeover(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "treehouse")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	repo, pool, paths := acquisitionFixture(t, 2)
	original := readFixtureFile(t, filepath.Join(paths[0], "README.md"))
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("new reset target\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "advance reset target")
	var replacement LeaseInfo
	checked := false
	old := acquisitionStage
	acquisitionStage = func(stage, path string) {
		if stage != "reset" || path != paths[0] {
			return
		}
		assertPoolUnlocked(t, pool)
		cmd := exec.Command(binary, "lease", "--path", path, "--lease-holder", "CLI replacement", "--json")
		cmd.Dir = repo
		private := t.TempDir()
		cmd.Env = append(os.Environ(), "TREEHOUSE_ROOT="+private, "HOME="+private, "XDG_CONFIG_HOME="+private, "TREEHOUSE_VCS=git")
		out, err := cmd.Output()
		if err == nil {
			if err := json.Unmarshal(out, &replacement); err != nil {
				t.Fatal(err)
			}
		} else if failure, ok := err.(*exec.ExitError); !ok || !strings.Contains(string(failure.Stderr), "acquisition in progress") {
			t.Fatalf("CLI takeover failed for unrelated reason: %v", err)
		}
		checked = true
	}
	t.Cleanup(func() { acquisitionStage = old })
	got, err := AcquireWithOptions(repo, pool, 2, nil, AcquireOptions{SkipFetch: true})
	if !checked {
		t.Fatal("late CLI schedule unvisited")
	}
	if replacement.LeaseID != "" {
		state, readErr := ReadState(pool)
		data, fileErr := os.ReadFile(filepath.Join(paths[0], "README.md"))
		if readErr != nil || fileErr != nil || string(data) != original || state.Worktrees[0].LeaseID != replacement.LeaseID || state.Worktrees[0].LeaseHolder != "CLI replacement" || got == paths[0] {
			t.Fatalf("get acted on CLI replacement: contents=%q state=%#v path=%q acquire=%v", data, state, got, err)
		}
	} else if err != nil || got != paths[0] {
		t.Fatalf("unchanged claim failed: %q %v", got, err)
	}
}

func TestAcquireOwnerReplacementHelper(t *testing.T) {
	if os.Getenv("TREEHOUSE_OWNER_REPLACEMENT_HELPER") != "1" {
		return
	}
	path, err := AcquireWithOptions(os.Getenv("TREEHOUSE_FIXTURE_REPO"), os.Getenv("TREEHOUSE_FIXTURE_POOL"), 2, nil, AcquireOptions{SkipFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(path); err != nil {
		t.Fatal(err)
	}
	// Keep this different owner's PID/birth reservation alive until the parent
	// has checked the checkout after the interrupted acquisition resumes.
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
}

func TestAcquireReleaseThenOwnerReclaim(t *testing.T) {
	for _, writer := range []string{"Release", "ReleaseConditional"} {
		t.Run(writer, func(t *testing.T) {
			repo, pool, paths := acquisitionFixture(t, 2)
			originalReadme := readFixtureFile(t, filepath.Join(paths[0], "README.md"))
			// A wants a newer target, but an unconditional return and B's get
			// both park at main. B therefore retains A's observed HEAD, so Git's
			// HEAD-change check cannot hide an owner-reservation takeover bug.
			runGit(t, repo, "checkout", "-b", "a-target")
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("A reset target\n"), 0644); err != nil {
				t.Fatal(err)
			}
			runGit(t, repo, "add", "README.md")
			runGit(t, repo, "commit", "-m", "A target")
			runGit(t, repo, "checkout", "main")
			oldStage := acquisitionStage
			visited, released := false, false
			var replacement WorktreeEntry
			acquisitionStage = func(stage, path string) {
				if stage != "reset" || path != paths[0] || visited {
					return
				}
				visited = true
				assertPoolUnlocked(t, pool)
				before, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				original := reservationOf(before.Worktrees[0])
				if writer == "Release" {
					err = Release(pool, path)
				} else {
					err = ReleaseConditional(pool, path, "", ReleasePreconditions{}, nil)
				}
				released = err == nil
				afterRelease, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				if !released && reservationOf(afterRelease.Worktrees[0]) != original {
					t.Fatal("refused release changed A's claim")
				}
				child := exec.Command(os.Args[0], "-test.run=^TestAcquireOwnerReplacementHelper$")
				child.Dir = repo
				child.Env = append(os.Environ(), "TREEHOUSE_OWNER_REPLACEMENT_HELPER=1", "TREEHOUSE_FIXTURE_REPO="+repo, "TREEHOUSE_FIXTURE_POOL="+pool)
				out, err := child.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				input, err := child.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := child.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { input.Close(); child.Wait() })
				line, err := bufio.NewReader(out).ReadString('\n')
				var replacementPath string
				if err != nil || json.Unmarshal([]byte(line), &replacementPath) != nil {
					t.Fatalf("replacement owner readiness: %q %v", line, err)
				}
				expected := paths[1]
				if released {
					expected = paths[0]
				}
				if replacementPath != expected {
					t.Fatalf("replacement did not claim expected existing capacity: got=%s want=%s released=%t", replacementPath, expected, released)
				}
				state, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range state.Worktrees {
					if entry.Path == replacementPath {
						replacement = entry
					}
				}
				if replacement.Leased || replacement.LeaseID != "" || replacement.OwnerPID != int32(child.Process.Pid) || replacement.OwnerStartedAt == 0 || replacement.OwnerPID == original.OwnerPID {
					t.Fatalf("replacement is not a distinct live default owner: %#v original=%#v", replacement, original)
				}
				if got := readFixtureFile(t, filepath.Join(replacementPath, "README.md")); got != originalReadme {
					t.Fatalf("replacement checkout = %q, want %q", got, originalReadme)
				}
				t.Logf("%s released=%t; replacement owner=%d/%d path=%s; lease fields remain false/empty", writer, released, replacement.OwnerPID, replacement.OwnerStartedAt, replacement.Path)
			}
			t.Cleanup(func() { acquisitionStage = oldStage })
			got, acquireErr := AcquireWithOptions(repo, pool, 2, nil, AcquireOptions{SkipFetch: true, BaseBranch: "a-target"})
			if !visited || replacement.Path == "" {
				t.Fatal("release/reclaim schedule was not visited")
			}
			state, err := ReadState(pool)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range state.Worktrees {
				if entry.Path == replacement.Path {
					found = reservationOf(entry) == reservationOf(replacement)
				}
			}
			data, fileErr := os.ReadFile(filepath.Join(replacement.Path, "README.md"))
			if !found || fileErr != nil || string(data) != originalReadme || got == replacement.Path {
				t.Fatalf("A acted on replacement owner: contents=%q reservationRetained=%t replacement=%#v Apath=%s Aerror=%v read=%v", data, found, replacement, got, acquireErr, fileErr)
			}
			if !released && (acquireErr != nil || got != paths[0]) {
				t.Fatalf("unchanged fenced acquisition failed: %s %v", got, acquireErr)
			}
		})
	}
}

func TestAcquirePostClaimChangesArePreserved(t *testing.T) {
	for _, change := range []string{"dirty", "live", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			repo, pool, paths := acquisitionFixture(t, 2)
			oldStage := acquisitionStage
			acquisitionStage = func(stage, path string) {
				if stage != "claimed" || path != paths[0] {
					return
				}
				if change == "dirty" {
					if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("late work\n"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if change == "live" {
					tenant := exec.Command(os.Args[0], "-test.run=^TestAcquireTenantHelper$")
					tenant.Dir = path
					tenant.Env = append(os.Environ(), "TREEHOUSE_TENANT_HELPER=1")
					out, err := tenant.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					input, err := tenant.StdinPipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := tenant.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { input.Close(); tenant.Process.Kill(); tenant.Wait() })
					ready, err := bufio.NewReader(out).ReadString('\n')
					if err != nil || ready != "TENANT_READY\n" {
						t.Fatalf("tenant readiness: %q %v", ready, err)
					}
				}
			}
			t.Cleanup(func() { acquisitionStage = oldStage })
			got, err := AcquireLeaseInfoWithOptions(repo, pool, 2, nil, "post-claim", AcquireOptions{SkipFetch: true})
			want := paths[1]
			if change == "unchanged" {
				want = paths[0]
			}
			if err != nil || got.Path != want {
				t.Fatalf("selected %q want %q: %v", got.Path, want, err)
			}
			if change == "dirty" {
				data, err := os.ReadFile(filepath.Join(paths[0], "README.md"))
				if err != nil || string(data) != "late work\n" {
					t.Fatalf("late edits lost: %q %v", data, err)
				}
			}
			state, err := ReadState(pool)
			if err != nil {
				t.Fatal(err)
			}
			if change != "unchanged" && (state.Worktrees[0].Leased || state.Worktrees[0].OwnerPID != 0) {
				t.Fatal("rejected candidate leaked reservation")
			}
		})
	}
}

func TestAcquireHookLeaseHelper(t *testing.T) {
	if os.Getenv("TREEHOUSE_HOOK_LEASE_HELPER") != "1" {
		return
	}
	lease, err := LeaseExisting(os.Getenv("TREEHOUSE_HOOK_POOL"), "1", "post-create takeover")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("TREEHOUSE_HOOK_RESULT"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAcquirePostCreateHookMayLeasePublishedSlot(t *testing.T) {
	for _, existing := range []int{0, 1} {
		t.Run(fmt.Sprint("existing=", existing), func(t *testing.T) {
			repo, pool, _ := acquisitionFixture(t, existing)
			result := filepath.Join(t.TempDir(), "hook-lease.json")
			t.Setenv("TREEHOUSE_HOOK_LEASE_HELPER", "1")
			t.Setenv("TREEHOUSE_HOOK_POOL", pool)
			t.Setenv("TREEHOUSE_HOOK_RESULT", result)
			command := fmt.Sprintf("\"%s\" \"-test.run=^TestAcquireHookLeaseHelper$\"", os.Args[0])
			got, err := AcquireWithOptions(repo, pool, 1, []string{command}, AcquireOptions{SkipFetch: true})
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(result)
			if err != nil {
				t.Fatalf("post-create hook could not lease published slot: %v", err)
			}
			var lease LeaseInfo
			if err := json.Unmarshal(data, &lease); err != nil {
				t.Fatal(err)
			}
			state, err := ReadState(pool)
			if err != nil {
				t.Fatal(err)
			}
			if got != lease.Path || lease.LeaseID == "" || len(state.Worktrees) != 1 || state.Worktrees[0].LeaseID != lease.LeaseID || state.Worktrees[0].LeaseHolder != "post-create takeover" {
				t.Fatalf("published hook takeover lost: path=%q lease=%#v state=%#v", got, lease, state)
			}
		})
	}
}

func TestAcquireTenantHelper(t *testing.T) {
	if os.Getenv("TREEHOUSE_TENANT_HELPER") != "1" {
		return
	}
	fmt.Println("TENANT_READY")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestAcquireCrashHelper(t *testing.T) {
	if os.Getenv("TREEHOUSE_CRASH_HELPER") != "1" {
		return
	}
	acquisitionStage = func(stage, path string) {
		if stage == os.Getenv("TREEHOUSE_CRASH_STAGE") {
			fmt.Println("HELD")
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}
	opts := AcquireOptions{SkipFetch: true}
	var err error
	if os.Getenv("TREEHOUSE_CRASH_LEASE") == "1" {
		_, err = AcquireLeaseInfoWithOptions(os.Getenv("TREEHOUSE_CRASH_REPO"), os.Getenv("TREEHOUSE_CRASH_POOL"), 2, nil, "crash lease", opts)
	} else {
		_, err = AcquireWithOptions(os.Getenv("TREEHOUSE_CRASH_REPO"), os.Getenv("TREEHOUSE_CRASH_POOL"), 2, nil, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRefusedScreeningCrashReservations(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%t/lease=true", foreign), func(t *testing.T) {
			repo, pool, paths := acquisitionFixture(t, 2)
			caller := repo
			if foreign {
				caller = filepath.Join(t.TempDir(), "myrepo")
				runGit(t, repo, "clone", filepath.Join(filepath.Dir(repo), "remote.git"), caller)
			}
			const contents = "recognizable refused checkout\n"
			original := readFixtureFile(t, filepath.Join(paths[0], "README.md"))
			if err := os.WriteFile(filepath.Join(paths[0], "README.md"), []byte(contents), 0644); err != nil {
				t.Fatal(err)
			}
			spare, err := LeaseExisting(pool, "2", "keep the spare reserved")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireCrashHelper$")
			cmd.Env = append(os.Environ(), "TREEHOUSE_CRASH_HELPER=1", "TREEHOUSE_CRASH_STAGE=screening", "TREEHOUSE_CRASH_LEASE=1", "TREEHOUSE_CRASH_REPO="+caller, "TREEHOUSE_CRASH_POOL="+pool)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { input.Close(); cmd.Process.Kill(); cmd.Wait() })
			ready, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || ready != "HELD\n" {
				t.Fatalf("screening readiness: %q %v", ready, err)
			}
			before, err := ReadState(pool)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("before kill foreign=%t leased=%t holder=%q owner=%d", foreign, before.Worktrees[0].Leased, before.Worktrees[0].LeaseHolder, before.Worktrees[0].OwnerPID)
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if _, err := List(pool); err != nil {
				t.Fatal(err)
			}
			after, err := ReadState(pool)
			if err != nil || len(after.Worktrees) != 2 {
				t.Fatalf("screening recovery membership: %#v %v", after, err)
			}
			refused := after.Worktrees[0]
			if refused.Leased || refused.LeaseID != "" || refused.OwnerPID != 0 {
				t.Fatalf("killed screening get stranded a reservation on a refused copy: %#v", refused)
			}
			assertFileContents(t, filepath.Join(paths[0], "README.md"), contents)
			got, err := AcquireLeaseInfoWithOptions(caller, pool, 2, nil, "after refused crash", AcquireOptions{SkipFetch: true})
			if err == nil || got.Path != "" {
				t.Fatalf("dirty copy was allocated after screening crash: %#v %v", got, err)
			}
			assertFileContents(t, filepath.Join(paths[0], "README.md"), contents)
			// No named return is needed: repairing the actual risk must make the
			// same-clone copy reusable or the foreign one reclaimable again.
			if err := os.WriteFile(filepath.Join(paths[0], "README.md"), []byte(original), 0644); err != nil {
				t.Fatal(err)
			}
			got, err = AcquireLeaseInfoWithOptions(caller, pool, 2, nil, "after repair", AcquireOptions{SkipFetch: true})
			if err != nil || got.Path == "" || !foreign && got.Path != paths[0] {
				t.Fatalf("screening crash permanently stranded capacity: %#v %v", got, err)
			}
			assertCloneCommonDir(t, got.Path, caller)
			state, err := ReadState(pool)
			if err != nil {
				t.Fatal(err)
			}
			keptSpare := false
			for _, entry := range state.Worktrees {
				if entry.Name == "2" && entry.Leased && entry.LeaseID == spare.LeaseID {
					keptSpare = true
				}
			}
			if !keptSpare {
				t.Fatal("screening recovery overwrote the independent spare lease")
			}
			t.Logf("foreign=%t: List cleared the dead screening owner, checkout preserved, unsafe allocation refused, legitimate completion resumed without return", foreign)
		})
	}
}

func TestAcquireFreshCrashReservations(t *testing.T) {
	for _, stage := range []string{"fresh-claimed", "fresh-created"} {
		for _, leased := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lease=%t", stage, leased), func(t *testing.T) {
				repo, pool, _ := acquisitionFixture(t, 0)
				original := checkoutFixtureFile(t, repo, "main:README.md")
				mode := "0"
				if leased {
					mode = "1"
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireCrashHelper$")
				cmd.Env = append(os.Environ(), "TREEHOUSE_CRASH_HELPER=1", "TREEHOUSE_CRASH_STAGE="+stage, "TREEHOUSE_CRASH_LEASE="+mode, "TREEHOUSE_CRASH_REPO="+repo, "TREEHOUSE_CRASH_POOL="+pool)
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { input.Close(); cmd.Process.Kill(); cmd.Wait() })
				ready, err := bufio.NewReader(out).ReadString('\n')
				if err != nil || ready != "HELD\n" {
					t.Fatalf("fresh readiness: %q %v", ready, err)
				}
				state, err := ReadState(pool)
				if err != nil || len(state.Worktrees) != 1 {
					t.Fatalf("fresh claim missing: %#v %v", state, err)
				}
				claim := state.Worktrees[0]
				if !claim.Leased || claim.OwnerPID != 0 || claim.SeedInventoryKnown {
					t.Fatalf("fresh claim lost provisional quarantine: %#v", claim)
				}
				t.Logf("persisted fresh stage=%s requestedLease=%t holder=%q path=%s", stage, leased, claim.LeaseHolder, claim.Path)
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				if _, err := List(pool); err != nil {
					t.Fatal(err)
				}
				state, err = ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				if stage == "fresh-claimed" {
					if len(state.Worktrees) != 0 {
						t.Fatal("missing fresh checkout stranded capacity")
					}
				} else {
					if len(state.Worktrees) != 1 || !state.Worktrees[0].Leased {
						t.Fatal("created fresh checkout lost quarantine")
					}
					data, err := os.ReadFile(filepath.Join(claim.Path, "README.md"))
					if err != nil || string(data) != original {
						t.Fatalf("created checkout lost contents: %q %v", data, err)
					}
				}
				got, err := AcquireLeaseInfoWithOptions(repo, pool, 2, nil, "after fresh crash", AcquireOptions{SkipFetch: true})
				if err != nil || got.Path == "" || stage == "fresh-created" && got.Path == claim.Path {
					t.Fatalf("fresh crash allocation: %#v %v", got, err)
				}
				if stage == "fresh-created" {
					if err := Release(pool, claim.Path); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestAcquireCrashReservations(t *testing.T) {
	for _, stage := range []string{"claimed", "verified", "seeding", "published"} {
		for _, leased := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/lease=%t", stage, leased), func(t *testing.T) {
				repo, pool, paths := acquisitionFixture(t, 2)
				cmd := exec.Command(os.Args[0], "-test.run=^TestAcquireCrashHelper$")
				mode := "0"
				if leased {
					mode = "1"
				}
				cmd.Env = append(os.Environ(), "TREEHOUSE_CRASH_HELPER=1", "TREEHOUSE_CRASH_STAGE="+stage, "TREEHOUSE_CRASH_LEASE="+mode, "TREEHOUSE_CRASH_REPO="+repo, "TREEHOUSE_CRASH_POOL="+pool)
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer input.Close()
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
				line, err := bufio.NewReader(out).ReadString('\n')
				if err != nil || strings.TrimSpace(line) != "HELD" {
					t.Fatalf("helper stage %q %v", line, err)
				}
				state, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				claim := state.Worktrees[0]
				// Requested leases become durable only after screening/reset;
				// before that, either mode heals a dead screening owner.
				durable := stage == "seeding" || stage == "published" && leased
				t.Logf("persisted stage=%s requestedLease=%t leased=%t holder=%q leaseID=%q owner=%d", stage, leased, claim.Leased, claim.LeaseHolder, claim.LeaseID, claim.OwnerPID)
				if durable {
					if !claim.Leased || (stage != "seeding" && claim.LeaseID == "") || claim.OwnerPID != 0 {
						t.Fatalf("not durable: %#v", claim)
					}
				} else if claim.OwnerPID == 0 || claim.Leased {
					t.Fatalf("not owner reservation: %#v", claim)
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				// Existing List heals dead owners but never an ordinary durable lease.
				if _, err := List(pool); err != nil {
					t.Fatal(err)
				}
				state, err = ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				if durable {
					if state.Worktrees[0].LeaseID != claim.LeaseID || !state.Worktrees[0].Leased {
						t.Fatal("crash lost durable claim")
					}
				} else if state.Worktrees[0].OwnerPID != 0 {
					t.Fatal("dead owner not healed")
				}
				got, err := AcquireLeaseInfoWithOptions(repo, pool, 2, nil, "after crash", AcquireOptions{SkipFetch: true})
				want := paths[0]
				if durable {
					want = paths[1]
				}
				if err != nil || got.Path != want {
					t.Fatalf("recovery selected %q want %q: %v", got.Path, want, err)
				}
				if durable {
					if err := Release(pool, paths[0]); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
