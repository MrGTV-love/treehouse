package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Every subprocess is confined to an explicit scratch root, including observers
// and racing lifecycle commands. Recovery is exercised through truncated JSON,
// not by manufacturing a recovered entry.
func handoffFixture(t *testing.T) (repo, home, root, path, state string) {
	t.Helper()
	repo, home = setupTestRepo(t)
	root = filepath.Join(home, "scratch-pools")
	out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "get", "--lease", "--json")
	if code != 0 {
		t.Fatalf("scratch get: %d %s", code, stderr)
	}
	var lease leaseJSONResult
	if err := json.Unmarshal([]byte(out), &lease); err != nil {
		t.Fatal(err)
	}
	path = lease.Path
	state = filepath.Join(filepath.Dir(filepath.Dir(path)), "treehouse-state.json")
	if err := os.WriteFile(state, []byte("{"), 0644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "status", "--json")
	if code != 0 || !strings.Contains(stderr, "corrupt") {
		t.Fatalf("corruption recovery failed: %d %s", code, stderr)
	}
	t.Logf("scratch root=%s slot=%s", root, path)
	return
}

func handoffArgs(path, holder string) []string {
	return []string{"lease", "--path", path, "--recover", "--if-lease-holder", recoveredHolderLiteral, "--lease-holder", holder, "--json"}
}

type handoffInventoryEntry struct {
	Mode     fs.FileMode
	Size     int64
	Modified int64
	Changed  any
	Hash     [32]byte
	Link     string
}

