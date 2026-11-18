package pool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func handoffFixtureSlot(t *testing.T) (poolDir, path string) {
	t.Helper()
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 1)
	quarantine(t, poolDir, "1")
	return poolDir, paths[0]
}

func readFenceFile(t *testing.T, poolDir, leaseID string) handoffFence {
	t.Helper()
	data, err := os.ReadFile(handoffFencePath(poolDir, leaseID))
	if err != nil {
		t.Fatalf("reading the handoff fence: %v", err)
	}
	var fence handoffFence
	if err := json.Unmarshal(data, &fence); err != nil {
		t.Fatal(err)
	}
	return fence
}

func bulkStartedAt(poolDir, path string, started time.Time) error {
	return ValidateReleasePreconditions(poolDir, path, ReleasePreconditions{BulkStartedAt: &started}, nil)
}

func clearHandoffSeams(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		beforeHandoffHolderWrite, afterHandoffHolderWrite = nil, nil
		beforeHandoffSealWrite, afterHandoffSealWrite = nil, nil
	})
}

func TestRecoveredHandoffFencesItsLeaseForBulkReturns(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	began := time.Now()
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	fence := readFenceFile(t, poolDir, lease.LeaseID)
	if fence.LeaseID != lease.LeaseID || fence.Boundary.Before(began) || fence.Boundary.Year() > 3000 {
		t.Fatalf("fence = %+v, want the minted lease and a real boundary", fence)
	}
	// The fence is not in the state file, which an older binary rewrites.
	raw, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	var entries struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries.Worktrees {
		for key := range entry {
			if strings.Contains(key, "handoff") || strings.Contains(key, "handed_off") {
				t.Fatalf("the state entry carries a handoff field %q", key)
			}
		}
	}
	for name, started := range map[string]time.Time{"long before": fence.Boundary.Add(-time.Hour), "just before": fence.Boundary.Add(-time.Nanosecond), "tie": fence.Boundary} {
		if err := bulkStartedAt(poolDir, path, started); !errors.Is(err, ErrHandedOffEntry) {
			t.Fatalf("a run that started %s the boundary was not refused: %v", name, err)
		}
	}
	if err := bulkStartedAt(poolDir, path, fence.Boundary.Add(time.Nanosecond)); err != nil {
		t.Fatalf("a run that started after the boundary was refused: %v", err)
	}
	// A named release is never fenced, a release clears the fence, and a later
	// acquisition mints a new lease, so the old fence is inert for it.
	if err := ValidateReleasePreconditions(poolDir, path, ReleasePreconditions{}, nil); err != nil {
		t.Fatalf("a named release was refused: %v", err)
	}
	if err := Release(poolDir, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(handoffFencePath(poolDir, lease.LeaseID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fence outlived its lease: %v", err)
	}
	if _, err := LeaseExisting(poolDir, "1", "another"); err != nil {
		t.Fatal(err)
	}
	if err := bulkStartedAt(poolDir, path, fence.Boundary.Add(-time.Hour)); err != nil {
		t.Fatalf("a later ordinary acquisition was fenced by the old handoff: %v", err)
	}
}

func TestRecoveredHandoffFenceExistsBeforeTheHolderChanges(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	clearHandoffSeams(t)
	var holder string
	var fence handoffFence
	beforeHandoffHolderWrite = func() {
		state, err := ReadState(poolDir)
		if err != nil {
			t.Error(err)
			return
		}
		holder = state.Worktrees[0].LeaseHolder
		matches, _ := filepath.Glob(filepath.Join(poolDir, handoffFencePrefix+"*"+handoffFenceSuffix))
		if len(matches) != 1 {
			t.Errorf("fences before the holder change = %v, want one", matches)
			return
		}
		data, err := os.ReadFile(matches[0])
		if err != nil || json.Unmarshal(data, &fence) != nil {
			t.Errorf("reading the fence: %v", err)
		}
	}
	if _, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task"); err != nil {
		t.Fatal(err)
	}
	if holder != RecoveredLeaseHolder || fence.LeaseID == "" || fence.Boundary.Year() < 9000 {
		t.Fatalf("before the holder change: holder %q, fence %+v; want the recovered holder and a pending fence", holder, fence)
	}
}

func TestRecoveredHandoffFenceIsPendingUntilSealed(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	clearHandoffSeams(t)
	var holder string
	var between handoffFence
	var admitted error
	afterHandoffHolderWrite = func() {
		state, err := ReadState(poolDir)
		if err != nil {
			t.Error(err)
			return
		}
		wt := state.Worktrees[0]
		holder = wt.LeaseHolder
		between = readFenceFile(t, poolDir, wt.LeaseID)
		// The handoff holds the state lock here, so ask the check directly.
		admitted = refuseFencedBulkReturn(poolDir, wt, time.Now().Add(time.Hour))
	}
	if _, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task"); err != nil {
		t.Fatal(err)
	}
	if holder != "real-task" || between.Boundary.Year() < 9000 {
		t.Fatalf("between the writes: holder %q, fence %+v; want the new holder and a pending fence", holder, between)
	}
	// What a crash between the holder change and the seal leaves: every bulk
	// return is refused, however late it started.
	if !errors.Is(admitted, ErrHandedOffEntry) {
		t.Fatalf("a bulk return was admitted while the fence was pending: %v", admitted)
	}
}

