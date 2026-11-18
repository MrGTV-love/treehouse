package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/pool"
)

func handoffStatusRow(t *testing.T, repo, home, root string) statusJSONWorktree {
	t.Helper()
	out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "status", "--json")
	var rows []statusJSONWorktree
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 {
		t.Fatalf("status: %d %s %s", code, out, stderr)
	}
	return rows[0]
}

func handoffStateEntry(t *testing.T, state string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	if err := json.Unmarshal(data, &s); err != nil || len(s.Worktrees) != 1 {
		t.Fatalf("state: %v %s", err, data)
	}
	return s.Worktrees[0]
}

func changedEntryKeys(before, after map[string]any) []string {
	var keys []string
	for k, b := range before {
		if a, ok := after[k]; !ok || !reflect.DeepEqual(a, b) {
			keys = append(keys, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Origins whose seed inventory is unknown after recovery; the amendment of J1
// (key r3-f2-unknown-inventory) lets the handoff record an authenticated empty
// cleanup list for these and nothing else about the inventory.
var unknownInventoryOrigins = []string{"corrupt-state", "missing-state-file", "missing-entry", "lost-state-key"}

// J5: each writer of RecoveredLeaseHolder is handed off through the real command.
// Beside the handoff's own marker and the lease it mints, the unknown-inventory
// origins change exactly seed_inventory_known and seed_inventory_digest, and the
// known-inventory origin leaves every seed field byte-identical. A later `status`
// keeps the new holder: an ordinary lease with an unknown inventory would be
// quarantined again by ReadState, so this is the permanent regression of that
// probe.
func TestRecoveredHandoffEveryRecoveredOrigin(t *testing.T) {
	for _, origin := range recoveredOrigins {
		t.Run(origin, func(t *testing.T) {
			repo, home, root, path, state := handoffFixtureFor(t, origin)
			beforeEntry := handoffStateEntry(t, state)
			out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, handoffArgs(path, "real-task")...)
			if code != 0 {
				t.Fatalf("%s handoff failed: %d %s", origin, code, stderr)
			}
			var lease leaseJSONResult
			if err := json.Unmarshal([]byte(out), &lease); err != nil || lease.Path != path || lease.LeaseHolder != "real-task" {
				t.Fatalf("%s handoff result: %v %s", origin, err, out)
			}
			afterEntry := handoffStateEntry(t, state)
			changed := changedEntryKeys(beforeEntry, afterEntry)
			leaseFields := []string{"handed_off_at", "handoff_lease_id", "lease_holder", "lease_id", "leased_at"}
			var want []string
			if origin == "earlier-version-known-inventory" {
				want = append(append(want, leaseFields...), "recovery_reason")
				for _, k := range []string{"seed_inventory_known", "seed_inventory_digest", "seeded_paths", "seed_backend", "seed_auth_identity"} {
					if !reflect.DeepEqual(beforeEntry[k], afterEntry[k]) {
						t.Fatalf("%s changed seed field %s: %v -> %v", origin, k, beforeEntry[k], afterEntry[k])
					}
				}
			} else {
				want = append(append(want, leaseFields...), "seed_inventory_digest", "seed_inventory_known")
				if beforeEntry["seed_inventory_known"] != nil || beforeEntry["seed_inventory_digest"] != nil {
					t.Fatalf("%s did not start with an unknown inventory: %v", origin, beforeEntry)
				}
				if afterEntry["seed_inventory_known"] != true || afterEntry["seed_inventory_digest"] == nil || afterEntry["seeded_paths"] != nil {
					t.Fatalf("%s did not record an authenticated empty inventory: %v", origin, afterEntry)
				}
			}
			sort.Strings(want)
			if !reflect.DeepEqual(changed, want) {
				t.Fatalf("%s changed fields %v, want exactly %v", origin, changed, want)
			}
			for _, field := range []string{"name", "path", "created_at"} {
				if beforeEntry[field] != afterEntry[field] {
					t.Fatalf("%s changed %s: %v -> %v", origin, field, beforeEntry[field], afterEntry[field])
				}
			}
			// Permanent regression: the state reader keeps the new holder.
			for range 2 {
				row := handoffStatusRow(t, repo, home, root)
				if row.Status != "leased" || row.LeaseHolder != "real-task" || row.RecoveryReason != "" || row.Path != path {
					t.Fatalf("%s: status after handoff lost the new holder: %+v", origin, row)
				}
			}
		})
	}
}

// Amendment of J1 (key r3-f2-unknown-inventory), required test 2: after a handoff on an
// unknown-inventory origin, a later return, get and prune of the slot delete no
// file that was present at handoff. An ignored file in the slot is not
// cleanup-authorized by the empty list; a handoff that recorded a non-empty list
// naming it would have return delete it.
func TestRecoveredHandoffUnknownInventoryDeletesNothingLater(t *testing.T) {
	for _, origin := range unknownInventoryOrigins {
		t.Run(origin, func(t *testing.T) {
			repo, home, root, path, _ := handoffFixtureFor(t, origin)
			env := []string{"TREEHOUSE_ROOT=" + root}
			exclude := gitCmd(t, path, "rev-parse", "--git-path", "info/exclude")
			if !filepath.IsAbs(exclude) {
				exclude = filepath.Join(path, exclude)
			}
			if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exclude, []byte("handoff-ignored-*\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			kept := []string{filepath.Join(path, "handoff-ignored-file"), filepath.Join(path, "handoff-ignored-dir")}
			if err := os.WriteFile(kept[0], []byte("not the pool's\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(kept[1], 0o755); err != nil {
				t.Fatal(err)
			}
			kept = append(kept, filepath.Join(kept[1], "inside"))
			if err := os.WriteFile(kept[2], []byte("inside\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, stderr, code := runTreehouse(t, repo, home, env, handoffArgs(path, "real-task")...); code != 0 {
				t.Fatalf("handoff: %d %s", code, stderr)
			}
			present := func(step string) {
				t.Helper()
				for _, f := range kept {
					if _, err := os.Lstat(f); err != nil {
						t.Fatalf("%s deleted %s that was present at handoff: %v", step, f, err)
					}
				}
			}
			present("handoff")
			// prune skips the leased slot. (After a return the slot is idle and prune
			// removes an idle slot wholesale, ignored files included, exactly as for an
			// ordinary slot: TestOrdinaryIdleSlotPruneControl.)
			if _, stderr, code := runTreehouse(t, repo, home, env, "prune", "--yes"); code != 0 {
				t.Fatalf("prune: %d %s", code, stderr)
			}
			present("prune of the leased slot")
			if _, stderr, code := runTreehouse(t, repo, home, env, "return", path); code != 0 {
				t.Fatalf("return: %d %s", code, stderr)
			}
			present("return")
			out, stderr, code := runTreehouse(t, repo, home, env, "get", "--lease", "--json")
			var lease leaseJSONResult
			if code != 0 || json.Unmarshal([]byte(out), &lease) != nil {
				t.Fatalf("get: %d %s %s", code, out, stderr)
			}
			present("get")
			if lease.Path == path {
				if _, stderr, code := runTreehouse(t, repo, home, env, "return", path); code != 0 {
					t.Fatalf("second return: %d %s", code, stderr)
				}
				present("second return")
			}
		})
	}
}

// F2: a state file that exists but cannot be read is reported with its read
// error, not as an unmanaged worktree, and nothing is created.
func TestRecoveredHandoffDanglingStateReportsReadReason(t *testing.T) {
	repo, home, root, path, state := handoffFixture(t)
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(state), "missing-target"), state); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	poolDir := filepath.Dir(state)
	before := handoffInventory(t, poolDir)
	_, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, handoffArgs(path, "real-task")...)
	if code == 0 || !strings.Contains(stderr, "reading handoff state") || strings.Contains(stderr, "not managed by treehouse") {
		t.Fatalf("dangling state refusal: code=%d stderr=%s", code, stderr)
	}
	if after := handoffInventory(t, poolDir); len(after) != len(before) {
		t.Fatalf("refusal changed the pool directory:%s", handoffInventoryDiff(before, after))
	}
}

// setProcessStartedAt makes this test process stand in for a `return --all`
// that started at the given instant.
func setProcessStartedAt(t *testing.T, started time.Time) {
	t.Helper()
	saved := processStartedAt
	processStartedAt = started
	t.Cleanup(func() { processStartedAt = saved })
}

func handoffEntryStamp(t *testing.T, state string) time.Time {
	t.Helper()
	entry := handoffStateEntry(t, state)
	raw, ok := entry["handed_off_at"].(string)
	if !ok || entry["handoff_lease_id"] != entry["lease_id"] {
		t.Fatalf("handoff left no completion marker bound to its lease: %v", entry)
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || at.Year() > 3000 {
		t.Fatalf("handoff completion time %q (%v) is missing or still pending", raw, err)
	}
	return at
}

// J1 on every platform: a `return --all` that started at or before the moment a
// handoff completed never releases the slot it produced, however late it lists
// the pool; one that started after releases it as an ordinary lease. The start is
// the process's own, so this covers a run delayed before it reads any state.
func TestBulkReturnStartedBeforeHandoffNeverReleasesSlot(t *testing.T) {
	repo, _, _, path, state := handoffFixture(t)
	poolDir := filepath.Dir(state)
	t.Chdir(repo)
	if _, err := pool.HandoffRecoveredLease(poolDir, path, recoveredHolderLiteral, "real-task"); err != nil {
		t.Fatal(err)
	}
	stamp := handoffEntryStamp(t, state)
	for name, started := range map[string]time.Time{"long before": stamp.Add(-time.Hour), "just before": stamp.Add(-time.Nanosecond), "tie": stamp} {
		setProcessStartedAt(t, started)
		if err := returnHeldWorktreesIn(poolDir); err != nil {
			t.Fatalf("%s: bulk return: %v", name, err)
		}
		if entry := handoffStateEntry(t, state); entry["leased"] != true || entry["lease_holder"] != "real-task" {
			t.Fatalf("a bulk return that started %s the handoff completed released the slot: %v", name, entry)
		}
	}
	setProcessStartedAt(t, stamp.Add(time.Nanosecond))
	if err := returnHeldWorktreesIn(poolDir); err != nil {
		t.Fatalf("bulk return after the handoff: %v", err)
	}
	if entry := handoffStateEntry(t, state); entry["leased"] == true {
		t.Fatalf("a bulk return that started after the handoff left the ordinary lease held: %v", entry)
	}
}

// R3-F3: the marker belongs to the lease the handoff minted. A later ordinary
// acquisition of the same slot, with or without an earlier handoff, is an
// ordinary lease for every bulk return, whenever that run started.
func TestBulkReturnReleasesLaterAcquisitionOfAHandedOffOrRecoveredSlot(t *testing.T) {
	for _, handedOff := range []bool{false, true} {
		name := map[bool]string{false: "no handoff", true: "handoff then named return"}[handedOff]
		t.Run(name, func(t *testing.T) {
			repo, home, root, path, state := handoffFixture(t)
			poolDir := filepath.Dir(state)
			env := []string{"TREEHOUSE_ROOT=" + root}
			if handedOff {
				if _, err := pool.HandoffRecoveredLease(poolDir, path, recoveredHolderLiteral, "real-task"); err != nil {
					t.Fatal(err)
				}
			}
			if _, stderr, code := runTreehouse(t, repo, home, env, "return", path); code != 0 {
				t.Fatalf("named return: %d %s", code, stderr)
			}
			out, stderr, code := runTreehouse(t, repo, home, env, "get", "--lease", "--lease-holder", "new-acquisition", "--json")
			var lease leaseJSONResult
			if code != 0 || json.Unmarshal([]byte(out), &lease) != nil || lease.Path != path {
				t.Fatalf("reacquisition: %d %s %s", code, out, stderr)
			}
			t.Chdir(repo)
			setProcessStartedAt(t, time.Unix(1, 0))
			if err := returnHeldWorktreesIn(poolDir); err != nil {
				t.Fatalf("bulk return: %v", err)
			}
			if entry := handoffStateEntry(t, state); entry["leased"] == true {
				t.Fatalf("a bulk return skipped an ordinary reacquisition (%s): %v", name, entry)
			}
		})
	}
}

// R3-F1 with real processes and no sleeps. A git shim holds the real
// `return --all` in its first repository-discovery step, before it reads any
// state; the handoff then completes, and only then does the run continue. It
// started before the handoff completed, so it must leave the slot alone. The
// shim is a shell script, so this runs where there is a POSIX shell; the
// in-process test above covers Windows.
func TestBulkReturnDelayedBeforeFirstReadNeverReleasesSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a POSIX shell script; TestBulkReturnStartedBeforeHandoffNeverReleasesSlot covers Windows")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	repo, home, root, path, state := handoffFixture(t)
	shimDir := t.TempDir()
	script := "#!/bin/sh\nif [ ! -e \"$SHIM_DIR/seen\" ]; then : > \"$SHIM_DIR/seen\"; while [ ! -e \"$SHIM_DIR/go\" ]; do sleep 0.05; done; fi\nexec \"$REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"TREEHOUSE_ROOT=" + root, "SHIM_DIR=" + shimDir, "REAL_GIT=" + realGit, "PATH=" + shimDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	var bulkStderr string
	var bulkCode int
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, bulkStderr, bulkCode = runTreehouse(t, repo, home, env, "return", "--all")
	}()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(shimDir, "seen")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the return --all process never reached its first git call")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, handoffArgs(path, "real-task")...); code != 0 {
		t.Fatalf("handoff: %d %s", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	<-done
	entry := handoffStateEntry(t, state)
	if bulkCode != 0 || entry["leased"] != true || entry["lease_holder"] != "real-task" || !strings.Contains(bulkStderr, "Returned 0 of 1") || !strings.Contains(bulkStderr, "1 skipped") {
		t.Fatalf("a return --all that started before the handoff released the handed-off slot: code=%d entry=%v\n%s", bulkCode, entry, bulkStderr)
	}
	// The same slot, handed off and finished, is ordinary for a run started now.
	if _, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "return", "--all"); code != 0 {
		t.Fatalf("later return --all: %d %s", code, stderr)
	}
	if entry := handoffStateEntry(t, state); entry["leased"] == true {
		t.Fatalf("a return --all started after the handoff left the ordinary lease held: %v", entry)
	}
}

// R3-F4: retained negative control for the release window itself. A real
// release followed (after a barrier) by a lease is the shape of the "return then
// lease" mutant. With no lock held between the two steps, the observer must see
// the slot unleased and a racing `get` must be able to receive that exact slot.
// If the observer or the allocation path could not see the window, the atomic
// handoff's passing observer test would prove nothing.
func TestRecoveredHandoffReleaseThenLeaseWindowIsSeenAndAllocatable(t *testing.T) {
	repo, home, root, path, state := handoffFixture(t)
	poolDir := filepath.Dir(state)
	stopObserver := startHandoffObserver(t, state, path)
	if err := pool.Release(poolDir, path); err != nil {
		t.Fatal(err)
	}
	// Barrier: the slot stays released until the racing get has returned.
	out, stderr, code := runTreehouse(t, repo, home, []string{"TREEHOUSE_ROOT=" + root}, "get", "--lease", "--json")
	var lease leaseJSONResult
	if code != 0 || json.Unmarshal([]byte(out), &lease) != nil || lease.Path != path {
		t.Fatalf("the racing get could not receive the released slot: %d %s %s", code, out, stderr)
	}
	if _, err := stopObserver(); err == nil || !strings.Contains(err.Error(), "observer saw unleased target") {
		t.Fatalf("the observer missed a real release window: %v", err)
	}
}

// R3-F5: one continuous identity watcher over the state file across every
// refusal class. It polls the file's identity (inode where the platform exposes
// one, otherwise os.SameFile against the first stat), size, modification time and
// change time, and a refusal must leave exactly one distinct identity.
type stateIdentity struct {
	Size     int64
	Modified int64
	Inode    any
	Changed  any
}

func statStateIdentity(path string) (stateIdentity, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return stateIdentity{}, nil, err
	}
	id := stateIdentity{Size: info.Size(), Modified: info.ModTime().UnixNano()}
	sys := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if sys.IsValid() && sys.Kind() == reflect.Struct {
		for _, field := range []string{"Ctim", "Ctimespec"} {
			if v := sys.FieldByName(field); v.IsValid() {
				id.Changed = v.Interface()
			}
		}
		if v := sys.FieldByName("Ino"); v.IsValid() {
			id.Inode = v.Interface()
		}
	}
	return id, info, nil
}

// watchStateIdentity returns stop, which reports how many distinct identities the
// watcher saw (the file being replaced, or removed and recreated, counts) and how
// many samples it took. Where the platform exposes no inode, a replaced file is
// still told apart by os.SameFile against the first stat, and by mtime.
func watchStateIdentity(t *testing.T, path string) (stop func() (distinct, samples int)) {
	t.Helper()
	first, firstInfo, err := statStateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	stopCh := make(chan struct{})
	result := make(chan [2]int)
	go func() {
		seen := map[stateIdentity]bool{first: true}
		replaced := false
		samples := 0
		for {
			select {
			case <-stopCh:
				distinct := len(seen)
				if replaced && distinct < 2 {
					distinct = 2
				}
				result <- [2]int{distinct, samples}
				return
			default:
			}
			if id, info, err := statStateIdentity(path); err == nil {
				seen[id] = true
				if !os.SameFile(firstInfo, info) {
					replaced = true
				}
			}
			samples++
		}
	}()
	return func() (int, int) {
		close(stopCh)
		r := <-result
		return r[0], r[1]
	}
}

// Positive control: the watcher sees a transient rewrite that restores the very
// same bytes (what a refusal that unleases and restores would leave behind).
func TestStateIdentityWatcherSeesTransientRewrite(t *testing.T) {
	_, _, _, _, state := handoffFixture(t)
	stop := watchStateIdentity(t, state)
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	tmp := state + ".rewrite"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, state); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if distinct, samples := stop(); distinct < 2 {
		t.Fatalf("the watcher missed a same-bytes rewrite (%d identities in %d samples)", distinct, samples)
	}
}

