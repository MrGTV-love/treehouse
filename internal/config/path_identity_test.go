package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePoolDir_LocalRepositoryAliases(t *testing.T) {
	repo := setupGitRepo(t)
	run(t, repo, "git", "remote", "remove", "origin")
	root := t.TempDir()
	canonical, err := ResolvePoolDir(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(repo), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := ResolvePoolDir(alias, root)
	if err != nil || got != canonical {
		t.Fatalf("local repository alias changed pool identity: got %s, want %s (%v)", got, canonical, err)
	}
	caseAlias := filepath.Join(filepath.Dir(repo), strings.ToUpper(filepath.Base(repo)))
	info, err := os.Stat(caseAlias)
	if os.IsNotExist(err) {
		return // This filesystem keeps case variants distinct.
	}
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(repo)
	if err != nil || !os.SameFile(original, info) {
		t.Fatalf("unexpected case probe identity: %v", err)
	}
	got, err = ResolvePoolDir(caseAlias, root)
	if err != nil || got != canonical {
		t.Fatalf("local repository case alias changed pool hash/name: got %s, want %s (%v)", got, canonical, err)
	}
}

func TestResolvePoolDir_ExistingPoolSpellingPreserved(t *testing.T) {
	repo := setupGitRepo(t)
	root := t.TempDir()
	requested, err := ResolvePoolDir(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	// A legacy pool can have been created by a differently-cased clone. The
	// existing physical directory wins; resolution must never move that pool.
	legacy := filepath.Join(filepath.Dir(requested), strings.ToUpper(filepath.Base(requested)))
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(requested)
	if os.IsNotExist(err) {
		// On a case-sensitive filesystem these names are genuinely distinct.
		got, resolveErr := ResolvePoolDir(repo, root)
		if resolveErr != nil || got != requested {
			t.Fatalf("distinct legacy name redirected pool: %s %v", got, resolveErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	legacyInfo, err := os.Stat(legacy)
	if err != nil || !os.SameFile(info, legacyInfo) {
		t.Fatalf("legacy pool case probe failed: %v", err)
	}
	got, err := ResolvePoolDir(repo, root)
	if err != nil || got != legacy {
		t.Fatalf("existing legacy pool spelling changed: got %s, want %s (%v)", got, legacy, err)
	}
}
