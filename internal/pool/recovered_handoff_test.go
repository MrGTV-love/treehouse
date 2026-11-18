package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveredHandoffKeepsSiblingAndInvalidatesOldIdentity(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 2)
	quarantine(t, poolDir, "1", "2")
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	state.Worktrees[0].LeaseID = "old-recovered-identity"
	state.Worktrees[0].RecoveryReason = "old recovery reason"
	if err := WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	sibling, err := json.Marshal(state.Worktrees[1])
	if err != nil {
		t.Fatal(err)
	}
	lease, err := HandoffRecoveredLease(poolDir, paths[0], RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	got := after.Worktrees[0]
	if !got.Leased || got.LeaseHolder != "real-task" || got.LeaseID == "" || got.LeaseID == "old-recovered-identity" || got.LeasedAt.IsZero() || got.RecoveryReason != "" || !got.SeedInventoryKnown || len(got.SeededPaths) != 0 {
		t.Fatalf("not an ordinary lease: %+v", got)
	}
	siblingAfter, err := json.Marshal(after.Worktrees[1])
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != got.LeaseID || !bytes.Equal(sibling, siblingAfter) {
		t.Fatalf("identity or sibling changed: %+v", after)
	}
	// A delayed owner acting on the earlier recovered identity must not be able
	// to release the newly acknowledged task's lease.
	old := "old-recovered-identity"
	if err := ReleaseConditional(poolDir, paths[0], "", ReleasePreconditions{ExpectedLeaseID: &old}, nil); err == nil {
		t.Fatal("old identity released new holder")
	}
}

func TestRecoveredHandoffChecksHolderAfterWaitingForLock(t *testing.T) {
	_, poolDir, path := recoveredFixture(t)
	var result chan error
	var before []byte
	if err := WithStateLock(poolDir, func() error {
		started := make(chan struct{})
		result = make(chan error, 1)
		go func() {
			close(started)
			_, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "late-task")
			result <- err
		}()
		<-started
		state, err := ReadState(poolDir)
		if err != nil {
			return err
		}
		if err := markAcquired(&state.Worktrees[0], acquireOptions{lease: true, leaseHolder: "winning-task"}); err != nil {
			return err
		}
		setSeedInventory(&state.Worktrees[0], nil, true)
		if err := WriteState(poolDir, state); err != nil {
			return err
		}
		before, err = os.ReadFile(stateFilePath(poolDir))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "not a recovered lease") {
		t.Fatalf("late handoff: %v", err)
	}
	after, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("late handoff changed state: %v", err)
	}
}

func TestRecoveredHandoffRejectsUntrustedInventoryWithoutBlessingSibling(t *testing.T) {
	_, poolDir, path := recoveredFixture(t)
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	state.Worktrees = append(state.Worktrees, WorktreeEntry{Name: "forged", Path: filepath.Join(poolDir, "missing"), SeedInventoryKnown: true, SeededPaths: []string{"private"}, SeedInventoryDigest: "forged"})
	writeRawState(t, poolDir, state)
	before, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandoffRecoveredLease(poolDir, path, RecoveredLeaseHolder, "real-task"); err == nil || !strings.Contains(err.Error(), "invalid state inventory") {
		t.Fatalf("forged inventory: %v", err)
	}
	after, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refusal changed state: %v", err)
	}
}

func TestRecoveredHandoffMarksItsLeaseForBulkReturns(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 1)
	quarantine(t, poolDir, "1")
	began := time.Now()
	lease, err := HandoffRecoveredLease(poolDir, paths[0], RecoveredLeaseHolder, "real-task")
	if err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	wt := state.Worktrees[0]
	if wt.HandoffLeaseID != lease.LeaseID || wt.HandedOffAt == nil || wt.HandedOffAt.Before(began) || wt.HandedOffAt.Year() > 3000 {
		t.Fatalf("handoff marker = %q %v, want the minted lease and a real completion time", wt.HandoffLeaseID, wt.HandedOffAt)
	}
	completed := *wt.HandedOffAt
	for name, started := range map[string]time.Time{"before": completed.Add(-time.Second), "tie": completed} {
		err := ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{BulkStartedAt: &started}, nil)
		if !errors.Is(err, ErrHandedOffEntry) {
			t.Fatalf("a run that started %s the completion was not refused: %v", name, err)
		}
	}
	after := completed.Add(time.Nanosecond)
	if err := ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{BulkStartedAt: &after}, nil); err != nil {
		t.Fatalf("a run that started after the completion was refused: %v", err)
	}
	// A named release is never fenced, and a later acquisition mints a new lease, so
	// the old marker is inert for it.
	early := completed.Add(-time.Hour)
	if err := ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{}, nil); err != nil {
		t.Fatalf("a named release was refused: %v", err)
	}
	if err := Release(poolDir, paths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := LeaseExisting(poolDir, "1", "another"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{BulkStartedAt: &early}, nil); err != nil {
		t.Fatalf("a later ordinary acquisition was fenced by the old handoff: %v", err)
	}
}

func TestRecoveredHandoffIsPendingBetweenItsTwoWrites(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 1)
	quarantine(t, poolDir, "1")
	var between WorktreeEntry
	afterHandoffHolderWrite = func() {
		state, err := ReadState(poolDir)
		if err != nil {
			t.Error(err)
			return
		}
		between = state.Worktrees[0]
	}
	t.Cleanup(func() { afterHandoffHolderWrite = nil })
	if _, err := HandoffRecoveredLease(poolDir, paths[0], RecoveredLeaseHolder, "real-task"); err != nil {
		t.Fatal(err)
	}
	if !between.Leased || between.LeaseHolder != "real-task" || between.HandoffLeaseID == "" || between.HandoffLeaseID != between.LeaseID ||
		between.HandedOffAt == nil || between.HandedOffAt.Year() < 9000 {
		t.Fatalf("between the writes the entry was %+v (HandedOffAt %v); want leased to the new holder with a pending marker", between, between.HandedOffAt)
	}
}

func TestRecoveredHandoffPendingMarkerRefusesEveryBulkReturn(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 1)
	quarantine(t, poolDir, "1")
	if _, err := HandoffRecoveredLease(poolDir, paths[0], RecoveredLeaseHolder, "real-task"); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	// What a crash between the handoff's two writes leaves behind.
	pending := handoffPending
	state.Worktrees[0].HandedOffAt = &pending
	if err := WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	if err := ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{BulkStartedAt: &now}, nil); !errors.Is(err, ErrHandedOffEntry) {
		t.Fatalf("a bulk return was admitted while the handoff was pending: %v", err)
	}
}

func TestStateFileEntryExistsDoesNotFollowOrCreate(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	idleSlots(t, repo, poolDir, 1)
	stateFile := stateFilePath(poolDir)
	if present, err := StateFileEntryExists(poolDir); err != nil || !present {
		t.Fatalf("StateFileEntryExists = %v, %v", present, err)
	}
	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(poolDir, "missing-target"), stateFile); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if present, err := StateFileEntryExists(poolDir); err != nil || !present {
		t.Fatalf("a dangling symlink is an entry: %v, %v", present, err)
	}
	nowhere := filepath.Join(poolDir, "nowhere")
	if present, err := StateFileEntryExists(nowhere); err != nil || present {
		t.Fatalf("StateFileEntryExists(absent) = %v, %v", present, err)
	}
	if _, err := os.Lstat(nowhere); err == nil {
		t.Fatal("the check created a directory")
	}
}
