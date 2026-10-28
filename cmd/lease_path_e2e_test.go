package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// plainSlot creates a registered, unleased slot the way a long-lived agent home
// comes to exist: a plain get whose shell exits at once.
func plainSlot(t *testing.T, repoDir, homeDir string) string {
	t.Helper()
	_, getErr, code := runTreehouse(t, repoDir, homeDir, []string{"SHELL=" + exitShellBin}, "get")
	if code != 0 {
		t.Fatalf("get failed (code %d): %s", code, getErr)
	}
	wtPath := extractWorktreePath(getErr, homeDir)
	if wtPath == "" {
		t.Fatal("could not extract worktree path")
	}
	return wtPath
}

func slotStatus(t *testing.T, repoDir, homeDir, path string) statusJSONResult {
	t.Helper()
	for _, e := range statusEntries(t, repoDir, homeDir) {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no slot at %s in status", path)
	return statusJSONResult{}
}

func TestLeaseByPathLeasesFreeSlotStateOnly(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	wtPath := plainSlot(t, repoDir, homeDir)

	marker := filepath.Join(wtPath, "agent-home.txt")
	if err := os.WriteFile(marker, []byte("home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := gitCmd(t, wtPath, "rev-parse", "HEAD")
	statusBefore := gitCmd(t, wtPath, "status", "--porcelain")

	// Stand outside every repository: a path must find its own pool.
	outside := t.TempDir()
	stdout, stderr, code := runTreehouseFromDir(t, repoDir, outside, homeDir,
		[]string{"TREEHOUSE_LEASE_HOLDER=teardown"}, "lease", "--path", wtPath)
	if code != 0 {
		t.Fatalf("lease --path failed (code %d): %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != wtPath {
		t.Fatalf("lease --path printed %q, want %q", stdout, wtPath)
	}

	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "home\n" {
		t.Fatalf("slot file changed: %q, %v", got, err)
	}
	if head := gitCmd(t, wtPath, "rev-parse", "HEAD"); head != headBefore {
		t.Fatalf("HEAD moved: %s -> %s", headBefore, head)
	}
	if st := gitCmd(t, wtPath, "status", "--porcelain"); st != statusBefore {
		t.Fatalf("git status changed: %q -> %q", statusBefore, st)
	}

	entry := slotStatus(t, repoDir, homeDir, wtPath)
	if entry.Status != "leased" || entry.LeaseHolder != "teardown" || entry.LeaseID == "" {
		t.Fatalf("expected leased slot with holder, got %+v", entry)
	}

	// A later get must not hand the slot out.
	next, nextErr, code := runTreehouse(t, repoDir, homeDir, nil, "get", "--lease")
	if code != 0 {
		t.Fatalf("get --lease failed (code %d): %s", code, nextErr)
	}
	if strings.TrimSpace(next) == wtPath {
		t.Fatalf("get handed out the leased slot %s", wtPath)
	}
}

func TestLeaseByPathResolvesOnlyTheNamedPool(t *testing.T) {
	repoA, homeDir := setupTestRepo(t)
	repoB := setupTestRepoWithHome(t, homeDir, "otherrepo")

	pathA := plainSlot(t, repoA, homeDir)
	pathB := plainSlot(t, repoB, homeDir)
	if pathA == pathB || filepath.Dir(filepath.Dir(pathA)) == filepath.Dir(filepath.Dir(pathB)) {
		t.Fatalf("expected two separate pools, got %s and %s", pathA, pathB)
	}
	if slotStatus(t, repoA, homeDir, pathA).Name != "1" || slotStatus(t, repoB, homeDir, pathB).Name != "1" {
		t.Fatal("expected both pools to hold a slot named 1")
	}

	// Run from repo A's directory, naming repo B's slot.
	_, stderr, code := runTreehouse(t, repoA, homeDir, nil, "lease", "--path", pathB)
	if code != 0 {
		t.Fatalf("lease --path failed (code %d): %s", code, stderr)
	}
	if got := slotStatus(t, repoB, homeDir, pathB); got.Status != "leased" {
		t.Fatalf("named pool's slot not leased: %+v", got)
	}
	if got := slotStatus(t, repoA, homeDir, pathA); got.Status == "leased" {
		t.Fatalf("other pool's slot 1 was leased: %+v", got)
	}
}

func TestLeaseByPathRefusals(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	wtPath := plainSlot(t, repoDir, homeDir)

	if _, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "lease", "--path", wtPath, "--lease-holder", "first"); code != 0 {
		t.Fatalf("first lease failed (code %d): %s", code, stderr)
	}
	// The leased slot is skipped, so this is a second, free slot.
	otherPath := plainSlot(t, repoDir, homeDir)
	if otherPath == wtPath {
		t.Fatal("expected a second slot")
	}

	assertUnchanged := func(label string) {
		t.Helper()
		if got := slotStatus(t, repoDir, homeDir, otherPath); got.Status == "leased" {
			t.Fatalf("%s: unrelated slot got leased: %+v", label, got)
		}
		if got := slotStatus(t, repoDir, homeDir, wtPath); got.LeaseHolder != "first" {
			t.Fatalf("%s: lease changed: %+v", label, got)
		}
	}

	_, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "lease", "--path", wtPath, "--lease-holder", "second")
	if code == 0 || !strings.Contains(stderr, "already leased") || !strings.Contains(stderr, "first") {
		t.Fatalf("expected already-leased refusal, code=%d stderr=%q", code, stderr)
	}
	assertUnchanged("already leased")

	outside := t.TempDir()
	_, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "lease", "--path", outside)
	if code == 0 || !strings.Contains(stderr, "not managed by treehouse") {
		t.Fatalf("expected outside-pool refusal, code=%d stderr=%q", code, stderr)
	}
	assertUnchanged("outside pool")

	sub := filepath.Join(otherPath, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "lease", "--path", sub)
	if code == 0 || !strings.Contains(stderr, "not managed by treehouse") {
		t.Fatalf("expected non-root refusal, code=%d stderr=%q", code, stderr)
	}
	assertUnchanged("non-root")

	_, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "lease", slotStatus(t, repoDir, homeDir, otherPath).Name, "--path", otherPath)
	if code == 0 || !strings.Contains(stderr, "not both") {
		t.Fatalf("expected both-forms refusal, code=%d stderr=%q", code, stderr)
	}
	assertUnchanged("both forms")

	_, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "lease", "--path", "")
	if code == 0 {
		t.Fatalf("expected empty --path refusal, stderr=%q", stderr)
	}
	assertUnchanged("empty path")
}

func TestLeaseByNameStillWorks(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	wtPath := plainSlot(t, repoDir, homeDir)

	stdout, stderr, code := runTreehouse(t, repoDir, homeDir, nil, "lease", "1")
	if code != 0 || strings.TrimSpace(stdout) != wtPath {
		t.Fatalf("lease 1 failed (code %d, stdout %q): %s", code, stdout, stderr)
	}
	if got := slotStatus(t, repoDir, homeDir, wtPath); got.Status != "leased" {
		t.Fatalf("expected leased, got %+v", got)
	}
	_, stderr, code = runTreehouse(t, repoDir, homeDir, nil, "lease", "1")
	if code == 0 || !strings.Contains(stderr, `worktree 1 is already leased (holder: "")`) {
		t.Fatalf("expected today's already-leased message, code=%d stderr=%q", code, stderr)
	}
}
