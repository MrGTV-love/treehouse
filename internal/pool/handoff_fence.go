package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A recovered-lease handoff leaves a fence beside the state file so a `return
// --all` that began before the handoff completed never releases the slot the
// handoff produced.
//
// The fence is its own small file, one per minted lease, and not a field of the
// state file. An older treehouse binary rewrites the whole state file and drops
// every field it does not know, so a field there would vanish on its next write.
// A file named for the lease survives that, and it cannot be mistaken for a slot:
// pool scans only follow directories.
//
// Order is what makes it crash-safe. The fence is written pending (a boundary no
// run starts before) before the holder change is committed, and is sealed with a
// real boundary only after. A crash at any point leaves it pending, so every bulk
// return leaves the slot alone until it is returned by name.
//
// A sealed boundary must be at or after the commit of the write that records it.
// A boundary read before its own write would let a run that started in between
// pass, though it began before the handoff finished. So the boundary is set in
// the future of the write, and the write is checked against the clock afterwards;
// a write that outlasted its boundary is redone with a larger one.

const (
	handoffFencePrefix = "treehouse-handoff-"
	handoffFenceSuffix = ".fence"

	// handoffMinMargin is the least distance a sealed boundary is placed ahead of
	// the clock. The margin is that or four times the last fence write, whichever
	// is larger, and doubles on every redo.
	handoffMinMargin = 10 * time.Millisecond
	// handoffSealAttempts bounds the redo loop. The last margin is 128 times the first.
	handoffSealAttempts = 8
)

// handoffPending is the boundary of a fence that is not sealed: later than any
// real process start.
var handoffPending = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

// Test seams. They are nil in production.
var (
	// beforeHandoffHolderWrite runs after the pending fence is written and before
	// the holder change is committed.
	beforeHandoffHolderWrite func()
	// afterHandoffHolderWrite runs between the committed holder change and the
	// seal of the fence.
	afterHandoffHolderWrite func()
	// beforeHandoffSealWrite runs after a boundary is chosen and before it is written.
	beforeHandoffSealWrite func(attempt int, boundary time.Time)
	// afterHandoffSealWrite runs right after a boundary is written and before it
	// is checked against the clock.
	afterHandoffSealWrite func(attempt int, boundary time.Time)
)

type handoffFence struct {
	LeaseID  string    `json:"lease_id"`
	Boundary time.Time `json:"boundary"`
}

// handoffFenceName reports the file name for a lease, or "" when the identity is
// not one treehouse mints (and so cannot name a safe file).
func handoffFenceName(leaseID string) string {
	if leaseID == "" || len(leaseID) > 128 {
		return ""
	}
	for _, r := range leaseID {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return ""
		}
	}
	return handoffFencePrefix + leaseID + handoffFenceSuffix
}

func handoffFencePath(poolDir, leaseID string) string {
	name := handoffFenceName(leaseID)
	if name == "" {
		return ""
	}
	return filepath.Join(poolDir, name)
}

func writeHandoffFence(poolDir, leaseID string, boundary time.Time) error {
	path := handoffFencePath(poolDir, leaseID)
	if path == "" {
		return fmt.Errorf("lease identity %q cannot name a handoff fence", leaseID)
	}
	data, err := json.Marshal(handoffFence{LeaseID: leaseID, Boundary: boundary.UTC()})
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o644)
}

// removeHandoffFence drops a lease's fence once the lease is over. A failure
// leaves an inert file, because no later lease carries the same identity.
func removeHandoffFence(poolDir, leaseID string) {
	if path := handoffFencePath(poolDir, leaseID); path != "" {
		_ = os.Remove(path)
	}
}

// removeOrphanHandoffFences drops the fences of leases the state no longer holds,
// such as those left by a slot an older binary returned.
func removeOrphanHandoffFences(poolDir string, state State) {
	live := map[string]bool{}
	for _, wt := range state.Worktrees {
		if name := handoffFenceName(wt.LeaseID); name != "" {
			live[name] = true
		}
	}
	entries, err := os.ReadDir(poolDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, handoffFencePrefix) || !strings.HasSuffix(name, handoffFenceSuffix) || live[name] {
			continue
		}
		_ = os.Remove(filepath.Join(poolDir, name))
	}
}

// sealHandoffFence replaces the pending fence of leaseID with a boundary that is
// at or after the commit of the write that records it, and returns that boundary.
// firstWrite is how long the pending write took, which sizes the first margin.
// Any failure leaves the fence pending.
func sealHandoffFence(poolDir, leaseID string, firstWrite time.Duration) (boundary time.Time, err error) {
	defer func() {
		if err != nil {
			_ = writeHandoffFence(poolDir, leaseID, handoffPending)
		}
	}()
	margin := max(handoffMinMargin, 4*firstWrite)
	for attempt := 0; attempt < handoffSealAttempts; attempt++ {
		boundary = time.Now().Add(margin)
		if beforeHandoffSealWrite != nil {
			beforeHandoffSealWrite(attempt, boundary)
		}
		if err = writeHandoffFence(poolDir, leaseID, boundary); err != nil {
			return time.Time{}, err
		}
		if afterHandoffSealWrite != nil {
			afterHandoffSealWrite(attempt, boundary)
		}
		if time.Now().Before(boundary) {
			return boundary, nil
		}
		margin *= 2
	}
	return time.Time{}, fmt.Errorf("the fence write took longer than %v on each of %d attempts", margin/2, handoffSealAttempts)
}

// refuseFencedBulkReturn is the check a bulk return makes under the state lock
// for a leased worktree. It returns ErrHandedOffEntry when a handoff fence names
// the current lease and the run started at or before the fence boundary, and also
// when a fence file exists but cannot be read, since a run that cannot tell must
// leave the slot alone.
func refuseFencedBulkReturn(poolDir string, wt WorktreeEntry, startedAt time.Time) error {
	path := handoffFencePath(poolDir, wt.LeaseID)
	if !wt.Leased || path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: worktree %s has a handoff fence that cannot be read (%v); return it by name", ErrHandedOffEntry, wt.Path, err)
	}
	var fence handoffFence
	if err := json.Unmarshal(data, &fence); err != nil || fence.LeaseID != wt.LeaseID {
		return fmt.Errorf("%w: worktree %s has a handoff fence that is not valid; return it by name", ErrHandedOffEntry, wt.Path)
	}
	if fence.Boundary.Before(startedAt) {
		return nil
	}
	return fmt.Errorf("%w: worktree %s was handed to %q by a recovered-lease handoff that completed after this run began", ErrHandedOffEntry, wt.Path, wt.LeaseHolder)
}
