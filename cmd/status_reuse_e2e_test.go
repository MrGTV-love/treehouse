package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/config"
	"github.com/kunchenguid/treehouse/v3/internal/pool"
	"github.com/kunchenguid/treehouse/v3/internal/process"
)

type reuseFixture struct {
	own, foreign, home, root, poolDir string
	paths                             map[string]string
	want                              map[string]string
}

// Two real clones, one scratch shared pool. Names and expected statuses are
// assigned by construction, independently of the listing being tested.
func newReuseFixture(t *testing.T) reuseFixture {
	t.Helper()
	own, home := setupTestRepo(t)
	foreign := filepath.Join(filepath.Dir(own), "other", filepath.Base(own))
	gitCmd(t, "", "clone", filepath.Join(filepath.Dir(own), "remote.git"), foreign)
	root := filepath.Join(home, "scratch-pool")
	poolDir, err := config.ResolvePoolDir(own, root)
	if err != nil {
		t.Fatal(err)
	}
	f := reuseFixture{own: own, foreign: foreign, home: home, root: root, poolDir: poolDir, paths: map[string]string{}, want: map[string]string{}}
	// Interleave ownership: neither the slot name nor the path prefix identifies
	// a clone. Every slot has a durable lease until construction is complete.
	for _, row := range []struct {
		id      string
		foreign bool
		want    string
	}{
		{"R4", true, "foreign-idle"}, {"R1", false, "available"},
		{"R5", true, "leased"}, {"R8", true, "dirty"},
		{"R9", false, "dirty"}, {"R10", false, "leased"},
		{"R11", true, "unknown-reuse"}, {"R12", true, "damaged"},
		{"R2", false, "available"}, {"R3", false, "available"},
		{"R6", true, "in-use"}, {"R7", true, "in-use"},
	} {
		repo := own
		if row.foreign {
			repo = foreign
		}
		out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "get", "--lease", "--no-fetch", "--json")
		if code != 0 {
			t.Fatalf("construct %s: %d %s", row.id, code, stderr)
		}
		var lease leaseJSONResult
		if err := json.Unmarshal([]byte(out), &lease); err != nil {
			t.Fatal(err)
		}
		f.paths[row.id] = lease.Path
		f.want[row.id] = row.want
	}
	for id, path := range f.paths {
		if id == "R5" || id == "R10" {
			continue
		}
		if err := pool.Release(poolDir, path); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"R8", "R9"} {
		if err := os.WriteFile(filepath.Join(f.paths[id], "README.md"), []byte("keep dirty work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(f.paths["R12"], ".git")); err != nil {
		t.Fatal(err)
	}
	// R11 keeps its real marker and clean checkout; f.run injects failure
	// only at the Git common-directory resolver.
	startForeignWorktreeProcess(t, f.paths["R6"])
	state, err := pool.ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	started, ok := process.StartedAt(int32(os.Getpid()))
	if !ok {
		t.Fatal("cannot identify fixture owner")
	}
	for i := range state.Worktrees {
		if state.Worktrees[i].Path == f.paths["R7"] {
			state.Worktrees[i].OwnerPID = int32(os.Getpid())
			state.Worktrees[i].OwnerStartedAt = started
		}
	}
	if err := pool.WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	caseAlias := filepath.Join(filepath.Dir(own), strings.ToUpper(filepath.Base(own)))
	original, err := os.Stat(own)
	if err != nil {
		t.Fatal(err)
	}
	if alternate, err := os.Stat(caseAlias); err == nil && os.SameFile(original, alternate) {
		cmd := exec.Command("git", "--no-optional-locks", "rev-parse", "--absolute-git-dir")
		cmd.Dir = f.paths["R3"]
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(strings.TrimSpace(string(out)), "commondir"), []byte(filepath.Join(caseAlias, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("R3 differently-cased alias unexercised: case-sensitive filesystem (%v)", err)
	}
	alias := filepath.Join(home, "clone-alias")
	if err := os.Symlink(own, alias); err != nil {
		t.Logf("R2 alias unavailable: %v", err)
	} else {
		a, err := os.Stat(filepath.Join(alias, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(filepath.Join(own, ".git"))
		if err != nil || !os.SameFile(a, b) {
			t.Fatalf("fixture alias not same file: %v", err)
		}
		cmd := exec.Command("git", "--no-optional-locks", "rev-parse", "--absolute-git-dir")
		cmd.Dir = f.paths["R2"]
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(strings.TrimSpace(string(out)), "commondir"), []byte(filepath.Join(alias, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func identityFailureEnv(t *testing.T, dirs ...string) []string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	source := fmt.Sprintf(`package main
import("os";"os/exec";"strings";"fmt")
func main(){
 cwd,_:=os.Getwd()
 for _,arg:=range os.Args[1:] { if arg=="--git-common-dir" {
  for _,dir:=range strings.Split(os.Getenv("TREEHOUSE_TEST_UNREADABLE_IDENTITIES"),string(os.PathListSeparator)) { if cwd==dir { fmt.Fprintln(os.Stderr,"injected unreadable requester identity"); os.Exit(71) } }
  if cwd==os.Getenv("TREEHOUSE_TEST_ALIAS_SLOT") && os.Getenv("TREEHOUSE_TEST_ALIAS_COMMON_DIR")!="" { fmt.Println(os.Getenv("TREEHOUSE_TEST_ALIAS_COMMON_DIR"));os.Exit(0) }
 } }
 cmd:=exec.Command(%q,append([]string{"--no-optional-locks"},os.Args[1:]...)...)
 cmd.Stdin=os.Stdin;cmd.Stdout=os.Stdout;cmd.Stderr=os.Stderr
 if err:=cmd.Run();err!=nil { if e,ok:=err.(*exec.ExitError);ok { os.Exit(e.ExitCode()) };fmt.Fprintln(os.Stderr,err);os.Exit(1) }
}
`, realGit)
	file := filepath.Join(shim, "proxy.go")
	if err := os.WriteFile(file, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", filepath.Join(shim, name), file).CombinedOutput(); err != nil {
		t.Fatalf("build resolver proxy: %s %v", out, err)
	}
	return []string{"PATH=" + shim + string(os.PathListSeparator) + os.Getenv("PATH"), "TREEHOUSE_TEST_UNREADABLE_IDENTITIES=" + strings.Join(dirs, string(os.PathListSeparator))}
}

func (f reuseFixture) run(t *testing.T, dir string, extra []string, args ...string) (string, string, int) {
	t.Helper()
	env := append([]string{"TREEHOUSE_ROOT=" + f.root}, identityFailureEnv(t, f.paths["R11"])...)
	// Git may canonicalize symlink spellings itself. Preserve the actual
	// fixture alias at this resolver boundary so a string comparator cannot
	// accidentally pass R2 because Git already did the identity work for it.
	alias := filepath.Join(f.home, "clone-alias", ".git")
	if _, err := os.Stat(alias); err == nil {
		env = append(env, "TREEHOUSE_TEST_ALIAS_SLOT="+f.paths["R2"], "TREEHOUSE_TEST_ALIAS_COMMON_DIR="+alias)
	}
	env = append(env, extra...)
	return runTreehouse(t, dir, f.home, env, args...)
}

// This detects persistent changes only. The delivery evidence separately
// records whether the host can trace transient write attempts.
func TestStatusReusePreservesSlotFiles(t *testing.T) {
	f := newReuseFixture(t)
	snapshot := func() map[string]string {
		result := map[string]string{}
		for id, root := range f.paths {
			err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				digest := ""
				if info.Mode().IsRegular() {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					digest = fmt.Sprintf("%x", sha256.Sum256(data))
				}
				result[id+":"+relative] = fmt.Sprintf("%d %v %d %s", info.Size(), info.Mode(), info.ModTime().UnixNano(), digest)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	before := snapshot()
	_, stderr, code := f.run(t, f.own, nil, "status", "--json")
	if code != 0 {
		t.Fatalf("status: %d %s", code, stderr)
	}
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("slot file snapshot changed: before=%v after=%v", before, after)
	}
}

func TestStatusCallerReuseMatrix(t *testing.T) {
	f := newReuseFixture(t)
	out, stderr, code := f.run(t, f.own, nil, "status", "--json")
	if code != 0 {
		t.Fatalf("status: %d %s", code, stderr)
	}
	var rows []statusJSONResult
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(f.want) {
		t.Fatalf("rows: %s", out)
	}
	for id, path := range f.paths {
		found := false
		for _, row := range rows {
			if row.Path == path {
				found = true
				if row.Status != f.want[id] {
					t.Errorf("%s: got %s, want %s", id, row.Status, f.want[id])
				}
			}
		}
		if !found {
			t.Errorf("missing %s", id)
		}
	}
	text, stderr, code := f.run(t, f.own, nil, "status")
	if code != 0 {
		t.Fatalf("text status: %d %s", code, stderr)
	}
	for id, path := range f.paths {
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, strings.Replace(path, f.home, "~", 1)) && !strings.Contains(line, f.want[id]) {
				t.Errorf("text %s: %s", id, line)
			}
		}
	}
	t.Logf("JSON: %s\nText: %s", out, text)
}

func TestStatusReuseColumnAlignment(t *testing.T) {
	f := newReuseFixture(t)
	out, stderr, code := f.run(t, f.own, nil, "status")
	if code != 0 {
		t.Fatalf("status: %d %s", code, stderr)
	}
	column := -1
	count := 0
	for _, line := range strings.Split(out, "\n") {
		for _, path := range f.paths {
			if pos := strings.Index(line, strings.Replace(path, f.home, "~", 1)); pos >= 0 {
				if column < 0 {
					column = pos
				}
				if pos != column {
					t.Errorf("path shifted: %q at %d, want %d", line, pos, column)
				}
				count++
			}
		}
	}
	if count != len(f.paths) {
		t.Fatalf("found %d rows: %s", count, out)
	}
}

func TestStatusUnreadableRequester(t *testing.T) {
	f := newReuseFixture(t)
	out, stderr, code := f.run(t, f.own, identityFailureEnv(t, f.own, f.paths["R11"]), "status", "--json")
	if code == 0 {
		t.Errorf("unreadable requester exited 0; stderr %q", stderr)
	}
	if strings.Count(strings.TrimSpace(stderr), "\n") != 0 || !strings.Contains(stderr, f.own) || !strings.Contains(stderr, "injected unreadable requester identity") {
		t.Errorf("expected exactly one requester identity error: %q", stderr)
	}
	var rows []statusJSONResult
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("must still emit array: %v %s", err, out)
	}
	for _, row := range rows {
		want := "unknown-reuse"
		for id, path := range f.paths {
			if path == row.Path && f.want[id] != "available" && f.want[id] != "foreign-idle" && f.want[id] != "unknown-reuse" {
				want = f.want[id]
			}
		}
		if row.Status != want {
			t.Errorf("%s: %s, want %s", row.Path, row.Status, want)
		}
	}
	// Named entry is not acquisition: unreadable reuse identity must not
	// remove the existing ability to enter a known pool member by name.
	entered, enterErr, enterCode := f.run(t, f.own, identityFailureEnv(t, f.own, f.paths["R11"]), "enter", "--print-path", filepath.Base(filepath.Dir(f.paths["R4"])))
	if enterCode != 0 || strings.TrimSpace(entered) != f.paths["R4"] {
		t.Errorf("named entry must not depend on requester reuse identity: code=%d stdout=%q stderr=%q", enterCode, entered, enterErr)
	}
	// The new identity diagnostic must not mask a genuine membership/state
	// failure. A directory at the state-file path deterministically fails reads
	// even when the tests run with elevated filesystem privileges.
	statePath := filepath.Join(f.poolDir, "treehouse-state.json")
	if err := os.Rename(statePath, filepath.Join(f.home, "saved-state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	entered, enterErr, enterCode = f.run(t, f.own, nil, "enter", "--print-path", filepath.Base(filepath.Dir(f.paths["R4"])))
	if enterCode == 0 || entered != "" || !strings.Contains(enterErr, "treehouse-state.json") {
		t.Errorf("named entry lost fatal state-read refusal: code=%d stdout=%q stderr=%q", enterCode, entered, enterErr)
	}
	t.Logf("exit=%d stderr=%q json=%s", code, stderr, out)
}

func TestReturnAllSkipsForeignAndUnknownIdle(t *testing.T) {
	f := newReuseFixture(t)
	// A branch provides an externally visible reset detector: status cannot
	// prove preservation by merely agreeing with its own output.
	for _, id := range []string{"R4", "R11"} {
		gitCmd(t, f.paths[id], "checkout", "-b", "keep-"+id)
	}
	out, stderr, code := f.run(t, f.own, nil, "return", "--all", "--force")
	if code != 0 {
		t.Fatalf("return: %d %s %s", code, out, stderr)
	}
	for _, id := range []string{"R4", "R11"} {
		if strings.Contains(stderr, "Returning "+filepath.Base(filepath.Dir(f.paths[id]))+" (") {
			t.Errorf("acted on %s: %s", id, stderr)
		}
	}
	for _, id := range []string{"R4", "R11"} {
		cmd := exec.Command("git", "--no-optional-locks", "branch", "--show-current")
		cmd.Dir = f.paths[id]
		branch, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(branch)) != "keep-"+id {
			t.Errorf("idle %s slot reset: %q %v", id, branch, err)
		}
	}
	for _, id := range []string{"R8", "R9"} {
		data, err := os.ReadFile(filepath.Join(f.paths[id], "README.md"))
		if err != nil || string(data) != "hello\n" {
			t.Errorf("dirty %s not returned: %q %v", id, data, err)
		}
	}
	state, err := pool.ReadState(f.poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range state.Worktrees {
		if wt.Leased || wt.OwnerPID != 0 {
			t.Errorf("held claim not returned: %s", wt.Name)
		}
	}
	if inUse, err := process.IsWorktreeInUse(f.paths["R6"]); err != nil || inUse {
		t.Errorf("live process slot not returned: inUse=%v error=%v", inUse, err)
	}
	// Run again inside B's own parked slot: HeldOnlyByCwd must prevent reset.
	gitCmd(t, f.paths["R1"], "checkout", "-b", "keep-own-branch")
	_, stderr, code = f.run(t, f.paths["R1"], nil, "return", "--all", "--force")
	if code != 0 {
		t.Fatalf("inside return: %d %s", code, stderr)
	}
	cmd := exec.Command("git", "--no-optional-locks", "branch", "--show-current")
	cmd.Dir = f.paths["R1"]
	branch, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(branch)) != "keep-own-branch" {
		t.Fatalf("own parked slot reset: %q %v", branch, err)
	}
	t.Logf("second return stderr: %s", stderr)
}

func (f reuseFixture) branchOf(t *testing.T, id string) string {
	t.Helper()
	cmd := exec.Command("git", "--no-optional-locks", "branch", "--show-current")
	cmd.Dir = f.paths[id]
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read %s branch: %v", id, err)
	}
	return strings.TrimSpace(string(out))
}

func (f reuseFixture) assertHeldReturned(t *testing.T) {
	t.Helper()
	for _, id := range []string{"R8", "R9"} {
		data, err := os.ReadFile(filepath.Join(f.paths[id], "README.md"))
		if err != nil || string(data) != "hello\n" {
			t.Errorf("dirty %s not returned: %q %v", id, data, err)
		}
	}
	state, err := pool.ReadState(f.poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range state.Worktrees {
		if wt.Leased || wt.OwnerPID != 0 {
			t.Errorf("held claim not returned: %s", wt.Name)
		}
	}
	if inUse, err := process.IsWorktreeInUse(f.paths["R6"]); err != nil || inUse {
		t.Errorf("live process slot not returned: inUse=%v error=%v", inUse, err)
	}
}

// An unreadable requester identity (as in a .jj-only repository) must not stop
// return --all from returning held slots, and must not make idle slots targets.
func TestReturnAllUnreadableRequesterReturnsHeld(t *testing.T) {
	f := newReuseFixture(t)
	for _, id := range []string{"R1", "R4", "R11"} {
		gitCmd(t, f.paths[id], "checkout", "-b", "keep-"+id)
	}
	unreadable := identityFailureEnv(t, f.own, f.paths["R11"])
	out, stderr, code := f.run(t, f.own, unreadable, "return", "--all", "--force")
	if code != 0 {
		t.Fatalf("return --all with unreadable requester: %d %s %s", code, out, stderr)
	}
	for _, id := range []string{"R1", "R4", "R11"} {
		if got := f.branchOf(t, id); got != "keep-"+id {
			t.Errorf("idle %s slot reset: branch %q", id, got)
		}
	}
	f.assertHeldReturned(t)
	t.Logf("return stderr: %s", stderr)

	statePath := filepath.Join(f.poolDir, "treehouse-state.json")
	if err := os.Rename(statePath, filepath.Join(f.home, "saved-state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = f.run(t, f.own, unreadable, "return", "--all", "--force")
	if code == 0 || !strings.Contains(stderr, "treehouse-state.json") {
		t.Errorf("return --all lost fatal state-read refusal: code=%d stderr=%q", code, stderr)
	}
}

// Standing in one's own idle slot while the requester identity is unreadable
// shows that slot as "you're here" over unknown-reuse; it must not be reset.
func TestReturnAllUnreadableRequesterInsideOwnIdleSlot(t *testing.T) {
	f := newReuseFixture(t)
	gitCmd(t, f.paths["R1"], "checkout", "-b", "keep-R1")
	_, stderr, code := f.run(t, f.paths["R1"], identityFailureEnv(t, f.own, f.paths["R11"]), "return", "--all", "--force")
	if code != 0 {
		t.Fatalf("return --all inside own slot: %d %s", code, stderr)
	}
	if got := f.branchOf(t, "R1"); got != "keep-R1" {
		t.Errorf("own idle slot reset from inside it: branch %q; stderr %s", got, stderr)
	}
	f.assertHeldReturned(t)
	t.Logf("return stderr: %s", stderr)
}

func TestGetForeignRefusalReasons(t *testing.T) {
	own, home := setupTestRepo(t)
	foreign := filepath.Join(filepath.Dir(own), "other", filepath.Base(own))
	gitCmd(t, "", "clone", filepath.Join(filepath.Dir(own), "remote.git"), foreign)
	gitCmd(t, foreign, "config", "user.email", "test@test.com")
	gitCmd(t, foreign, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(foreign, ".gitignore"), []byte("private.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, foreign, "add", ".gitignore")
	gitCmd(t, foreign, "commit", "-m", "ignore scratch work")
	root := filepath.Join(home, "scratch-pool")
	poolDir, err := config.ResolvePoolDir(own, root)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for range 2 {
		out, stderr, code := runTreehouse(t, foreign, home, []string{"TREEHOUSE_ROOT=" + root}, "get", "--lease", "--no-fetch", "--json")
		if code != 0 {
			t.Fatalf("get: %d %s", code, stderr)
		}
		var lease leaseJSONResult
		if err := json.Unmarshal([]byte(out), &lease); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, lease.Path)
	}
	for _, path := range paths {
		if err := pool.Release(poolDir, path); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "--no-optional-locks", "rev-parse", "--path-format=absolute", "--git-path", "HEAD")
	cmd.Dir = paths[0]
	head, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSpace(string(head))+".lock", []byte("held\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths[1], "private.txt"), []byte("unique ignored work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(poolDir, "treehouse-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "treehouse.toml"), []byte("max_trees = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runTreehouse(t, own, home, []string{"TREEHOUSE_ROOT=" + root}, "get", "--lease", "--no-fetch", "--json")
	if code != 1 {
		t.Errorf("cap refusal status: %d %s", code, stderr)
	}
	for _, want := range []string{"worktree 1", "HEAD.lock", "worktree 2", "unseeded ignored path (private.txt)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("missing %q: %s", want, stderr)
		}
	}
	after, err := os.ReadFile(filepath.Join(poolDir, "treehouse-state.json"))
	if err != nil || string(before) != string(after) {
		t.Errorf("cap refusal changed state: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(paths[1], "private.txt"))
	if err != nil || string(data) != "unique ignored work\n" {
		t.Errorf("ignored work changed: %q %v", data, err)
	}
	t.Logf("exit=%d stderr=%s", code, stderr)
}