// Control for the sequence above: prune removes an idle slot wholesale, ignored
// files included, for a slot that was never recovered. So a file lost to prune
// after a return is the ordinary lifecycle, not an effect of the handoff's list.
func TestOrdinaryIdleSlotPruneControl(t *testing.T) {
	repo, home := setupTestRepo(t)
	root := filepath.Join(home, "scratch-pools")
	env := []string{"TREEHOUSE_ROOT=" + root}
	out, stderr, code := runTreehouse(t, repo, home, env, "get", "--lease", "--json")
	var lease leaseJSONResult
	if code != 0 || json.Unmarshal([]byte(out), &lease) != nil {
		t.Fatalf("get: %d %s %s", code, out, stderr)
	}
	exclude := gitCmd(t, lease.Path, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(exclude) {
		exclude = filepath.Join(lease.Path, exclude)
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte("handoff-ignored-*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(lease.Path, "handoff-ignored-file")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runTreehouse(t, repo, home, env, "return", lease.Path); code != 0 {
		t.Fatalf("return: %d %s", code, stderr)
	}
	if _, stderr, code := runTreehouse(t, repo, home, env, "prune", "--yes"); code != 0 {
		t.Fatalf("prune: %d %s", code, stderr)
	}
	if _, err := os.Lstat(file); err == nil {
		t.Fatalf("prune kept an ignored file in an idle ordinary slot; the control's premise changed")
	}
}