func TestRecoveredHandoffBoundaryIsNotBeforeItsOwnCommit(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	clearHandoffSeams(t)
	// A write outlasts its boundary on the first attempt, as a stalled disk would
	// make it. A run that starts while the boundary is chosen but not yet written,
	// and one that starts right after the first commit, both began before the
	// handoff completed.
	var duringChoice, afterFirstCommit time.Time
	attempts := 0
	var commits []time.Time
	beforeHandoffSealWrite = func(attempt int, boundary time.Time) {
		attempts++
		if attempt == 0 {
			duringChoice = time.Now()
			time.Sleep(time.Until(boundary) + 5*time.Millisecond)
		}
	}
	afterHandoffSealWrite = func(attempt int, boundary time.Time) {
		commits = append(commits, time.Now())
		if attempt == 0 {
			afterFirstCommit = commits[0]
		}
	}
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Fatalf("a boundary that its write outlasted was accepted (attempts %d)", attempts)
	}
	fence := readFenceFile(t, poolDir, lease.LeaseID)
	if last := commits[len(commits)-1]; fence.Boundary.Before(last) {
		t.Fatalf("boundary %v is before the commit of the write that recorded it (%v)", fence.Boundary, last)
	}
	for name, started := range map[string]time.Time{"while the boundary was chosen": duringChoice, "after the first commit": afterFirstCommit} {
		if err := bulkStartedAt(poolDir, path, started); !errors.Is(err, ErrHandedOffEntry) {
			t.Fatalf("a run that started %s was admitted: %v", name, err)
		}
	}
}

func TestRecoveredHandoffBoundaryCoversTheCommitOfTheFirstWrite(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	clearHandoffSeams(t)
	var committed time.Time
	afterHandoffSealWrite = func(attempt int, boundary time.Time) { committed = time.Now() }
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	fence := readFenceFile(t, poolDir, lease.LeaseID)
	if !fence.Boundary.After(committed) {
		t.Fatalf("boundary %v is not after the commit that recorded it (%v)", fence.Boundary, committed)
	}
	if err := bulkStartedAt(poolDir, path, committed); !errors.Is(err, ErrHandedOffEntry) {
		t.Fatalf("a run that started right after the commit was admitted: %v", err)
	}
}

func TestRecoveredHandoffReturnsOnlyAfterItsBoundary(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	returned := time.Now()
	fence := readFenceFile(t, poolDir, lease.LeaseID)
	// A run that starts after the command has returned must be ordinary.
	if returned.Before(fence.Boundary) {
		t.Fatalf("the command returned at %v, before its boundary %v", returned, fence.Boundary)
	}
	if err := bulkStartedAt(poolDir, path, returned); err != nil {
		t.Fatalf("a run that started after the command returned was refused: %v", err)
	}
}

