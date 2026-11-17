package pool

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Slot locks fence acquisition's checkout work from reservation writers. They
// are always taken nonblocking under the state lock, never waited on there.
// Keep lock files permanently: unlinking one can split exclusion across inodes.
func lockSlot(poolDir, name string) (*os.File, error) {
	dir := filepath.Join(poolDir, ".treehouse-slot-locks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.lock", sha256.Sum256([]byte(name))))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := tryLockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("worktree %s has an acquisition in progress: %w", name, err)
	}
	return f, nil
}

// Called only with the state lock held. A writer keeps that lock until its
// action finishes, so a successful probe needs no retained slot lock: a new
// acquisition cannot take the slot fence until the writer releases state.
func slotWritable(poolDir, name string) error {
	f, err := lockSlot(poolDir, name)
	if err != nil {
		return err
	}
	return f.Close()
}

type acquisitionClaim struct {
	poolDir string
	entry   WorktreeEntry
	fence   *os.File
}

func (c *acquisitionClaim) close() { c.fence.Close() }

// Callers omit a clean first reuse and empty-pool creation. Every other
// completion is reported immediately, once per path. Fixed reasons never
// include Git output or checkout contents.
var candidateProgressPathEscaper = strings.NewReplacer("\\", "\\\\", "\r", "\\r", "\n", "\\n")

func newCandidateProgress(writer io.Writer) func(string, string, string) {
	if writer == nil {
		writer = os.Stderr
	}
	var reported map[string]struct{}
	return func(path, decision, reason string) {
		if path == "" {
			return
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return
		}
		if _, exists := reported[absolute]; exists {
			return
		}
		if reported == nil {
			reported = make(map[string]struct{})
		}
		reported[absolute] = struct{}{}
		escaped := candidateProgressPathEscaper.Replace(absolute)
		fmt.Fprintf(writer, "treehouse: checked %s: %s: %s\n", escaped, decision, reason)
	}
}

// save merges just the fenced slot into freshly read state. Never publish a
// stale pool snapshot over reservations acquired by another get.
func (c *acquisitionClaim) save(local State) error {
	return WithStateLock(c.poolDir, func() error {
		state, err := ReadState(c.poolDir)
		if err != nil {
			return err
		}
		for i, wt := range state.Worktrees {
			if wt.Name != c.entry.Name {
				continue
			}
			if !reflect.DeepEqual(wt, c.entry) {
				return fmt.Errorf("worktree %s reservation changed during acquisition", wt.Name)
			}
			if len(local.Worktrees) == 0 {
				state.Worktrees = append(state.Worktrees[:i], state.Worktrees[i+1:]...)
			} else {
				state.Worktrees[i] = local.Worktrees[0]
			}
			if err := persistState(c.poolDir, state); err != nil {
				return err
			}
			// ReadState authenticates inventories and WriteState stamps their digest.
			persisted, err := ReadState(c.poolDir)
			if err != nil {
				return err
			}
			for _, entry := range persisted.Worktrees {
				if entry.Name == c.entry.Name {
					c.entry = entry
					break
				}
			}
			return nil
		}
		return fmt.Errorf("worktree %s disappeared during acquisition", c.entry.Name)
	})
}

func claimFresh(repoRoot, poolDir string, poolSize, firstSlot int, opts acquireOptions) (*acquisitionClaim, error) {
	var claim *acquisitionClaim
	err := WithStateLock(poolDir, func() error {
		state, err := ReadState(poolDir)
		if err != nil {
			return err
		}
		state, err = healState(poolDir, state)
		if err != nil {
			return err
		}
		if len(state.Worktrees) >= poolSize {
			return nil
		}
		slot := nextSlotNumber(state)
		// The default layout selected its new name before foreign reclamation.
		// Keep that floor, while still avoiding names another get has claimed.
		if opts.worktreePath == "" && slot < firstSlot {
			slot = firstSlot
		}
		name := strconv.Itoa(slot)
		path, err := resolveWorktreePath(repoRoot, poolDir, name, opts.worktreePath, opts.uniqueLeaf)
		if err != nil {
			return err
		}
		if opts.worktreePath != "" {
			name, path, err = freeTemplatedSlot(repoRoot, poolDir, state, poolSize, opts)
			if err != nil {
				return err
			}
		} else if err := reclaimEmptySlotDirectory(path); err != nil {
			return err
		}
		fence, err := lockSlot(poolDir, name)
		if err != nil {
			return err
		}
		entry := WorktreeEntry{Name: name, Path: path, CreatedAt: time.Now(), BaseBranch: opts.baseBranch}
		// Fresh creation can run checkout hooks and seed ignored files before
		// publication. Its existing provisional lease must survive a crash or
		// failed later write; it is not a published owner/consumer lease yet.
		setSeedInventory(&entry, nil, false)
		entry.Leased = true
		entry.LeaseHolder = acquisitionIncompleteLeaseHolder
		entry.LeasedAt = time.Now()
		state.Worktrees = append(state.Worktrees, entry)
		if err := persistState(poolDir, state); err != nil {
			fence.Close()
			return err
		}
		persisted, err := ReadState(poolDir)
		if err != nil {
			fence.Close()
			return err
		}
		for _, wt := range persisted.Worktrees {
			if wt.Name == name {
				entry = wt
				break
			}
		}
		claim = &acquisitionClaim{poolDir: poolDir, entry: entry, fence: fence}
		return nil
	})
	return claim, err
}

func claimCandidate(poolDir string, observed WorktreeEntry) (*acquisitionClaim, error) {
	var claim *acquisitionClaim
	err := WithStateLock(poolDir, func() error {
		state, err := ReadState(poolDir)
		if err != nil {
			return err
		}
		for i, wt := range state.Worktrees {
			if wt.Name != observed.Name {
				continue
			}
			if !reflect.DeepEqual(wt, observed) || wt.Destroying || wt.Leased || ownerAlive(wt) {
				return nil
			}
			fence, err := lockSlot(poolDir, wt.Name)
			if err != nil {
				return nil
			}
			// Screening has not selected this copy yet. Even --lease must heal
			// like an interrupted ordinary get if it dies before reset.
			if err := reserveOwner(&state.Worktrees[i]); err != nil {
				fence.Close()
				return err
			}
			if err := persistState(poolDir, state); err != nil {
				fence.Close()
				return err
			}
			persisted, err := ReadState(poolDir)
			if err != nil {
				fence.Close()
				return err
			}
			for _, entry := range persisted.Worktrees {
				if entry.Name == wt.Name {
					claim = &acquisitionClaim{poolDir: poolDir, entry: entry, fence: fence}
					return nil
				}
			}
			fence.Close()
			return fmt.Errorf("worktree %s disappeared after claim", wt.Name)
		}
		return nil
	})
	return claim, err
}
