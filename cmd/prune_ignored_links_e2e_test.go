package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/pool"
)

// ignoredFixture is a scratch pool with a manifest-seeded repository. Every
// command runs the real treehouse binary with an explicit scratch --root.
type ignoredFixture struct {
	t    *testing.T
	repo string
	home string
	root string
	env  []string
}

func newIgnoredFixture(t *testing.T, files map[string]string, manifest string) *ignoredFixture {
	t.Helper()
	repo, home := setupTestRepo(t)
	root := filepath.Join(home, "scratch-pools")
	f := &ignoredFixture{t: t, repo: repo, home: home, root: root, env: []string{"TREEHOUSE_ROOT=" + root, "TREEHOUSE_VCS=git"}}
	f.write(".gitignore", "*.ignored\n")
	f.write(".worktreeinclude", manifest)
	for name, content := range files {
		f.write(name, content)
	}
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", "ignored links fixture")
	gitCmd(t, repo, "push", "origin", "main")
	return f
}

func (f *ignoredFixture) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ignoredFixture) run(extraEnv []string, args ...string) string {
	f.t.Helper()
	args = append([]string{"--root", f.root}, args...)
	out, stderr, code := runTreehouse(f.t, f.repo, f.home, append(append([]string(nil), f.env...), extraEnv...), args...)
	f.t.Logf("treehouse %v: exit=%d\n%s%s", args, code, out, stderr)
	if code != 0 {
		f.t.Fatalf("command failed: %s", stderr)
	}
	return out
}

// lease acquires a real seeded slot and returns its path and pool directory.
func (f *ignoredFixture) lease() (slot, poolDir string) {
	f.t.Helper()
	slot = strings.TrimSpace(f.run(nil, "get", "--lease"))
	return slot, filepath.Dir(filepath.Dir(slot))
}

// idleAll is the contract's test-only transition: it clears only lease and
// reservation fields and keeps the authenticated seed inventory and bytes.
func idleAll(t *testing.T, poolDir string) {
	t.Helper()
	state, err := pool.ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range state.Worktrees {
		entry := &state.Worktrees[i]
		if !entry.SeedInventoryKnown || len(entry.SeededPaths) == 0 {
			t.Fatalf("fixture slot was not authentically seeded: %#v", *entry)
		}
		entry.Leased, entry.Destroying = false, false
		entry.LeaseID, entry.LeaseHolder = "", ""
		entry.LeasedAt = time.Time{}
		entry.OwnerPID, entry.OwnerStartedAt = 0, 0
	}
	if err := pool.WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
}

func registrations(t *testing.T, poolDir string) int {
	t.Helper()
	state, err := pool.ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(state.Worktrees)
}

