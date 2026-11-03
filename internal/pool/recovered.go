package pool

import (
	"os"
	"path/filepath"
)

// A recovered entry (see RecoveredLeaseHolder) is a persistent quarantine:
// nothing in this package frees it, moves its contents, or clears its lease
// except an explicit return naming its slot. What remains here only REPORTS
// the retained backup folders that the automatic recovery of earlier treehouse
// versions moved untracked files into.

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

// recoveredQuarantineReason is what status says about a recovered lease that
// carries no reason of its own. Treehouse no longer examines a recovered entry
// to decide whether to free it, so the notice is the same for every one.
const recoveredQuarantineReason = "treehouse never frees a recovered slot on its own; it is held until it is returned by name"

// recoveryReasonOf is the reason status reports for wt: the one an earlier
// version recorded, else the persistent-quarantine notice for a recovered lease.
func recoveryReasonOf(wt WorktreeEntry) string {
	if wt.RecoveryReason != "" || !wt.Leased || wt.LeaseHolder != RecoveredLeaseHolder {
		return wt.RecoveryReason
	}
	return recoveredQuarantineReason
}