func TestRecoveredHandoffLeavesFencePendingWhenItCannotSeal(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	clearHandoffSeams(t)
	afterHandoffSealWrite = func(attempt int, boundary time.Time) {
		time.Sleep(time.Until(boundary) + time.Millisecond)
	}
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err == nil || !strings.Contains(err.Error(), "bulk returns leave this slot alone") {
		t.Fatalf("a boundary that never outlasted its write was accepted: %v", err)
	}
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if wt := state.Worktrees[0]; !wt.Leased || wt.LeaseHolder != "real-task" || wt.LeaseID == "" {
		t.Fatalf("the committed holder change was lost: %+v", wt)
	}
	fence := readFenceFile(t, poolDir, state.Worktrees[0].LeaseID)
	if fence.Boundary.Year() < 9000 {
		t.Fatalf("the fence was left with a boundary its write outlasted: %+v (lease %+v)", fence, lease)
	}
	if err := bulkStartedAt(poolDir, path, time.Now().Add(time.Hour)); !errors.Is(err, ErrHandedOffEntry) {
		t.Fatalf("a bulk return was admitted past an unsealed fence: %v", err)
	}
}

// legacyEntry and legacyState carry exactly the fields of the state file as the
// base of this change (754b8b0) knew them. Reading and writing through them is
// what an older binary does to the shared state file: every other field is dropped.
type legacyEntry struct {
	Name                string    `json:"name"`
	Path                string    `json:"path"`
	CreatedAt           time.Time `json:"created_at"`
	Destroying          bool      `json:"destroying,omitempty"`
	OwnerPID            int32     `json:"owner_pid,omitempty"`
	OwnerStartedAt      int64     `json:"owner_started_at,omitempty"`
	Leased              bool      `json:"leased,omitempty"`
	LeaseID             string    `json:"lease_id,omitempty"`
	LeaseHolder         string    `json:"lease_holder,omitempty"`
	LeasedAt            time.Time `json:"leased_at,omitempty,omitzero"`
	BaseBranch          string    `json:"base_branch,omitempty"`
	SeededPaths         []string  `json:"seeded_paths,omitempty"`
	SeedInventoryKnown  bool      `json:"seed_inventory_known,omitempty"`
	SeedInventoryDigest string    `json:"seed_inventory_digest,omitempty"`
	SeedBackend         string    `json:"seed_backend,omitempty"`
	SeedAuthIdentity    string    `json:"seed_auth_identity,omitempty"`
	RecoveryError       string    `json:"recovery_error,omitempty"`
	RecoveryReason      string    `json:"recovery_reason,omitempty"`
}

type legacyState struct {
	Version   int           `json:"version,omitempty"`
	Worktrees []legacyEntry `json:"worktrees"`
}

// olderWriterRewrite is one rewrite of the state file by an older binary, with a
// probe field that proves the rewrite drops what it does not know.
func olderWriterRewrite(t *testing.T, poolDir string) {
	t.Helper()
	data, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["worktrees"].([]any)[0].(map[string]any)["field_an_older_binary_does_not_know"] = "x"
	probed, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFilePath(poolDir), probed, 0o644); err != nil {
		t.Fatal(err)
	}
	var old legacyState
	if err := json.Unmarshal(probed, &old); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(stateFilePath(poolDir), out, 0o644); err != nil {
		t.Fatal(err)
	}
	if rewritten, _ := os.ReadFile(stateFilePath(poolDir)); strings.Contains(string(rewritten), "field_an_older_binary_does_not_know") {
		t.Fatal("the simulated older writer kept a field it does not know")
	}
}