// makeDirLink makes a directory alias: a symbolic link, or on Windows a
// junction (which needs no privilege). It never skips: a platform that can make
// neither fails the test.
func makeDirLink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// cmd.exe splits mklink arguments on '=', ',' and ';', which subtest
		// temp paths contain; PowerShell reads both paths from the environment.
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"New-Item -ItemType Junction -Path $env:TREEHOUSE_LINK -Value $env:TREEHOUSE_TARGET | Out-Null")
		cmd.Env = append(os.Environ(), "TREEHOUSE_LINK="+link, "TREEHOUSE_TARGET="+target)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("junction %s -> %s: %v\n%s", link, target, err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// F1 (X6/X11 rows 9 and 16): the source's PARENT directory is a directory link,
// so source and slot file resolve to one file inside the slot (row 9), or the
// source resolves into the pool directory outside the slot (row 16). Both must
// refuse and keep the file. The unchanged nested control must still be pruned,
// so an always-refuse repair fails here too.
func TestPruneIgnoredSourceParentDirectoryLink(t *testing.T) {
	for _, all := range []bool{false, true} {
		for _, scenario := range []string{"nested-unchanged", "ancestor-link-into-slot", "ancestor-link-into-pool"} {
			t.Run(fmt.Sprintf("all=%t/%s", all, scenario), func(t *testing.T) {
				f := newIgnoredFixture(t, map[string]string{"sub/x.ignored": "original\n"}, "sub/x.ignored\n")
				slot, poolDir := f.lease()
				idleAll(t, poolDir)
				file := filepath.Join(slot, "sub", "x.ignored")
				if got, err := os.ReadFile(file); err != nil || string(got) != "original\n" {
					t.Fatalf("real get did not seed the nested file: %q (%v)", got, err)
				}
				sourceDir := filepath.Join(f.repo, "sub")
				switch scenario {
				case "ancestor-link-into-slot", "ancestor-link-into-pool":
					target := filepath.Join(slot, "sub")
					if scenario == "ancestor-link-into-pool" {
						// A directory inside the pool, outside the slot, with equal bytes.
						target = filepath.Join(poolDir, "other-slot-stand-in", "sub")
						if err := os.MkdirAll(target, 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(target, "x.ignored"), []byte("original\n"), 0o640); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.RemoveAll(sourceDir); err != nil {
						t.Fatal(err)
					}
					makeDirLink(t, target, sourceDir)
					// The alias really reaches the file it must not be trusted for.
					if got, err := os.ReadFile(filepath.Join(sourceDir, "x.ignored")); err != nil || string(got) != "original\n" {
						t.Fatalf("directory alias not established: %q (%v)", got, err)
					}
				}
				if status := gitCmd(t, slot, "status", "--porcelain", "--untracked-files=all"); status != "" {
					t.Fatalf("fixture is not ignored-only: %s", status)
				}
				gitCmd(t, slot, "check-ignore", "--", "sub/x.ignored")
				before := pruneFileInventory(t, slot)
				args := []string{"prune"}
				if all {
					args = append(args, "--all")
				}
				dry := f.run(nil, args...)
				out := f.run(nil, append(args, "--yes")...)
				if scenario == "nested-unchanged" {
					if _, err := os.Lstat(slot); !os.IsNotExist(err) {
						t.Errorf("regenerable nested slot not removed: %v", err)
					}
					if !strings.Contains(out, "Pruned 1 stale worktree") || registrations(t, poolDir) != 0 {
						t.Errorf("missing successful removal: %s", out)
					}
					return
				}
				// Outcome, not prose: the slot is reported skipped for its ignored
				// path, nothing is pruned, and the payload check below proves no loss.
				for label, text := range map[string]string{"dry run": dry, "execute": out} {
					if !strings.Contains(text, pool.PruneSkipIgnoredFiles) || !strings.Contains(text, "x.ignored") || strings.Contains(text, "Pruned 1 stale worktree") {
						t.Errorf("%s did not report a truthful skip for the ignored path: %s", label, text)
					}
				}
				if after := pruneFileInventory(t, slot); !reflect.DeepEqual(before, after) {
					t.Errorf("prune lost or changed payload: before=%v after=%v", before, after)
				}
				if got, err := os.ReadFile(file); err != nil || string(got) != "original\n" {
					t.Errorf("file did not survive: %q (%v)", got, err)
				}
				if registrations(t, poolDir) != 1 {
					t.Errorf("refused slot lost its registration")
				}
			})
		}
	}
}

// Rows 17 and 18: fresh real seeding writes a REGULAR file holding a symbolic
// source's link-target text, never a link, so a slot entry that is a symbolic
// link is not what seeding writes now, whatever its target. Both the equal-
// target (row 17) and different-target (row 18) fixtures are refused and the
// link survives. A candidate that takes the kind of the written entry for its
// content, or compares only link targets, would prune the equal-target one.
func TestPruneIgnoredSlotEntryIsSymbolicLink(t *testing.T) {
	for _, scenario := range []string{"same-target", "other-target"} {
		t.Run(scenario, func(t *testing.T) {
			f := newIgnoredFixture(t, map[string]string{"seed.ignored": "original\n", "other.ignored": "unchanged\n"}, "seed.ignored\nother.ignored\n")
			if err := os.Remove(filepath.Join(f.repo, "seed.ignored")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("other.ignored", filepath.Join(f.repo, "seed.ignored")); err != nil {
				t.Fatal(err)
			}
			slot, poolDir := f.lease()
			idleAll(t, poolDir)
			file := filepath.Join(slot, "seed.ignored")
			info, err := os.Lstat(file)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("fresh real seeding did not write a regular file for a link source: %v (%v)", info, err)
			}
			if got, _ := os.ReadFile(file); string(got) != "other.ignored" {
				t.Fatalf("seeding wrote %q, expected the link-target text", got)
			}
			target := "other.ignored"
			if scenario == "other-target" {
				target = "elsewhere.ignored"
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, file); err != nil {
				t.Fatal(err)
			}
			before := pruneFileInventory(t, slot)
			dry := f.run(nil, "prune")
			out := f.run(nil, "prune", "--yes")
			if !strings.Contains(dry, pool.PruneSkipIgnoredFiles) || !strings.Contains(out, pool.PruneSkipIgnoredFiles) || strings.Contains(out, "Pruned 1 stale worktree") {
				t.Errorf("missing truthful skip for the symbolic entry: dry=%s execute=%s", dry, out)
			}
			if after := pruneFileInventory(t, slot); !reflect.DeepEqual(before, after) {
				t.Errorf("prune lost or changed payload: before=%v after=%v", before, after)
			}
			if got, err := os.Readlink(file); err != nil || got != target {
				t.Errorf("slot link did not survive: %q (%v)", got, err)
			}
			if registrations(t, poolDir) != 1 {
				t.Errorf("refused slot lost its registration")
			}
		})
	}
}

// Row 8, second fixture: each source is a link to the OTHER seeded slot file and
// both slot files hold equal bytes. The reference for what seeding writes now is
// taken from a fresh real `get` in a second scratch pool whose copy of the
// fixture has no slot for the links to reach. The slot is pruned only if its
// entry equals that reference entry in kind and content.
func TestPruneIgnoredCrossLinkedSources(t *testing.T) {
	f := newIgnoredFixture(t, map[string]string{"a.ignored": "same\n", "b.ignored": "same\n"}, "a.ignored\nb.ignored\n")
	slot, poolDir := f.lease()
	idleAll(t, poolDir)
	crossLink := func(repo, aTarget, bTarget string) {
		t.Helper()
		for name, target := range map[string]string{"a.ignored": aTarget, "b.ignored": bTarget} {
			path := filepath.Join(repo, name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	crossLink(f.repo, filepath.Join(slot, "b.ignored"), filepath.Join(slot, "a.ignored"))

	// Reference: the same manifest and the IDENTICAL link text, with the first
	// slot absent. The slot is moved aside while a second scratch pool runs the
	// real get, so the links dangle exactly as in a copy without the first slot.
	ref := newIgnoredFixture(t, map[string]string{"a.ignored": "same\n", "b.ignored": "same\n"}, "a.ignored\nb.ignored\n")
	crossLink(ref.repo, filepath.Join(slot, "b.ignored"), filepath.Join(slot, "a.ignored"))
	away := slot + ".away"
	if err := os.Rename(slot, away); err != nil {
		t.Fatal(err)
	}
	refSlot, _ := ref.lease()
	if err := os.Rename(away, slot); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(ref.repo, "a.ignored")); got != filepath.Join(slot, "b.ignored") {
		t.Fatalf("reference source link text differs from the fixture's: %q", got)
	}
	type entry struct {
		kind    os.FileMode
		content string
		exists  bool
	}
	read := func(path string) entry {
		info, err := os.Lstat(path)
		if err != nil {
			return entry{}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(path)
			return entry{kind: os.ModeSymlink, content: target, exists: true}
		}
		data, _ := os.ReadFile(path)
		return entry{kind: info.Mode().Type(), content: string(data), exists: true}
	}
	wantPrune := true
	for _, name := range []string{"a.ignored", "b.ignored"} {
		reference, current := read(filepath.Join(refSlot, name)), read(filepath.Join(slot, name))
		t.Logf("%s: reference seeding wrote %+v; slot entry is %+v", name, reference, current)
		if !reference.exists || reference != current {
			wantPrune = false
		}
	}

	dry := f.run(nil, "prune")
	out := f.run(nil, "prune", "--yes")
	if wantPrune {
		if _, err := os.Lstat(slot); !os.IsNotExist(err) || !strings.Contains(out, "Pruned 1 stale worktree") {
			t.Errorf("slot equal to the reference entry was not pruned: dry=%s execute=%s", dry, out)
		}
		return
	}
	for _, name := range []string{"a.ignored", "b.ignored"} {
		if got, err := os.ReadFile(filepath.Join(slot, name)); err != nil || !bytes.Equal(got, []byte("same\n")) {
			t.Errorf("%s did not survive: %q (%v)", name, got, err)
		}
	}
	if registrations(t, poolDir) != 1 {
		t.Errorf("refused slot lost its registration: dry=%s execute=%s", dry, out)
	}
}

// X10 Runs A, C and D in one pool: a seeded-then-modified slot beside an
// unchanged seeded control. The real `prune --yes` refuses the modified slot at
// the plan step and removes the control through the real native `git worktree
// remove` and removeWorktreeDirectory. Observation seams: GIT_TRACE records
// every real git invocation (the git process itself writes the record, so the
// real operation always runs); removeWorktreeDirectory has no call-level seam
// and is observed by its effect, the pool-owned slot parent directory that git
// worktree remove leaves behind being gone. Run D restores the modified bytes
// and a second prune removes the slot with no stale reservation.
func TestPruneIgnoredModifiedBesideControl(t *testing.T) {
	f := newIgnoredFixture(t, map[string]string{"seed.ignored": "original\n"}, "seed.ignored\n")
	modified, poolDir := f.lease()
	control, _ := f.lease()
	if modified == control {
		t.Fatal("fixture needs two slots in one pool")
	}
	idleAll(t, poolDir)
	file := filepath.Join(modified, "seed.ignored")
	if err := os.WriteFile(file, []byte("modified\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	beforeModified := pruneFileInventory(t, modified)
	controlParent := filepath.Dir(control)
	trace := filepath.Join(f.home, "git-trace.log")
	traceEnv := []string{"GIT_TRACE=" + trace}

	dry := f.run(traceEnv, "prune")
	if strings.Contains(readFileOrEmpty(trace), "worktree remove") {
		t.Fatal("a dry run reached native removal")
	}
	out := f.run(traceEnv, "prune", "--yes")
	if !strings.Contains(out, "Pruned 1 stale worktree") || !strings.Contains(out, "modified seeded ignored path") {
		t.Fatalf("expected one pruned control and one refused modified slot: dry=%s execute=%s", dry, out)
	}
	if after := pruneFileInventory(t, modified); !reflect.DeepEqual(beforeModified, after) {
		t.Errorf("modified slot lost or changed payload: before=%v after=%v", beforeModified, after)
	}
	if registrations(t, poolDir) != 1 {
		t.Errorf("expected exactly the modified slot to stay registered")
	}
	// Run C: the control went through the real native removal and directory removal.
	removals := 0
	for _, line := range strings.Split(readFileOrEmpty(trace), "\n") {
		if strings.Contains(line, "worktree remove") && strings.Contains(line, control) {
			removals++
		}
		if strings.Contains(line, "worktree remove") && strings.Contains(line, modified) {
			t.Errorf("native removal reached the refused slot: %s", line)
		}
	}
	if removals == 0 {
		t.Errorf("trace shows no native `git worktree remove` of the control slot:\n%s", readFileOrEmpty(trace))
	}
	if _, err := os.Lstat(control); !os.IsNotExist(err) {
		t.Errorf("control slot directory remains: %v", err)
	}
	if _, err := os.Lstat(controlParent); !os.IsNotExist(err) {
		t.Errorf("removeWorktreeDirectory left the pool-owned slot parent: %v", err)
	}
	// Run D: resolve the protected condition; the second prune removes it.
	if err := os.WriteFile(file, []byte("original\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	retry := f.run(nil, "prune", "--yes")
	if _, err := os.Lstat(modified); !os.IsNotExist(err) || !strings.Contains(retry, "Pruned 1 stale worktree") {
		t.Errorf("restored slot was not removed: %s (%v)", retry, err)
	}
	state, err := pool.ReadState(poolDir)
	if err != nil || len(state.Worktrees) != 0 {
		t.Errorf("stale registration or reservation after second prune: %#v (%v)", state, err)
	}
}

func readFileOrEmpty(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// N3 control: a source that is a symbolic link with an ABSOLUTE target outside
// the pool. Seeding writes the link-target text as a regular file; an unchanged
// slot file equal to that text is regenerable and must be pruned. A repair that
// refuses every absolute source link would leave the slot behind and fail here.
func TestPruneIgnoredEqualAbsoluteSourceLink(t *testing.T) {
	f := newIgnoredFixture(t, map[string]string{"seed.ignored": "original\n"}, "seed.ignored\n")
	external := filepath.Join(f.home, "external-target.txt")
	if err := os.WriteFile(external, []byte("referent bytes\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.repo, "seed.ignored")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(f.repo, "seed.ignored")); err != nil {
		t.Fatal(err)
	}
	slot, poolDir := f.lease()
	idleAll(t, poolDir)
	got, err := os.ReadFile(filepath.Join(slot, "seed.ignored"))
	if err != nil || string(got) != external {
		t.Fatalf("seeding did not write the absolute link-target text: %q (%v)", got, err)
	}
	f.run(nil, "prune")
	out := f.run(nil, "prune", "--yes")
	if _, err := os.Lstat(slot); !os.IsNotExist(err) || !strings.Contains(out, "Pruned 1 stale worktree") || registrations(t, poolDir) != 0 {
		t.Errorf("equal absolute link text was not treated as regenerable: %s (%v)", out, err)
	}
}

// N1 / X5: `prune --all` must name EVERY pool it discovered, including a pool
// holding only a leased slot (nothing to prune, nothing skipped) beside a pool
// with a protected ignored file, in both the dry run and the --yes output.
func TestPruneAllNamesEveryDiscoveredPool(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "scratch-pools")
	env := []string{"TREEHOUSE_ROOT=" + root, "TREEHOUSE_VCS=git"}
	run := func(dir string, args ...string) string {
		t.Helper()
		out, stderr, code := runTreehouse(t, dir, home, env, append([]string{"--root", root}, args...)...)
		t.Logf("treehouse %v: exit=%d\n%s%s", args, code, out, stderr)
		if code != 0 {
			t.Fatalf("command failed: %s", stderr)
		}
		return out
	}
	leasedRepo := setupTestRepoWithHome(t, home, "leased-repo")
	ignoredRepo := setupTestRepoWithHome(t, home, "ignored-repo")
	if err := os.WriteFile(filepath.Join(ignoredRepo, ".gitignore"), []byte("*.ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, ignoredRepo, "add", ".")
	gitCmd(t, ignoredRepo, "commit", "-m", "ignore")
	gitCmd(t, ignoredRepo, "push", "origin", "main")

	leasedSlot := strings.TrimSpace(run(leasedRepo, "get", "--lease"))
	ignoredSlot := strings.TrimSpace(run(ignoredRepo, "get", "--lease"))
	leasedPool, ignoredPool := filepath.Dir(filepath.Dir(leasedSlot)), filepath.Dir(filepath.Dir(ignoredSlot))
	if leasedPool == ignoredPool {
		t.Fatal("fixture needs two pools")
	}
	// Idle only the ignored pool's slot; the other pool keeps its durable lease.
	state, err := pool.ReadState(ignoredPool)
	if err != nil || len(state.Worktrees) != 1 {
		t.Fatalf("ignored pool state: %#v (%v)", state, err)
	}
	entry := &state.Worktrees[0]
	entry.Leased, entry.Destroying = false, false
	entry.LeaseID, entry.LeaseHolder = "", ""
	entry.LeasedAt = time.Time{}
	entry.OwnerPID, entry.OwnerStartedAt = 0, 0
	if err := pool.WriteState(ignoredPool, state); err != nil {
		t.Fatal(err)
	}
	unique := filepath.Join(ignoredSlot, "unique.ignored")
	if err := os.WriteFile(unique, []byte("unique payload\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before := pruneFileInventory(t, ignoredSlot)
	for _, args := range [][]string{{"prune", "--all"}, {"prune", "--all", "--yes"}} {
		out := run(ignoredRepo, args...)
		for _, p := range []string{leasedPool, ignoredPool} {
			// The child printed paths relative to ITS home (the fixture's), not
			// the parent test process's: accept that spelling or the absolute one.
			if !strings.Contains(out, childDisplayPath(home, p)) && !strings.Contains(out, p) {
				t.Errorf("%v does not name pool %s:\n%s", args, p, out)
			}
		}
		if !strings.Contains(out, "across 2 pools") {
			t.Errorf("%v does not report 2 pools: %s", args, out)
		}
	}
	if after := pruneFileInventory(t, ignoredSlot); !reflect.DeepEqual(before, after) {
		t.Errorf("protected file changed: before=%v after=%v", before, after)
	}
	if registrations(t, leasedPool) != 1 || registrations(t, ignoredPool) != 1 {
		t.Errorf("a registration was lost")
	}
}

// childDisplayPath formats a path the way the CLI child does (ui.PrettyPath)
// for the child's own home directory.
func childDisplayPath(childHome, path string) string {
	if path == childHome {
		return "~"
	}
	if strings.HasPrefix(path, childHome+string(filepath.Separator)) {
		return "~" + path[len(childHome):]
	}
	return path
}
