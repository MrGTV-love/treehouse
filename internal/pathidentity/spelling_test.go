package pathidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExistingAliasSpellings(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := Existing(root)
	if err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(root, "StoredName")
	if err := os.Mkdir(stored, 0755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalRoot, "StoredName")
	if got, err := Existing(stored); err != nil || got != want {
		t.Fatalf("Existing(%q) = %q, %v; want %q", stored, got, err, want)
	}
	alias := filepath.Join(root, "storedname")
	storedInfo, err := os.Stat(stored)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, aliasErr := os.Stat(alias)
	if aliasErr == nil && os.SameFile(storedInfo, aliasInfo) {
		if got, err := Existing(alias); err != nil || got != want {
			t.Errorf("Existing(%q) = %q, %v; want %q", alias, got, err, want)
		}
	} else if os.IsNotExist(aliasErr) {
		if err := os.Mkdir(alias, 0755); err != nil {
			t.Fatal(err)
		}
		if got, err := Existing(alias); err != nil || got != filepath.Join(canonicalRoot, "storedname") || got == want {
			t.Errorf("case-distinct sibling identity = %q, %v", got, err)
		}
	} else {
		t.Fatalf("unexpected alias stat result: %v", aliasErr)
	}
	file := filepath.Join(stored, "RegularFile")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if got, err := Existing(file); err != nil || got != filepath.Join(want, "RegularFile") {
		t.Errorf("regular file identity = %q, %v", got, err)
	}
	missing := filepath.Join(stored, "Missing", "KeepCase")
	if got, err := Prefix(missing); err != nil || got != filepath.Join(want, "Missing", "KeepCase") {
		t.Errorf("missing suffix identity = %q, %v", got, err)
	}
}
