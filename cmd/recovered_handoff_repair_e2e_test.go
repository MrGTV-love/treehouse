package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// J5: each writer of RecoveredLeaseHolder is handed off through the real command.
// A candidate that handles only the corruption origin fails the other four.
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
			row := handoffStatusRow(t, repo, home, root)
			if row.Status != "leased" || row.LeaseHolder != "real-task" || row.RecoveryReason != "" || row.Path != path {
				t.Fatalf("%s status after handoff: %+v", origin, row)
			}
			afterEntry := handoffStateEntry(t, state)
			for _, field := range []string{"name", "path", "created_at"} {
				if beforeEntry[field] != afterEntry[field] {
					t.Fatalf("%s changed %s: %v -> %v", origin, field, beforeEntry[field], afterEntry[field])
				}
			}
			if afterEntry["seed_inventory_known"] != true {
				t.Fatalf("%s lost the inventory: %v", origin, afterEntry)
			}
			if origin == "earlier-version-known-inventory" && beforeEntry["seed_inventory_digest"] != afterEntry["seed_inventory_digest"] {
				t.Fatalf("authenticated inventory digest changed: %v -> %v", beforeEntry, afterEntry)
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

// J1 (F1), deterministic and on every platform: the run's first read of the pool
// finds the slot recovered, a handoff completes before the run lists the pool
// (the order a real race produces, because the listing waits for the lock the
// handoff holds), and the run must not release the slot. A run that begins after
// the handoff is ordinary.
func TestBulkReturnBegunBeforeHandoffNeverReleasesSlot(t *testing.T) {
	repo, _, _, path, state := handoffFixture(t)
	poolDir := filepath.Dir(state)
	t.Chdir(repo)
	afterBulkReturnSnapshot = func() {
		if _, err := pool.HandoffRecoveredLease(poolDir, path, recoveredHolderLiteral, "real-task"); err != nil {
			t.Errorf("handoff inside the race window: %v", err)
		}
	}
	t.Cleanup(func() { afterBulkReturnSnapshot = nil })
	if err := returnHeldWorktreesIn(poolDir); err != nil {
		t.Fatalf("bulk return: %v", err)
	}
	afterBulkReturnSnapshot = nil
	entry := handoffStateEntry(t, state)
	if entry["leased"] != true || entry["lease_holder"] != "real-task" {
		t.Fatalf("a bulk return that began before the handoff released the slot: %v", entry)
	}
	// Ordinary policy after completion: a run that begins now returns it.
	if err := returnHeldWorktreesIn(poolDir); err != nil {
		t.Fatalf("second bulk return: %v", err)
	}
	if entry := handoffStateEntry(t, state); entry["leased"] == true {
		t.Fatalf("a bulk return that began after the handoff left the ordinary lease held: %v", entry)
	}
}

// J1 (F1) with real processes. The test holds the state lock, starts the
// handoff and then `return --all`, and releases the lock after both are waiting.
// Skipped on Windows: it relies on process start order and a fixed wait, which
// the deterministic seam test above covers there.
func TestBulkReturnRacingHandoffNeverReleasesSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("timing-based race; TestBulkReturnBegunBeforeHandoffNeverReleasesSlot covers it on Windows")
	}
	repo, home, root, path, state := handoffFixture(t)
	poolDir := filepath.Dir(state)
	env := []string{"TREEHOUSE_ROOT=" + root}
	var bulkStderr string
	var bulkCode, handoffCode int
	var handoffStderr string
	var wg sync.WaitGroup
	if err := pool.WithStateLock(poolDir, func() error {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, handoffStderr, handoffCode = runTreehouse(t, repo, home, env, handoffArgs(path, "real-task")...)
		}()
		time.Sleep(time.Second)
		go func() {
			defer wg.Done()
			_, bulkStderr, bulkCode = runTreehouse(t, repo, home, env, "return", "--all")
		}()
		time.Sleep(4 * time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if handoffCode != 0 || bulkCode != 0 {
		t.Fatalf("handoff=%d %s bulk=%d %s", handoffCode, handoffStderr, bulkCode, bulkStderr)
	}
	entry := handoffStateEntry(t, state)
	if entry["leased"] != true || entry["lease_holder"] != "real-task" || strings.Contains(bulkStderr, "Returning ") {
		t.Fatalf("racing return --all released the handed-off slot: %v\n%s", entry, bulkStderr)
	}
}
