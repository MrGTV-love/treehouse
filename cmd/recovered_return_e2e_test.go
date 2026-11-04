package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const recoveredHolderLiteral = "recovered: state file was corrupt or truncated; verify before reuse"

// A reason an earlier release saved on a recovered entry described that
// release's sweep and may be false now; status text and JSON show the
// persistent-quarantine notice instead.
func TestStatusShowsQuarantineNoticeNotSavedReason(t *testing.T) {
	const saved = "a process is using this worktree (a shell standing in it counts); stop it or leave the worktree"
	const notice = "treehouse never frees a recovered slot on its own; it is held until it is returned by name"
	repoDir, homeDir := setupTestRepo(t)
	a := acquireLeaseJSON(t, repoDir, homeDir, "setup")
	poolDir := filepath.Dir(filepath.Dir(a.Path))
	data, err := json.Marshal(map[string]any{"version": 4, "worktrees": []map[string]any{{
		"name": filepath.Base(filepath.Dir(a.Path)), "path": a.Path, "created_at": time.Now().Add(-48 * time.Hour),
		"leased": true, "lease_holder": recoveredHolderLiteral, "leased_at": time.Now().Add(-time.Hour),
		"recovery_reason": saved,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "treehouse-state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	out, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "status")
	if code != 0 {
		t.Fatalf("status failed, code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(out, saved) || !strings.Contains(out, "recovery held: "+notice) {
		t.Fatalf("status text does not show the quarantine notice:\n%s", out)
	}
	out, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json failed, code=%d stderr=%q", code, stderr)
	}
	var worktrees []statusJSONWorktree
	if err := json.Unmarshal([]byte(out), &worktrees); err != nil {
		t.Fatalf("invalid status JSON %q: %v", out, err)
	}
	if len(worktrees) != 1 || worktrees[0].RecoveryReason != notice {
		t.Fatalf("status --json recovery_reason = %+v, want %q", worktrees, notice)
	}
}

// TestConditionalReturnClearsOnlyTheNamedRecoveredSlot covers the one way out of
// a persistent quarantine: an exact-target conditional return, with no --force
// and no prompt answer. A stale condition changes nothing, a sibling recovered
// slot keeps its lease and its files, and the returned slot can be allocated.
func TestConditionalReturnClearsOnlyTheNamedRecoveredSlot(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	a := acquireLeaseJSON(t, repoDir, homeDir, "setup-a")
	b := acquireLeaseJSON(t, repoDir, homeDir, "setup-b")
	poolDir := filepath.Dir(filepath.Dir(a.Path))
	var entries []map[string]any
	for _, l := range []string{a.Path, b.Path} {
		entries = append(entries, map[string]any{
			"name": filepath.Base(filepath.Dir(l)), "path": l, "created_at": time.Now().Add(-48 * time.Hour),
			"leased": true, "lease_holder": recoveredHolderLiteral, "leased_at": time.Now().Add(-time.Hour),
		})
	}
	data, err := json.Marshal(map[string]any{"version": 4, "worktrees": entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "treehouse-state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// B holds work the quarantine exists to protect.
	work := filepath.Join(b.Path, "agent-copy", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(work, []byte("unlanded\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "return", a.Path, "--if-lease-holder", "someone-else")
	if code == 0 || !strings.Contains(stderr, "lease holder does not match") {
		t.Fatalf("a stale holder condition returned or hid its cause: code=%d stderr=%q", code, stderr)
	}
	if out, _, _ := runTreehouse(t, repoDir, homeDir, nil, "status"); strings.Count(out, "held by recovered:") != 2 {
		t.Fatalf("a refused return changed the pool:\n%s", out)
	}

	_, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "return", a.Path, "--if-lease-holder", recoveredHolderLiteral)
	if code != 0 {
		t.Fatalf("conditional return of a recovered slot failed, code=%d stderr=%q", code, stderr)
	}
	out, _, _ := runTreehouse(t, repoDir, homeDir, nil, "status")
	if strings.Count(out, "held by recovered:") != 1 || !strings.Contains(out, "available") {
		t.Fatalf("only the named slot should be returned:\n%s", out)
	}
	if got, err := os.ReadFile(work); err != nil || string(got) != "unlanded\n" {
		t.Fatalf("the sibling's files changed: %q %v", got, err)
	}
	if backups, _ := filepath.Glob(filepath.Join(filepath.Dir(poolDir), "treehouse-recovered-backup-*")); len(backups) != 0 {
		t.Fatalf("a backup folder appeared: %v", backups)
	}
	if again := acquireLeaseJSON(t, repoDir, homeDir, "next"); again.Path != a.Path {
		t.Fatalf("get returned %s, want the returned slot %s", again.Path, a.Path)
	}
}
