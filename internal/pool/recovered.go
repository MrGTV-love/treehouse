package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/kunchenguid/treehouse/v3/internal/pathidentity"
)

// HandoffRecoveredLease replaces a recovered holder in place. It does not run
// recovery or healing: the caller must first inspect a persisted quarantine and
// verify who owns its contents. The exact path and expected holder are checked
// again under the state lock. No worktree operation or intermediate release is
// performed, and every refusal leaves the state file untouched.
func HandoffRecoveredLease(poolDir, path, expectedHolder, holder string) (LeaseInfo, error) {
	if strings.TrimSpace(holder) == "" {
		return LeaseInfo{}, fmt.Errorf("recovered handoff requires a non-empty lease holder")
	}
	if holder == RecoveredLeaseHolder {
		return LeaseInfo{}, fmt.Errorf("new lease holder must not be the recovered holder")
	}
	if strings.ContainsFunc(holder, unicode.IsControl) {
		return LeaseInfo{}, fmt.Errorf("lease holder must not contain control characters")
	}
	if expectedHolder != RecoveredLeaseHolder {
		return LeaseInfo{}, fmt.Errorf("lease holder does not match the required recovered holder")
	}

	var lease LeaseInfo
	err := WithStateLock(poolDir, func() error {
		// ReadState intentionally recovers corrupt JSON and unauthenticated
		// entries. A handoff must instead refuse those inputs, not manufacture
		// the precondition it was asked to compare.
		data, err := os.ReadFile(stateFilePath(poolDir))
		if err != nil {
			return fmt.Errorf("reading handoff state: %w", err)
		}
		var state State
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("invalid handoff state; inspect with treehouse status first: %w", err)
		}
		if state.Version != stateVersion {
			return fmt.Errorf("handoff requires state version %d; inspect with treehouse status first", stateVersion)
		}
		key, err := readStateKey(poolDir)
		if err != nil {
			return fmt.Errorf("reading handoff state key: %w", err)
		}
		for _, wt := range state.Worktrees {
			if wt.SeedInventoryKnown && !validSeedInventoryDigest(key, wt) {
				return fmt.Errorf("invalid state inventory for worktree %s; inspect with treehouse status first", wt.Name)
			}
		}
		resolved, err := pathidentity.Existing(path)
		if err != nil {
			return fmt.Errorf("worktree %s is not managed by treehouse: %w", path, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		entryCount := len(state.Worktrees)
		state, index, err := normalizeStatePaths(state)
		if err != nil {
			return err
		}
		if len(state.Worktrees) != entryCount {
			return fmt.Errorf("handoff state contains duplicate worktree aliases; inspect with treehouse status first")
		}
		i, ok := index.find(resolved, info)
		if !ok {
			return fmt.Errorf("worktree %s is not managed by treehouse", path)
		}
		wt := &state.Worktrees[i]
		if wt.Destroying {
			return fmt.Errorf("worktree %s is being destroyed", wt.Name)
		}
		if !wt.Leased || wt.LeaseHolder != RecoveredLeaseHolder {
			return fmt.Errorf("worktree %s is not a recovered lease (holder: %q)", wt.Name, wt.LeaseHolder)
		}
		if wt.LeaseHolder != expectedHolder {
			return fmt.Errorf("lease holder does not match for worktree %s", wt.Name)
		}
		if err := markAcquired(wt, acquireOptions{lease: true, leaseHolder: holder}); err != nil {
			return err
		}
		wt.RecoveryReason = ""
		// Recovery cannot reconstruct the old seed inventory. Trust an empty
		// cleanup list, not any of the unknown files in the live copy. They stay
		// on disk, including when this ordinary lease is eventually returned.
		if !wt.SeedInventoryKnown {
			setSeedInventory(wt, nil, true)
		}
		if err := WriteState(poolDir, state); err != nil {
			return err
		}
		lease = leaseInfoFromEntry(*wt, wt.BaseBranch)
		return nil
	})
	return lease, err
}

// StateFileEntryExists reports whether the pool state file has a directory entry,
// without following it. A dangling symlink is an entry that cannot be read, so it
// counts as present and the caller reports the read error; only an absent entry
// means "not a pool". It has no side effect, unlike taking the state lock.
func StateFileEntryExists(poolDir string) (bool, error) {
	_, err := os.Lstat(stateFilePath(poolDir))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// RecoveredEntryNames is the snapshot a bulk return takes as its very first
// read of the pool. It lists, by entry name, every entry that is recovered at
// that instant, from one lock-free read of the state file that never recovers,
// heals or writes anything. `return --all` never releases an entry named here,
// even when a handoff gives it an ordinary lease before the run lists the pool:
// the run began before that handoff completed. A read that fails for a reason
// other than "no file" is an error, so the caller releases nothing. A file that
// is missing, does not parse or has an older version yields no names because
// the listing that follows recovers every slot as recovered and the run skips
// each of them.
func RecoveredEntryNames(poolDir string) (map[string]bool, error) {
	data, err := ReadFileShared(stateFilePath(poolDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading recovered-entry snapshot: %w", err)
	}
	var state State
	if json.Unmarshal(data, &state) != nil || state.Version != stateVersion {
		return nil, nil
	}
	names := map[string]bool{}
	for _, wt := range state.Worktrees {
		if wt.Leased && wt.LeaseHolder == RecoveredLeaseHolder {
			names[wt.Name] = true
		}
	}
	return names, nil
}

// A recovered entry (see RecoveredLeaseHolder) is a persistent quarantine:
// nothing in this package frees it, moves its contents, or clears its lease
// except an explicit return naming its slot. HandoffRecoveredLease can instead
// acknowledge a verified holder without clearing the lease or touching the slot.
// The helpers below only REPORT retained backup folders that automatic recovery
// in earlier treehouse versions moved untracked files into.

// recoveryBackupDir is the one backup folder that the automatic recovery of
// earlier treehouse versions used for a slot. It is derived rather than recorded.
// This version never creates or writes one; it only lets status keep
// reporting a folder an earlier version left beside the pool.
func recoveryBackupDir(poolDir, name string) string {
	return filepath.Join(filepath.Dir(poolDir), "treehouse-recovered-backup-"+filepath.Base(poolDir)+"-"+name)
}

// recoveryBackup reports the slot's recovery backup folder when it is a real
// directory that holds anything, and "" otherwise.
func recoveryBackup(poolDir, name string) string {
	backup := recoveryBackupDir(poolDir, name)
	if info, err := os.Lstat(backup); err != nil || !info.IsDir() {
		return ""
	}
	entries, err := os.ReadDir(backup)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return backup
}

// recoveredQuarantineReason is what status says about every recovered lease.
// Treehouse no longer examines a recovered entry to decide whether to free it,
// so the notice is the same for every one.
const recoveredQuarantineReason = "treehouse never frees a recovered slot on its own; it is held until it is returned by name"

// recoveryReasonOf is the reason status reports for wt: the
// persistent-quarantine notice for a recovered lease, and "" otherwise. A
// reason an earlier version recorded described its own sweep and is never shown.
func recoveryReasonOf(wt WorktreeEntry) string {
	if !wt.Leased || wt.LeaseHolder != RecoveredLeaseHolder {
		return ""
	}
	return recoveredQuarantineReason
}