// ctime detects transient writes even when a mutant deletes its marker again.
// Unix exposes nanosecond ctime; Windows' os.FileInfo has no change-time field.
// Lstat, not DirEntry.Info: on Windows the latter returns the directory
// listing's cached size and mtime, which NTFS updates lazily after a write, so
// files written just before the before-inventory could change between scans.
func handoffInventory(t *testing.T, root string) map[string]handoffInventoryEntry {
	t.Helper()
	entries := map[string]handoffInventoryEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := handoffInventoryEntry{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime().UnixNano()}
		sys := reflect.Indirect(reflect.ValueOf(info.Sys()))
		if sys.IsValid() && sys.Kind() == reflect.Struct {
			for _, field := range []string{"Ctim", "Ctimespec"} {
				if v := sys.FieldByName(field); v.IsValid() {
					entry.Changed = v.Interface()
				}
			}
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry.Hash = sha256.Sum256(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			entry.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func handoffInventoryDiff(before, after map[string]handoffInventoryEntry) string {
	var diff strings.Builder
	for rel, b := range before {
		if a, ok := after[rel]; !ok || !reflect.DeepEqual(a, b) {
			fmt.Fprintf(&diff, "\n  %s: %+v -> %+v (present after: %v)", rel, b, a, ok)
		}
	}
	for rel, a := range after {
		if _, ok := before[rel]; !ok {
			fmt.Fprintf(&diff, "\n  %s: added %+v", rel, a)
		}
	}
	return diff.String()
}

func handoffGitSnapshot(t *testing.T, path string) []string {
	t.Helper()
	var result []string
	for _, args := range [][]string{{"status", "--porcelain=v1", "--untracked-files=all"}, {"rev-parse", "HEAD"}, {"symbolic-ref", "HEAD"}, {"show-ref"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git observation %v: %v %s", args, err, out)
		}
		result = append(result, string(out))
	}
	return result
}

func TestRecoveredHandoffPreservesLiveCopy(t *testing.T) {
	repo, home, root, path, state := handoffFixture(t)
	nested := filepath.Join(path, "nested-copy")
	gitCmd(t, "", "clone", repo, nested)
	gitCmd(t, nested, "config", "user.name", "Test")
	gitCmd(t, nested, "config", "user.email", "test@test.com")
	// git commit spawns a detached "maintenance run --auto", which takes
	// objects/maintenance.lock in this repo. On slow runners it lands after the
	// before-inventory and shows up as a slot change the handoff never made.
	gitCmd(t, nested, "config", "maintenance.auto", "false")
	gitCmd(t, nested, "checkout", "-b", "live-task")
	for file, content := range map[string]string{".gitignore": "ignored\n", "private": "unpushed\n"} {
		if err := os.WriteFile(filepath.Join(nested, file), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, nested, "add", ".")
	gitCmd(t, nested, "commit", "-m", "unpushed task work")
	for file, content := range map[string]string{"README.md": "uncommitted\n", "untracked": "untracked\n", "ignored": "ignored\n"} {
		if err := os.WriteFile(filepath.Join(nested, file), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	gitBefore := handoffGitSnapshot(t, nested)
	outerGitDir := gitCmd(t, path, "rev-parse", "--absolute-git-dir")
	metadataBefore := handoffInventory(t, outerGitDir)
	before := handoffInventory(t, filepath.Dir(path))
	out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, handoffArgs(path, "real-task")...)
	if code != 0 {
		t.Fatalf("recovered handoff failed: %d %s", code, stderr)
	}
	var lease leaseJSONResult
	if err := json.Unmarshal([]byte(out), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.Path != path || lease.LeaseHolder != "real-task" || lease.LeaseID == "" {
		t.Fatalf("handoff identity: %+v", lease)
	}
	if after := handoffInventory(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
		t.Fatalf("handoff touched the slot inventory (including nanosecond change times):%s", handoffInventoryDiff(before, after))
	}
	if after := handoffInventory(t, outerGitDir); !reflect.DeepEqual(metadataBefore, after) {
		t.Fatalf("handoff touched the outer linked worktree git directory:%s", handoffInventoryDiff(metadataBefore, after))
	}
	if after := handoffGitSnapshot(t, nested); !reflect.DeepEqual(gitBefore, after) {
		t.Fatalf("nested git state changed: %v -> %v", gitBefore, after)
	}
	out, stderr, code = runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "status", "--json")
	var rows []statusJSONWorktree
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].LeaseHolder != "real-task" || rows[0].RecoveryReason != "" {
		t.Fatalf("ordinary lease not durable: %d %s %s", code, out, stderr)
	}
	t.Logf("accepted lease=%s state=%s; slot and nested repository unchanged", lease.LeaseID, state)
}

func TestRecoveredHandoffExactPathSelectsItsOwnPool(t *testing.T) {
	repoA, homeA, rootA, pathA, stateA := handoffFixture(t)
	repoB, homeB, rootB, pathB, stateB := handoffFixture(t)
	beforeA, err := os.ReadFile(stateA)
	if err != nil {
		t.Fatal(err)
	}
	// Both pools have slot 1. The caller stands in A, but names B's exact
	// path. Neither the cwd nor its configured scratch root may redirect it.
	_, stderr, code := runTreehouse(t, repoA, homeA, []string{"TREEHOUSE_ROOT=" + rootA}, handoffArgs(pathB, "task-b")...)
	if code != 0 {
		t.Fatalf("cross-pool exact-path handoff: %d %s", code, stderr)
	}
	afterA, err := os.ReadFile(stateA)
	if err != nil || !bytes.Equal(beforeA, afterA) {
		t.Fatalf("wrong pool changed: %v", err)
	}
	out, stderr, code := runTreehouse(t, repoB, homeB, []string{"TREEHOUSE_ROOT=" + rootB}, "status", "--json")
	var rows []statusJSONWorktree
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].LeaseHolder != "task-b" {
		t.Fatalf("target pool not handed off: %d %s %s", code, out, stderr)
	}
	t.Logf("slot-1 paths distinct: %s and %s; target state=%s", pathA, pathB, stateB)
}

func TestRecoveredHandoffRefusesPathOutsideAnyPoolWithoutCreatingOne(t *testing.T) {
	repo, home := setupTestRepo(t)
	root := filepath.Join(home, "scratch-pools")
	_, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, handoffArgs(repo, "real-task")...)
	if code == 0 || !strings.Contains(stderr, "not managed by treehouse") {
		t.Fatalf("non-pool path not refused: %d %s", code, stderr)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(root)
		t.Fatalf("refusal created pool root %s: %v %v", root, err, entries)
	}
}

func TestRecoveredHandoffRefusals(t *testing.T) {
	for _, kind := range []string{"unleased", "ordinary", "destroying", "unknown-path", "unknown-name", "stale-holder", "empty-holder", "recovered-holder", "control-holder", "corrupt-state", "unreadable-state", "missing-expectation", "name-only", "blank-holder", "missing-label", "unsupported-state", "missing-directory"} {
		t.Run(kind, func(t *testing.T) {
			repo, home, root, path, state := handoffFixture(t)
			args := handoffArgs(path, "real-task")
			reason := ""
			data, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "ordinary" {
				out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "get", "--lease", "--lease-holder", "someone-else", "--json")
				var ordinary leaseJSONResult
				if code != 0 || json.Unmarshal([]byte(out), &ordinary) != nil {
					t.Fatalf("ordinary lease fixture failed: %d %s %s", code, out, stderr)
				}
				path = ordinary.Path
				args = handoffArgs(path, "real-task")
				data, err = os.ReadFile(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "unleased" {
				_, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "return", path)
				if code != 0 {
					t.Fatalf("unleased fixture return failed: %d %s", code, stderr)
				}
				data, err = os.ReadFile(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			var s map[string]any
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatal(err)
			}
			wt := s["worktrees"].([]any)[0].(map[string]any)
			switch kind {
			case "unleased":
				wt["leased"] = false
				reason = "not a recovered lease"
			case "ordinary":
				reason = "not a recovered lease"
			case "destroying":
				wt["destroying"] = true
				reason = "being destroyed"
			case "unknown-path":
				args[2] = filepath.Join(path, "unknown")
				reason = "not managed"
			case "unknown-name":
				args = []string{"lease", "999", "--recover", "--if-lease-holder", recoveredHolderLiteral, "--lease-holder", "real-task"}
				reason = "--path"
			case "stale-holder":
				args[5] = "stale"
				reason = "holder does not match"
			case "empty-holder":
				args[7] = ""
				reason = "non-empty"
			case "recovered-holder":
				args[7] = recoveredHolderLiteral
				reason = "recovered holder"
			case "control-holder":
				args[7] = "bad\nlabel"
				reason = "control"
			case "corrupt-state":
				reason = "state"
			case "unreadable-state":
				reason = "state"
			case "missing-expectation":
				args = append(args[:4], args[6:]...)
				reason = "--if-lease-holder"
			case "name-only":
				args = []string{"lease", "1", "--recover", "--if-lease-holder", recoveredHolderLiteral, "--lease-holder", "real-task"}
				reason = "--path"
			case "blank-holder":
				args[7] = "  "
				reason = "non-empty"
			case "missing-label":
				args = append(args[:6], args[8:]...)
				reason = "explicit --lease-holder"
			case "unsupported-state":
				s["version"] = 999
				reason = "state version"
			case "missing-directory":
				// Retain the task-created copy beside its former path. The
				// container inventory proves the refusal did not touch it.
				if err := os.Rename(path, path+"-retained-fixture"); err != nil {
					t.Fatal(err)
				}
				reason = "not managed"
			}
			data, err = json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt-state" {
				data = []byte("{")
			}
			if err := os.WriteFile(state, data, 0644); err != nil {
				t.Fatal(err)
			}
			if kind == "unreadable-state" {
				// A directory is unreadable as a state file even for privileged test users.
				if err := os.Remove(state); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(state, 0755); err != nil {
					t.Fatal(err)
				}
			}
			before := handoffInventory(t, filepath.Dir(path))
			_, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root, "TREEHOUSE_LEASE_HOLDER=environment-task"}, args...)
			if code == 0 || !strings.Contains(stderr, reason) {
				t.Fatalf("%s refusal: code=%d want=%q stderr=%s", kind, code, reason, stderr)
			}
			if kind == "unreadable-state" {
				info, err := os.Stat(state)
				if err != nil || !info.IsDir() {
					t.Fatalf("state directory changed: %v", err)
				}
				items, err := os.ReadDir(state)
				if err != nil || len(items) != 0 {
					t.Fatalf("unreadable state changed: %v %v", items, err)
				}
			} else if after, err := os.ReadFile(state); err != nil || !bytes.Equal(data, after) {
				t.Fatalf("refusal changed state: %v", err)
			}
			if after := handoffInventory(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed slot inventory:%s", handoffInventoryDiff(before, after))
			}
		})
	}
}

