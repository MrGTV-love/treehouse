package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/kunchenguid/treehouse/v3/internal/pathidentity"
	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

type statePathIndex struct {
	paths map[string]int
	files []os.FileInfo
}

func (index statePathIndex) find(path string, info os.FileInfo) (int, bool) {
	if i, ok := index.paths[filepath.Clean(path)]; ok {
		return i, true
	}
	if info == nil {
		return 0, false
	}
	for i, known := range index.files {
		if known != nil && os.SameFile(known, info) {
			return i, true
		}
	}
	return 0, false
}

// normalizeStatePaths gives each existing directory one identity after inventory
// authentication. The winning record keeps its original path: HMACs and jj
// workspace/seed registration names bind that spelling. Missing slots retain
// lexical identity for the existing stale-slot cleanup semantics.
func normalizeStatePaths(s State) (State, statePathIndex, error) {
	index := statePathIndex{paths: make(map[string]int, len(s.Worktrees)), files: make([]os.FileInfo, 0, len(s.Worktrees))}
	entries := make([]WorktreeEntry, 0, len(s.Worktrees))
	for _, wt := range s.Worktrees {
		resolved, err := pathidentity.Existing(wt.Path)
		if err != nil && !os.IsNotExist(err) {
			return State{}, statePathIndex{}, fmt.Errorf("resolving worktree identity %s: %w", wt.Path, err)
		}
		identity := filepath.Clean(wt.Path)
		if err == nil {
			identity = resolved
		}
		var info os.FileInfo
		if _, known := index.paths[identity]; err == nil && !known {
			info, err = os.Stat(wt.Path)
			if err != nil {
				return State{}, statePathIndex{}, fmt.Errorf("reading worktree identity %s: %w", wt.Path, err)
			}
		}
		if i, ok := index.find(identity, info); ok {
			merged, ok := mergeAliasEntries(entries[i], wt)
			if !ok {
				// Do not choose a winner when two records carry incompatible
				// ownership or trusted state. Returning an error leaves the
				// persisted records and physical worktree untouched.
				return State{}, statePathIndex{}, fmt.Errorf("conflicting alias records for worktree %s (names %q and %q); ownership and seed inventories were preserved; inspect treehouse-state.json before retrying", identity, entries[i].Name, wt.Name)
			}
			entries[i] = merged
			index.paths[identity] = i
			continue
		}
		index.paths[identity] = len(entries)
		index.files = append(index.files, info)
		entries = append(entries, wt)
	}
	s.Worktrees = entries
	return s, index, nil
}

func mergeAliasEntries(a, b WorktreeEntry) (WorktreeEntry, bool) {
	if a.Name != b.Name {
		return WorktreeEntry{}, false
	}
	if a.Path == b.Path && a.CreatedAt == b.CreatedAt && a.Destroying == b.Destroying &&
		a.OwnerPID == b.OwnerPID && a.OwnerStartedAt == b.OwnerStartedAt &&
		a.Leased == b.Leased && a.LeaseID == b.LeaseID && a.LeaseHolder == b.LeaseHolder && a.LeasedAt == b.LeasedAt &&
		a.BaseBranch == b.BaseBranch && a.SeedInventoryKnown == b.SeedInventoryKnown &&
		a.SeedInventoryDigest == b.SeedInventoryDigest && (a.SeededPaths == nil) == (b.SeededPaths == nil) &&
		slices.Equal(a.SeededPaths, b.SeededPaths) && a.SeedBackend == b.SeedBackend && a.SeedAuthIdentity == b.SeedAuthIdentity &&
		a.RecoveryError == b.RecoveryError && a.RecoveryReason == b.RecoveryReason {
		return a, true
	}
	// A reconstructed alias (including one an older binary auto-freed) has
	// no ownership, requested base or seeded files to contribute. It must not
	// overwrite the authoritative record's lease, owner or trusted inventory.
	aEmpty, bEmpty := emptyAliasRecord(a), emptyAliasRecord(b)
	if aEmpty && bEmpty {
		if a.Path != b.Path {
			registered, err := vcs.RegisteredWorktreePath(a.Path)
			if err == nil {
				if a.SeedInventoryKnown && registered == a.Path && registered != b.Path {
					return a, true
				}
				if b.SeedInventoryKnown && registered == b.Path && registered != a.Path {
					return b, true
				}
			}
		}
		return WorktreeEntry{}, false
	}
	if aEmpty != bEmpty {
		authoritative, alias := a, b
		if aEmpty {
			authoritative, alias = b, a
		}
		if authoritative.Path != alias.Path {
			registered, err := vcs.RegisteredWorktreePath(authoritative.Path)
			if err == nil && registered == alias.Path {
				return WorktreeEntry{}, false
			}
		}
		return authoritative, true
	}
	return WorktreeEntry{}, false
}

func emptyAliasRecord(wt WorktreeEntry) bool {
	return !wt.Destroying && wt.OwnerPID == 0 && wt.OwnerStartedAt == 0 && wt.LeaseID == "" &&
		(!wt.Leased && wt.LeaseHolder == "" || wt.Leased && wt.LeaseHolder == RecoveredLeaseHolder) &&
		wt.BaseBranch == "" && len(wt.SeededPaths) == 0 && wt.SeedBackend == "" && wt.SeedAuthIdentity == "" && wt.RecoveryError == ""
}