func TestRecoveredHandoffFenceSurvivesAnOlderWritersRewrite(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	fence := readFenceFile(t, poolDir, lease.LeaseID)
	olderWriterRewrite(t, poolDir)
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if wt := state.Worktrees[0]; !wt.Leased || wt.LeaseID != lease.LeaseID || wt.LeaseHolder != "real-task" {
		t.Fatalf("the rewrite changed the lease: %+v", wt)
	}
	if err := bulkStartedAt(poolDir, path, fence.Boundary.Add(-time.Hour)); !errors.Is(err, ErrHandedOffEntry) {
		t.Fatalf("a run that began before the handoff was admitted after an older writer's rewrite: %v", err)
	}
	if err := bulkStartedAt(poolDir, path, fence.Boundary.Add(time.Nanosecond)); err != nil {
		t.Fatalf("a run that began after the handoff was refused after an older writer's rewrite: %v", err)
	}
}

func TestRecoveredHandoffFenceThatCannotBeReadRefusesBulkReturns(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, fencePath, leaseID string){
		"garbage": func(t *testing.T, fencePath, _ string) {
			if err := os.WriteFile(fencePath, []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"another lease": func(t *testing.T, fencePath, _ string) {
			data, _ := json.Marshal(handoffFence{LeaseID: "someone-else", Boundary: time.Unix(1, 0)})
			if err := os.WriteFile(fencePath, data, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"not a file": func(t *testing.T, fencePath, _ string) {
			if err := os.Remove(fencePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(fencePath, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			poolDir, path := handoffFixtureSlot(t)
			lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
			if err != nil {
				t.Fatal(err)
			}
			damage(t, handoffFencePath(poolDir, lease.LeaseID), lease.LeaseID)
			if err := bulkStartedAt(poolDir, path, time.Now().Add(time.Hour)); !errors.Is(err, ErrHandedOffEntry) {
				t.Fatalf("a bulk return was admitted past a fence it could not read: %v", err)
			}
		})
	}
}

func TestRecoveredHandoffRemovesOnlyOrphanedFences(t *testing.T) {
	poolDir, path := handoffFixtureSlot(t)
	orphan := handoffFencePath(poolDir, "0123456789abcdef")
	if err := writeHandoffFence(poolDir, "0123456789abcdef", handoffPending); err != nil {
		t.Fatal(err)
	}
	bystander := filepath.Join(poolDir, handoffFencePrefix+"notes.txt")
	if err := os.WriteFile(bystander, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	lease, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an orphaned fence was kept: %v", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Fatalf("a file that is not a fence was removed: %v", err)
	}
	if _, err := os.Stat(handoffFencePath(poolDir, lease.LeaseID)); err != nil {
		t.Fatalf("the live fence was removed: %v", err)
	}
}

func TestHandoffFenceNameRefusesUnsafeLeaseIdentities(t *testing.T) {
	for _, id := range []string{"", "..", "a/b", `a\b`, "a b", "a.b", strings.Repeat("a", 129)} {
		if name := handoffFenceName(id); name != "" {
			t.Fatalf("handoffFenceName(%q) = %q, want none", id, name)
		}
	}
	if name := handoffFenceName("0a1B-_z"); name != handoffFencePrefix+"0a1B-_z"+handoffFenceSuffix {
		t.Fatalf("handoffFenceName = %q", name)
	}
}

func TestRecoveredHandoffKeepsTheFenceOfAnotherLiveHandoff(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 2)
	quarantine(t, poolDir, "1", "2")
	first, err := HandoffRecoveredLease(poolDir, paths[0], RecoveredLeaseHolder, "first-task")
	if err != nil {
		t.Fatal(err)
	}
	firstFence := readFenceFile(t, poolDir, first.LeaseID)
	second, err := HandoffRecoveredLease(poolDir, paths[1], RecoveredLeaseHolder, "second-task")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFenceFile(t, poolDir, first.LeaseID); got != firstFence {
		t.Fatalf("a later handoff changed the first slot's fence: %+v -> %+v", firstFence, got)
	}
	readFenceFile(t, poolDir, second.LeaseID)
	if err := bulkStartedAt(poolDir, paths[0], firstFence.Boundary.Add(-time.Hour)); !errors.Is(err, ErrHandedOffEntry) {
		t.Fatalf("the first slot lost its fence to the second handoff: %v", err)
	}
}
