package pathidentity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSearchOnlyAncestorIdentity(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permissions without root privileges")
	}
	root := t.TempDir()
	ancestor := filepath.Join(root, "SearchOnly")
	child := filepath.Join(ancestor, "MiXeD")
	sibling := filepath.Join(ancestor, "Other")
	for _, path := range []string{child, sibling} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "Alias")
	if err := os.Symlink(child, link); err != nil {
		t.Fatal(err)
	}
	canonicalChild, err := Existing(child)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSibling, err := Existing(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalChild == canonicalSibling {
		t.Fatal("distinct siblings share an identity")
	}
	alias := filepath.Join(root, strings.ToLower(filepath.Base(ancestor)), strings.ToLower(filepath.Base(child)))
	childInfo, err := os.Stat(child)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, aliasErr := os.Stat(alias)
	caseInsensitive := aliasErr == nil && os.SameFile(childInfo, aliasInfo)
	if err := os.Chmod(ancestor, 0111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ancestor, 0755); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadDir(ancestor); !os.IsPermission(err) {
		t.Skipf("directory read permission restriction is not enforced: %v", err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("search-only directory is not traversable: %v", err)
	}
	assertIdentity := func(path, want string) {
		t.Helper()
		got, err := Existing(path)
		if err != nil || got != want {
			t.Errorf("Existing(%q) = %q, %v; want %q", path, got, err, want)
		}
		got, err = Prefix(path)
		if err != nil || got != want {
			t.Errorf("Prefix(%q) = %q, %v; want %q", path, got, err, want)
		}
		missing := filepath.Join("NotCreated", "KeepCase")
		got, err = Prefix(filepath.Join(path, missing))
		if err != nil || got != filepath.Join(want, missing) {
			t.Errorf("Prefix(%q + missing) = %q, %v; want %q", path, got, err, filepath.Join(want, missing))
		}
	}
	assertIdentity(child, canonicalChild)
	assertIdentity(link, canonicalChild)
	assertIdentity(sibling, canonicalSibling)
	if caseInsensitive {
		assertIdentity(alias, canonicalChild)
	} else {
		t.Log("case alias checks unavailable on this filesystem")
	}
	if _, err := Existing(filepath.Join(child, "NotCreated")); !os.IsNotExist(err) {
		t.Errorf("missing target error = %v; want not-exist", err)
	}
	if err := os.Chmod(ancestor, 0000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child); !os.IsPermission(err) {
		t.Skipf("directory search permission restriction is not enforced: %v", err)
	}
	for _, path := range []string{child, link, filepath.Join(child, "NotCreated")} {
		if got, err := Existing(path); err == nil || !os.IsPermission(err) {
			t.Errorf("Existing(%q) = %q, %v; want permission failure", path, got, err)
		}
		if got, err := Prefix(path); err == nil || !os.IsPermission(err) {
			t.Errorf("Prefix(%q) = %q, %v; want permission failure", path, got, err)
		}
	}
}
