package process

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreeContainsCwd_CaseAliases(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Slot")
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "slot")
	original, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	aliased, err := os.Stat(alias)
	if err != nil || !os.SameFile(original, aliased) {
		t.Skip("requires a case-insensitive filesystem")
	}
	for _, pair := range [][2]string{{alias, root}, {alias, child}, {root, alias}, {root, filepath.Join(alias, "child")}} {
		if !WorktreeContainsCwd(pair[0], pair[1]) {
			t.Fatalf("case alias hides cwd: root=%s cwd=%s", pair[0], pair[1])
		}
	}
	other := filepath.Join(base, "Slot-other")
	if err := os.Mkdir(other, 0755); err != nil {
		t.Fatal(err)
	}
	if WorktreeContainsCwd(alias, other) || WorktreeContainsCwd(alias, "") {
		t.Fatal("an unrelated or unknown cwd was matched to the slot")
	}
}

func TestWorktreeContainsCwd_CaseSensitiveSiblings(t *testing.T) {
	base := t.TempDir()
	upper, lower := filepath.Join(base, "Slot"), filepath.Join(base, "slot")
	if err := os.Mkdir(upper, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lower, 0755); os.IsExist(err) {
		t.Skip("requires a case-sensitive filesystem")
	} else if err != nil {
		t.Fatal(err)
	}
	if WorktreeContainsCwd(upper, lower) || WorktreeContainsCwd(lower, upper) {
		t.Fatal("case-sensitive sibling cwd was attributed to the other live slot")
	}
}
