package pool

import (
	"bytes"
	"encoding/json"
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

func TestRecoveredEntryNamesIsASideEffectFreeSnapshot(t *testing.T) {
	repo, poolDir := setupLocalRepo(t)
	idleSlots(t, repo, poolDir, 2)
	quarantine(t, poolDir, "1")
	names, err := RecoveredEntryNames(poolDir)
	if err != nil || len(names) != 1 || !names["1"] {
		t.Fatalf("snapshot = %v, %v; want only slot 1", names, err)
	}
	stateFile := stateFilePath(poolDir)
	before, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(stateFile); !bytes.Equal(before, after) {
		t.Fatal("snapshot changed the state file")
	}
	// A corrupt file yields no names and no error: the listing that follows
	// recovers every slot as recovered.
	if err := os.WriteFile(stateFile, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if names, err := RecoveredEntryNames(poolDir); err != nil || names != nil {
		t.Fatalf("corrupt snapshot = %v, %v", names, err)
	}
	// An unreadable entry is an error, so the caller releases nothing.
	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoveredEntryNames(poolDir); err == nil {
		t.Fatal("unreadable state produced no error")
	}
	if present, err := StateFileEntryExists(poolDir); err != nil || !present {
		t.Fatalf("StateFileEntryExists = %v, %v", present, err)
	}
	if present, err := StateFileEntryExists(filepath.Join(poolDir, "nowhere")); err != nil || present {
		t.Fatalf("StateFileEntryExists(absent) = %v, %v", present, err)
	}
}
