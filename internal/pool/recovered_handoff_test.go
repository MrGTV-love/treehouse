package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// J1 (Main 2761): a bulk return never releases a lease whose identity changed
// after it enumerated it. The identity is the lease id together with the holder,
// compared under the state lock.
func TestRecoveredHandoffChangesTheEnumeratedIdentity(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 1)
	quarantine(t, poolDir, "1")
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	enumerated := reservationOf(state.Worktrees[0])
	if enumerated.LeaseHolder != RecoveredLeaseHolder {
		t.Fatalf("fixture is not recovered: %+v", enumerated)
	}
	if _, err := HandoffRecoveredLease(poolDir, paths[0], RecoveredLeaseHolder, "real-task"); err != nil {
		t.Fatal(err)
	}
	stale := ReleasePreconditions{ExpectedReservation: &enumerated}
	if err := ValidateReleasePreconditions(poolDir, paths[0], stale, nil); !errors.Is(err, ErrLeasePreconditionFailed) {
		t.Fatalf("a release by the identity enumerated before the handoff was not refused: %v", err)
	}
	after, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	current := reservationOf(after.Worktrees[0])
	if current.LeaseID == enumerated.LeaseID || current.LeaseHolder != "real-task" {
		t.Fatalf("the handoff left the identity %+v, enumerated %+v", current, enumerated)
	}
	// A run that enumerates after the handoff holds the new identity and may release.
	if err := ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{ExpectedReservation: &current}, nil); err != nil {
		t.Fatalf("a release by the identity enumerated after the handoff was refused: %v", err)
	}
}

func TestReservationIdentityIncludesTheHolder(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	paths := idleSlots(t, repo, poolDir, 1)
	if _, err := LeaseExisting(poolDir, "1", "first"); err != nil {
		t.Fatal(err)
	}
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	enumerated := reservationOf(state.Worktrees[0])
	// Same lease id, different holder: not the identity that was enumerated.
	state.Worktrees[0].LeaseHolder = "someone-else"
	writeRawState(t, poolDir, state)
	err = ValidateReleasePreconditions(poolDir, paths[0], ReleasePreconditions{ExpectedReservation: &enumerated}, nil)
	if !errors.Is(err, ErrLeasePreconditionFailed) {
		t.Fatalf("a changed holder under the same lease id was not refused: %v", err)
	}
}
