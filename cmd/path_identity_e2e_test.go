package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/config"
	"github.com/kunchenguid/treehouse/v3/internal/pool"
)

func TestCaseAliasPoolLifecycle(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	aliasRepo := filepath.Join(filepath.Dir(repoDir), strings.ToUpper(filepath.Base(repoDir)))
	info, err := os.Stat(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(aliasRepo)
	if err != nil || !os.SameFile(info, aliasInfo) {
		t.Skip("requires a case-insensitive filesystem")
	}
	if err := os.WriteFile(filepath.Join(repoDir, "treehouse.toml"), []byte("max_trees = 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	first := acquireLeaseJSON(t, repoDir, homeDir, "canonical-owner")
	poolDir := filepath.Dir(filepath.Dir(first.Path))
	aliasPool := filepath.Join(filepath.Dir(poolDir), strings.ToUpper(filepath.Base(poolDir)))
	aliasPath := filepath.Join(aliasPool, "1", strings.ToUpper(filepath.Base(first.Path)))
	// A second physical clone with a case-variant leaf and identical origin
	// reproduces GRE-1933: its requested pool spelling names the same directory.
	otherParent := t.TempDir()
	other := filepath.Join(otherParent, strings.ToUpper(filepath.Base(repoDir)))
	gitCmd(t, "", "clone", filepath.Join(filepath.Dir(repoDir), "remote.git"), other)
	if err := os.WriteFile(filepath.Join(other, "treehouse.toml"), []byte("max_trees = 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "README.md"), []byte("live dirty work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	second := acquireLeaseJSON(t, other, homeDir, "other-clone-owner")
	if first.Path == second.Path {
		t.Fatal("distinct live acquisitions shared a slot")
	}
	resolved, err := config.ResolvePoolDir(other, homeDir)
	if err != nil || resolved != poolDir {
		t.Fatalf("capitalized clone resolved a different pool: got %s, want %s (%v)", resolved, poolDir, err)
	}
	assertHeld := func() {
		t.Helper()
		entries := statusEntries(t, other, homeDir)
		if len(entries) != 2 {
			t.Fatalf("physical slots were counted more than once: %#v", entries)
		}
		for _, lease := range []leaseJSONResult{first, second} {
			found := false
			for _, entry := range entries {
				if entry.Path == lease.Path && entry.LeaseID == lease.LeaseID && entry.LeaseHolder == lease.LeaseHolder {
					found = true
				}
			}
			if !found {
				t.Fatalf("live lease changed: want %#v, got %#v", lease, entries)
			}
		}
	}
	assertHeld()
	// Simulate a legacy recovered alias without changing the original signed
	// inventory. Loading must consolidate it before automatic recovery acts.
	data, err := os.ReadFile(filepath.Join(poolDir, "treehouse-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state pool.State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Worktrees = append([]pool.WorktreeEntry{{Name: "1", Path: aliasPath, Leased: true, LeaseHolder: pool.RecoveredLeaseHolder}}, state.Worktrees...)
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "treehouse-state.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	assertHeld()
	for _, args := range [][]string{
		{"destroy", aliasPath, "--yes"},
		{"return", aliasPath, "--if-lease-id", first.LeaseID},
		{"return", aliasPath, "--force", "--if-lease-id", "wrong-lease"},
	} {
		out, diagnostic, code := runTreehouse(t, repoDir, homeDir, nil, args...)
		if code == 0 {
			t.Fatalf("alias bypassed a protection: %v\n%s\n%s", args, out, diagnostic)
		}
		assertHeld()
		if dirty, err := os.ReadFile(filepath.Join(first.Path, "README.md")); err != nil || string(dirty) != "live dirty work\n" {
			t.Fatalf("refused alias operation changed live work: %q %v", dirty, err)
		}
	}
	gitCmd(t, first.Path, "checkout", "--", "README.md")
	out, diagnostic, code := runTreehouse(t, repoDir, homeDir, nil, "return", aliasPath, "--if-lease-id", first.LeaseID)
	if code != 0 {
		t.Fatalf("alias conditional return failed: %d\n%s\n%s", code, out, diagnostic)
	}
	t.Run("live-process-protection", func(t *testing.T) {
		startForeignWorktreeProcess(t, first.Path)
		out, diagnostic, code := runTreehouse(t, repoDir, homeDir, nil, "destroy", aliasPath, "--yes")
		if code == 0 || (!strings.Contains(out+diagnostic, "in-use") && !strings.Contains(out+diagnostic, "in use")) {
			t.Fatalf("alias bypassed live-process protection: %d\n%s\n%s", code, out, diagnostic)
		}
	})
	gitCmd(t, first.Path, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "--allow-empty", "-m", "unlanded work")
	out, diagnostic, code = runTreehouse(t, repoDir, homeDir, nil, "destroy", aliasPath, "--yes")
	if code == 0 || !strings.Contains(out+diagnostic, "unlanded") {
		t.Fatalf("alias bypassed unlanded-work protection: %d\n%s\n%s", code, out, diagnostic)
	}
	gitCmd(t, first.Path, "reset", "--hard", "origin/main")
	out, diagnostic, code = runTreehouse(t, repoDir, homeDir, nil, "destroy", aliasPath, "--yes")
	if code != 0 {
		t.Fatalf("alias destroy failed: %d\n%s\n%s", code, out, diagnostic)
	}
	entries := statusEntries(t, other, homeDir)
	if len(entries) != 1 || entries[0].Path != second.Path || entries[0].LeaseID != second.LeaseID || entries[0].LeaseHolder != second.LeaseHolder {
		t.Fatalf("alias destroy changed the other live slot: %#v", entries)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("destroyed slot remains: %v", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("other live slot was removed: %v", err)
	}
}

func TestCaseAliasLegacyRecordKeepsLiveProcessProtection(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	lease := acquireLeaseJSON(t, repoDir, homeDir, "legacy-owner")
	poolDir := filepath.Dir(filepath.Dir(lease.Path))
	aliasPool := filepath.Join(filepath.Dir(poolDir), strings.ToUpper(filepath.Base(poolDir)))
	alias := filepath.Join(aliasPool, "1", filepath.Base(lease.Path))
	info, err := os.Stat(alias)
	original, originalErr := os.Stat(lease.Path)
	if err != nil || originalErr != nil || !os.SameFile(info, original) {
		t.Skip("requires a case-insensitive filesystem")
	}
	state, err := pool.ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the spelling a pre-fix capitalized-clone acquisition recorded,
	// including its path-bound signature, instead of silently rewriting it.
	state.Worktrees[0].Path = alias
	if err := pool.WriteState(poolDir, state); err != nil {
		t.Fatal(err)
	}
	pid := startForeignWorktreeProcess(t, lease.Path)
	out, diagnostic, code := runTreehouse(t, repoDir, homeDir, nil, "destroy", lease.Path, "--include-leased", "--yes")
	if code == 0 || (!strings.Contains(out+diagnostic, "in-use") && !strings.Contains(out+diagnostic, "in use")) {
		t.Fatalf("legacy case spelling hid its live process: %d\n%s\n%s", code, out, diagnostic)
	}
	entries := statusEntries(t, repoDir, homeDir)
	if len(entries) != 1 || entries[0].LeaseID != lease.LeaseID || entries[0].Path != alias {
		t.Fatalf("refused removal changed legacy ownership: %#v", entries)
	}
	var processes []statusJSONProcessResult
	if err := json.Unmarshal(entries[0].Processes, &processes); err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		if process.PID == pid {
			return
		}
	}
	t.Fatalf("refused removal did not leave live process %d visible: %#v", pid, processes)
}