// This subprocess reads the actual on-disk JSON continuously, independently of
// the command's locks. TestMain bypasses building binaries for this helper.
func TestRecoveredHandoffObserver(t *testing.T) {
	state := os.Getenv("TREEHOUSE_HANDOFF_OBSERVER")
	if state == "" {
		t.Skip("subprocess observer")
	}
	target := os.Getenv("TREEHOUSE_HANDOFF_TARGET")
	fmt.Println("observer-ready")
	samples := 0
	for {
		if _, err := os.Stat(state + ".observer-stop"); err == nil {
			break
		}
		data, err := os.ReadFile(state)
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Worktrees []struct {
				Path   string
				Leased bool
			}
		}
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, wt := range s.Worktrees {
			if wt.Path == target {
				found = true
				if !wt.Leased {
					t.Fatal("observer saw unleased target")
				}
			}
		}
		if !found {
			t.Fatal("observer saw target disappear")
		}
		samples++
	}
	if samples == 0 {
		t.Fatal("observer made no observations")
	}
	t.Logf("observer samples=%d", samples)
}

// On Windows an open read handle makes ReplaceFileW in the state commit path
// fail with a sharing violation, so the tight-loop observer would fail the
// racing commands for a test-only reason; the race still runs there unobserved.
func startHandoffObserver(t *testing.T, state, path string) (stop func() string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return func() string { return "" }
	}
	observer := exec.Command(os.Args[0], "-test.run=^TestRecoveredHandoffObserver$", "-test.v")
	observer.Env = append(os.Environ(), "TREEHOUSE_HANDOFF_OBSERVER="+state, "TREEHOUSE_HANDOFF_TARGET="+path)
	pipe, err := observer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var observerErr bytes.Buffer
	observer.Stderr = &observerErr
	if err := observer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if observer.ProcessState == nil {
			_ = observer.Process.Kill()
			_ = observer.Wait()
		}
	})
	// Read through the explicit readiness marker before starting any mutation.
	var prefix bytes.Buffer
	one := make([]byte, 1)
	for !strings.Contains(prefix.String(), "observer-ready\n") {
		if _, err := io.ReadFull(pipe, one); err != nil {
			t.Fatalf("observer startup: %v %s", err, observerErr.String())
		}
		prefix.Write(one)
	}
	var rest bytes.Buffer
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(&rest, pipe); close(drained) }()
	return func() string {
		if err := os.WriteFile(state+".observer-stop", nil, 0644); err != nil {
			t.Fatal(err)
		}
		<-drained
		if err := observer.Wait(); err != nil {
			t.Fatalf("observer failed: %v %s %s %s", err, prefix.String(), rest.String(), observerErr.String())
		}
		return rest.String()
	}
}

func TestRecoveredHandoffConcurrentLifecycle(t *testing.T) {
	const iterations = 8
	for iteration := range iterations {
		repo, home, root, path, state := handoffFixture(t)
		stopObserver := startHandoffObserver(t, state, path)
		var wg sync.WaitGroup
		for _, args := range [][]string{{"get", "--lease", "--json"}, {"prune", "--yes"}, {"destroy", path, "--yes"}} {
			wg.Add(1)
			go func(args []string) {
				defer wg.Done()
				out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, args...)
				if args[0] == "get" {
					var lease leaseJSONResult
					if code != 0 || json.Unmarshal([]byte(out), &lease) != nil || lease.Path == path {
						t.Errorf("racing get: %d %s %s", code, out, stderr)
					}
				}
			}(args)
		}
		_, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, handoffArgs(path, "real-task")...)
		wg.Wait()
		observed := stopObserver()
		if code != 0 {
			t.Fatalf("handoff: %d %s", code, stderr)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("racing lifecycle removed target: %v", err)
		}
		t.Logf("iteration=%d %s", iteration, observed)
	}
}
