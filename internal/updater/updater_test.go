package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.9.9", "v2.0.0", -1},
		{"v0.3.0", "v0.2.1", 1},
		{"v0.2.1", "v0.3.0", -1},
		// Without v prefix
		{"1.0.0", "1.0.0", 0},
		{"1.1.0", "1.0.0", 1},
		// Mixed prefix
		{"v1.0.0", "1.0.0", 0},
		// Partial versions
		{"v1", "v1.0.0", 0},
		{"v1.2", "v1.2.0", 0},
		// Pre-release sorts before release
		{"v1.0.0-beta", "v1.0.0", -1},
		{"v1.0.0", "v1.0.0-beta", 1},
		{"v1.0.0-alpha", "v1.0.0-beta", -1},
		{"v1.0.0-beta", "v1.0.0-alpha", 1},
		{"v1.0.0-rc.1", "v1.0.0-rc.1", 0},
		// Pre-release of higher version still beats release of lower
		{"v2.0.0-beta", "v1.0.0", 1},
	}

	for _, tt := range tests {
		got := CompareVersions(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input      string
		wantNums   [3]int
		wantPrerel string
	}{
		{"v1.2.3", [3]int{1, 2, 3}, ""},
		{"1.2.3", [3]int{1, 2, 3}, ""},
		{"v0.0.0", [3]int{0, 0, 0}, ""},
		{"v10.20.30", [3]int{10, 20, 30}, ""},
		{"v1", [3]int{1, 0, 0}, ""},
		{"v1.2", [3]int{1, 2, 0}, ""},
		{"v1.2.3-beta", [3]int{1, 2, 3}, "beta"},
		{"v1.2.3-rc.1", [3]int{1, 2, 3}, "rc.1"},
		{"invalid", [3]int{0, 0, 0}, ""},
	}

	for _, tt := range tests {
		got := parseVersion(tt.input)
		if got.nums != tt.wantNums {
			t.Errorf("parseVersion(%q).nums = %v, want %v", tt.input, got.nums, tt.wantNums)
		}
		if got.prerelease != tt.wantPrerel {
			t.Errorf("parseVersion(%q).prerelease = %q, want %q", tt.input, got.prerelease, tt.wantPrerel)
		}
	}
}

func TestCacheReadWriteStaleness(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	// Cache should not exist yet
	result := ReadCache("v1.0.0")
	if result != nil {
		t.Fatal("expected nil from ReadCache with no cache file")
	}

	if !IsCacheStale("v1.0.0") {
		t.Fatal("expected stale with no cache file")
	}

	// Write cache
	entry := CacheEntry{
		CheckedAt:     time.Now(),
		LatestVersion: "v2.0.0",
	}
	if err := writeCache(entry); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	// Cache should now be readable
	result = ReadCache("v1.0.0")
	if result == nil {
		t.Fatal("expected non-nil from ReadCache")
	}
	if !result.UpdateAvailable {
		t.Error("expected UpdateAvailable=true")
	}
	if result.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "v2.0.0")
	}

	// Cache should not be stale (current v1.0.0 < cached latest v2.0.0)
	if IsCacheStale("v1.0.0") {
		t.Error("expected fresh cache")
	}

	// No update available when current >= latest
	result = ReadCache("v2.0.0")
	if result == nil {
		t.Fatal("expected non-nil from ReadCache")
	}
	if result.UpdateAvailable {
		t.Error("expected UpdateAvailable=false when current == latest")
	}

	result = ReadCache("v3.0.0")
	if result == nil {
		t.Fatal("expected non-nil from ReadCache")
	}
	if result.UpdateAvailable {
		t.Error("expected UpdateAvailable=false when current > latest")
	}
}

func TestCacheStaleAfterTTL(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	entry := CacheEntry{
		CheckedAt:     time.Now().Add(-25 * time.Hour),
		LatestVersion: "v1.0.0",
	}
	if err := writeCache(entry); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	if !IsCacheStale("v0.9.0") {
		t.Error("expected stale cache after TTL")
	}
}

func TestCacheStaleWhenCurrentPastLatest(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	// Cache says latest is v1.0.0, but user is now on v1.1.0
	entry := CacheEntry{
		CheckedAt:     time.Now(),
		LatestVersion: "v1.0.0",
	}
	if err := writeCache(entry); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	// Should be stale because current version >= cached latest
	if !IsCacheStale("v1.1.0") {
		t.Error("expected stale when current version is newer than cached latest")
	}
	if !IsCacheStale("v1.0.0") {
		t.Error("expected stale when current version equals cached latest")
	}

	// Should NOT be stale when cached latest is ahead of current
	if IsCacheStale("v0.9.0") {
		t.Error("expected fresh when cached latest is newer than current")
	}
}

func TestCacheCorrupted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	dir := filepath.Join(tmp, treehouseDir)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, cacheFileName), []byte("not json"), 0o644)

	result := ReadCache("v1.0.0")
	if result != nil {
		t.Error("expected nil from corrupted cache")
	}
}

func TestAssetNameForVersion(t *testing.T) {
	name := AssetNameForVersion("v1.2.3")
	if runtime.GOOS == "windows" {
		expected := "treehouse-v1.2.3-windows-" + runtime.GOARCH + ".zip"
		if name != expected {
			t.Errorf("AssetNameForVersion() = %q, want %q", name, expected)
		}
	} else {
		expected := "treehouse-v1.2.3-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
		if name != expected {
			t.Errorf("AssetNameForVersion() = %q, want %q", name, expected)
		}
	}
}

func TestMatchesCurrentPlatformAsset(t *testing.T) {
	name := AssetNameForVersion("v2.0.0")
	if !matchesCurrentPlatformAsset(name) {
		t.Errorf("expected %q to match current platform", name)
	}

	if matchesCurrentPlatformAsset("treehouse-v1.0.0-fakeos-fakearg.tar.gz") {
		t.Error("expected non-matching asset to fail")
	}
}

func TestExtractTarGz(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	content := []byte("#!/bin/sh\necho hello\n")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{
		Name: "treehouse",
		Size: int64(len(content)),
		Mode: 0o755,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	binaryPath, err := extractTarGz(archivePath)
	if err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	defer os.Remove(binaryPath)

	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("extracted content = %q, want %q", string(got), string(content))
	}
}

func TestExtractZip(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "test.zip")
	content := []byte("MZ fake exe")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("treehouse.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	f.Close()

	binaryPath, err := extractZip(archivePath)
	if err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	defer os.Remove(binaryPath)

	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("extracted content = %q, want %q", string(got), string(content))
	}
}

func TestExtractTarGzMissingBinary(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "test.tar.gz")
	content := []byte("some file")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{
		Name: "other-file",
		Size: int64(len(content)),
		Mode: 0o644,
	})
	tw.Write(content)
	tw.Close()
	gz.Close()
	f.Close()

	_, err = extractTarGz(archivePath)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	now := time.Now().Truncate(time.Second)
	entry := CacheEntry{
		CheckedAt:     now,
		LatestVersion: "v1.2.3",
	}
	if err := writeCache(entry); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	data, err := os.ReadFile(cachePath())
	if err != nil {
		t.Fatal(err)
	}

	var got CacheEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}

	if got.LatestVersion != "v1.2.3" {
		t.Errorf("LatestVersion = %q, want %q", got.LatestVersion, "v1.2.3")
	}
}

// createTestTarGz builds a tar.gz archive containing a fake "treehouse" binary
// with the given content, and returns the raw bytes.
func createTestTarGz(t *testing.T, content []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{
		Name: "treehouse",
		Size: int64(len(content)),
		Mode: 0o755,
	})
	tw.Write(content)
	tw.Close()
	gz.Close()
	f.Close()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

func createTestZip(t *testing.T, content []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("treehouse.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	f.Close()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

// createTestArchive creates a tar.gz on unix or zip on windows.
func createTestArchive(t *testing.T, content []byte) []byte {
	t.Helper()
	if runtime.GOOS == "windows" {
		return createTestZip(t, content)
	}
	return createTestTarGz(t, content)
}

// fakeGitHubServer starts an httptest server that serves a GitHub-like
// /releases/latest response, an asset download endpoint, and a checksums endpoint.
func fakeGitHubServer(t *testing.T, latestTag string, archiveBytes []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	assetName := AssetNameForVersion(latestTag)

	// Compute SHA256 checksum of the archive
	h := sha256.New()
	h.Write(archiveBytes)
	checksum := hex.EncodeToString(h.Sum(nil))
	checksumsContent := fmt.Sprintf("%s  %s\n", checksum, assetName)

	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		release := githubRelease{
			TagName: latestTag,
			Assets: []githubAsset{
				{
					Name:               assetName,
					BrowserDownloadURL: fmt.Sprintf("http://%s/download/%s", r.Host, assetName),
				},
				{
					Name:               "checksums.txt",
					BrowserDownloadURL: fmt.Sprintf("http://%s/download/checksums.txt", r.Host),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(release)
	})

	mux.HandleFunc("/download/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(archiveBytes)
	})

	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(checksumsContent))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Disable HTTPS enforcement for test HTTP servers
	enforceHTTPS = false
	t.Cleanup(func() { enforceHTTPS = true })
	enforceReleaseSource = false
	t.Cleanup(func() { enforceReleaseSource = true })

	return srv
}

func TestCheckLatestE2E(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	archive := createTestArchive(t, []byte("new-binary-content"))
	srv := fakeGitHubServer(t, "v2.0.0", archive)

	origURL := githubAPIURL
	githubAPIURL = srv.URL + "/releases/latest"
	t.Cleanup(func() { githubAPIURL = origURL })

	result, err := CheckLatest("v1.0.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}

	if !result.UpdateAvailable {
		t.Error("expected UpdateAvailable=true")
	}
	if result.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "v2.0.0")
	}
	if result.DownloadURL == "" {
		t.Error("expected DownloadURL to be set")
	}
	if result.ChecksumURL == "" {
		t.Error("expected ChecksumURL to be set")
	}

	// Verify cache was written
	cached := ReadCache("v1.0.0")
	if cached == nil {
		t.Fatal("expected cache to be written")
	}
	if cached.LatestVersion != "v2.0.0" {
		t.Errorf("cached LatestVersion = %q, want %q", cached.LatestVersion, "v2.0.0")
	}
	if IsCacheStale("v1.0.0") {
		t.Error("cache should be fresh after CheckLatest")
	}
}

func TestCheckLatestNoUpdate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	archive := createTestArchive(t, []byte("content"))
	srv := fakeGitHubServer(t, "v1.0.0", archive)

	origURL := githubAPIURL
	githubAPIURL = srv.URL + "/releases/latest"
	t.Cleanup(func() { githubAPIURL = origURL })

	result, err := CheckLatest("v1.0.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}

	if result.UpdateAvailable {
		t.Error("expected UpdateAvailable=false when versions match")
	}
}

func TestApplyE2E(t *testing.T) {
	newContent := []byte("#!/bin/sh\necho updated\n")
	archive := createTestArchive(t, newContent)
	srv := fakeGitHubServer(t, "v2.0.0", archive)

	// Create a fake "current binary" to be replaced
	targetDir := t.TempDir()
	binaryName := "treehouse"
	if runtime.GOOS == "windows" {
		binaryName = "treehouse.exe"
	}
	targetPath := filepath.Join(targetDir, binaryName)
	if err := os.WriteFile(targetPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	assetName := AssetNameForVersion("v2.0.0")
	result := &CheckResult{
		CurrentVersion:  "v1.0.0",
		LatestVersion:   "v2.0.0",
		UpdateAvailable: true,
		DownloadURL:     srv.URL + "/download/" + assetName,
		ChecksumURL:     srv.URL + "/download/checksums.txt",
	}

	// Test individual steps since we can't patch os.Executable
	archivePath, err := downloadToTemp(result.DownloadURL)
	if err != nil {
		t.Fatalf("downloadToTemp: %v", err)
	}
	defer os.Remove(archivePath)

	if err := verifyChecksum(archivePath, result.ChecksumURL); err != nil {
		t.Fatalf("verifyChecksum: %v", err)
	}

	newBinaryPath, err := extractBinary(archivePath)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	defer os.Remove(newBinaryPath)

	got, err := os.ReadFile(newBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Errorf("extracted = %q, want %q", string(got), string(newContent))
	}

	if err := atomicReplace(targetPath, newBinaryPath); err != nil {
		t.Fatalf("atomicReplace: %v", err)
	}

	replaced, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(replaced) != string(newContent) {
		t.Errorf("replaced binary = %q, want %q", string(replaced), string(newContent))
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(targetPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Error("expected executable permissions on replaced binary")
		}
	}
}

func TestAtomicReplaceRunningWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows running-image replacement")
	}

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "hold.exe")
	holdSrc := filepath.Join("testdata", "holdopen", "main.go")
	build := exec.Command("go", "build", "-o", targetPath, holdSrc)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build holdopen: %v\n%s", err, out)
	}

	cmd := exec.Command(targetPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holdopen: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(200 * time.Millisecond)

	newBinaryPath := filepath.Join(dir, "new.bin")
	newContent := []byte("replacement-bytes")
	if err := os.WriteFile(newBinaryPath, newContent, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := atomicReplace(targetPath, newBinaryPath); err != nil {
		t.Fatalf("atomicReplace running windows exe: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Errorf("replaced = %q, want %q", string(got), string(newContent))
	}
}

func TestReplaceRunningWindowsStaleOld(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows running-image replacement")
	}

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "hold.exe")
	staleOld := targetPath + ".old"
	holdSrc := filepath.Join("testdata", "holdopen", "main.go")
	build := exec.Command("go", "build", "-o", staleOld, holdSrc)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("go build holdopen: %v\n%s", err, out)
	}

	cmd := exec.Command(staleOld)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start leftover .old: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(targetPath, []byte("current-bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(dir, "new.bin")
	newContent := []byte("replacement-bytes")
	if err := os.WriteFile(tmpPath, newContent, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceRunningWindows(targetPath, tmpPath); err != nil {
		t.Fatalf("replaceRunningWindows with mapped leftover .old: %v", err)
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Errorf("replaced = %q, want %q", string(got), string(newContent))
	}
}

func TestRequireHTTPS(t *testing.T) {
	if err := requireHTTPS("https://example.com/file"); err != nil {
		t.Errorf("expected no error for HTTPS URL, got: %v", err)
	}
	if err := requireHTTPS("http://example.com/file"); err == nil {
		t.Error("expected error for HTTP URL")
	}
	if err := requireHTTPS(""); err == nil {
		t.Error("expected error for empty URL")
	}
}

func TestVerifyChecksumMismatch(t *testing.T) {
	tmp, err := os.CreateTemp("", "checksum-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	tmp.Write([]byte("real content"))
	tmp.Close()

	assetName := AssetNameForVersion("v1.0.0")
	wrongHash := "0000000000000000000000000000000000000000000000000000000000000000"
	mux := http.NewServeMux()
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", wrongHash, assetName)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	enforceHTTPS = false
	defer func() { enforceHTTPS = true }()

	err = verifyChecksum(tmp.Name(), srv.URL+"/checksums.txt")
	if err == nil {
		t.Fatal("expected error for checksum mismatch")
	}
}

func TestVerifyChecksumNoURL(t *testing.T) {
	err := verifyChecksum("/dev/null", "")
	if err == nil {
		t.Fatal("expected error when checksum URL is empty")
	}
}

// The update-check child outlives the command that spawned it. Inheriting the
// caller's directory made it a process whose cwd sat inside whatever pooled
// worktree treehouse was invoked from, so treehouse reported a process of its
// own making as a tenant of that slot.
func TestBackgroundCheckCommandRunsOutsideTheCallersDirectory(t *testing.T) {
	caller := t.TempDir()
	t.Chdir(caller)

	cmd := backgroundCheckCommand("/usr/local/bin/treehouse", "v1.0.0")

	if cmd.Dir == "" {
		t.Fatal("expected the update-check child to be given a working directory of its own")
	}
	rel, err := filepath.Rel(cmd.Dir, caller)
	if err == nil && rel == "." {
		t.Fatalf("expected a directory outside the caller's, got %q", cmd.Dir)
	}
	if info, err := os.Stat(cmd.Dir); err != nil || !info.IsDir() {
		t.Fatalf("expected %q to be an existing directory: %v", cmd.Dir, err)
	}
}

// setTempDir points every variable os.TempDir consults on any supported
// platform at dir.
func setTempDir(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	out, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func isInsideTree(t *testing.T, dir, root string) bool {
	t.Helper()
	rel, err := filepath.Rel(resolved(t, root), resolved(t, dir))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// markWorktree turns dir into something the updater recognises as a worktree
// root: a .git pointer file, as a pooled git slot carries.
func markWorktree(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// A caller standing in no worktree takes the temp directory as-is.
func TestDetachedWorkingDirAcceptsATempDirOutsideAnyWorktree(t *testing.T) {
	base := t.TempDir()
	caller := filepath.Join(base, "caller")
	tmp := filepath.Join(base, "tmp")
	mkdirs(t, caller, tmp)
	t.Chdir(caller)
	setTempDir(t, tmp)

	if got := detachedWorkingDir(); resolved(t, got) != resolved(t, tmp) {
		t.Fatalf("expected the temp directory %q, got %q", tmp, got)
	}
}

// A temp directory outside the worktree the caller stands in is accepted even
// when the caller is deep inside that worktree.
func TestDetachedWorkingDirAcceptsATempDirOutsideTheEnclosingWorktree(t *testing.T) {
	base := t.TempDir()
	slot := filepath.Join(base, "slot")
	caller := filepath.Join(slot, "src", "pkg")
	tmp := filepath.Join(base, "tmp")
	mkdirs(t, caller, tmp)
	markWorktree(t, slot)
	t.Chdir(caller)
	setTempDir(t, tmp)

	if got := detachedWorkingDir(); resolved(t, got) != resolved(t, tmp) {
		t.Fatalf("expected the temp directory %q, got %q", tmp, got)
	}
}

// TMPDIR=$PWD/.tmp while standing at the root of a pooled worktree would hand
// the detached child a cwd inside the very slot it was detached from. The temp
// directory is rejected and the child runs somewhere the slot cannot contain.
func TestDetachedWorkingDirRejectsATempDirBeneathTheCaller(t *testing.T) {
	slot := t.TempDir()
	tmp := filepath.Join(slot, ".tmp")
	mkdirs(t, tmp)
	markWorktree(t, slot)
	t.Chdir(slot)
	setTempDir(t, tmp)

	assertOutsideWorktree(t, detachedWorkingDir(), slot)
}

// From <slot>/src with TMPDIR=<slot>/.tmp the temp directory is a sibling of
// the caller's directory, not beneath it, yet still inside the slot. The
// boundary is the enclosing worktree, so it is rejected all the same.
func TestDetachedWorkingDirRejectsATempDirBesideTheCallerInsideTheWorktree(t *testing.T) {
	slot := t.TempDir()
	caller := filepath.Join(slot, "src")
	tmp := filepath.Join(slot, ".tmp")
	mkdirs(t, caller, tmp)
	markWorktree(t, slot)
	t.Chdir(caller)
	setTempDir(t, tmp)

	assertOutsideWorktree(t, detachedWorkingDir(), slot)
}

// The worktree root itself is inside the worktree.
func TestDetachedWorkingDirRejectsTheWorktreeRootAsTempDir(t *testing.T) {
	slot := t.TempDir()
	caller := filepath.Join(slot, "src")
	mkdirs(t, caller)
	markWorktree(t, slot)
	t.Chdir(caller)
	setTempDir(t, slot)

	assertOutsideWorktree(t, detachedWorkingDir(), slot)
}

// A symlink outside the worktree that resolves into it is still inside.
func TestDetachedWorkingDirResolvesASymlinkedTempDir(t *testing.T) {
	base := t.TempDir()
	slot := filepath.Join(base, "slot")
	caller := filepath.Join(slot, "src")
	inside := filepath.Join(slot, ".tmp")
	mkdirs(t, caller, inside)
	markWorktree(t, slot)
	link := filepath.Join(base, "tmp-link")
	if err := os.Symlink(inside, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	t.Chdir(caller)
	setTempDir(t, link)

	assertOutsideWorktree(t, detachedWorkingDir(), slot)
}

// A caller reached through a symlink into a worktree is still inside it.
func TestDetachedWorkingDirResolvesASymlinkedCaller(t *testing.T) {
	base := t.TempDir()
	slot := filepath.Join(base, "slot")
	tmp := filepath.Join(slot, ".tmp")
	mkdirs(t, filepath.Join(slot, "src"), tmp)
	markWorktree(t, slot)
	link := filepath.Join(base, "slot-link")
	if err := os.Symlink(slot, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	t.Chdir(filepath.Join(link, "src"))
	setTempDir(t, tmp)

	assertOutsideWorktree(t, detachedWorkingDir(), slot)
}

func assertOutsideWorktree(t *testing.T, got, slot string) {
	t.Helper()
	if got == "" {
		t.Fatal("expected a working directory of the child's own, got an inheriting empty Dir")
	}
	if isInsideTree(t, got, slot) {
		t.Fatalf("expected a directory outside the worktree %q, got %q", slot, got)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("expected %q to be an existing directory: %v", got, err)
	}
}

// The child must still be told not to spawn a check of its own.
func TestBackgroundCheckCommandSuppressesRecursiveChecks(t *testing.T) {
	cmd := backgroundCheckCommand("/usr/local/bin/treehouse", "v1.0.0")

	var found bool
	for _, kv := range cmd.Env {
		if kv == "TREEHOUSE_NO_UPDATE_CHECK=1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected TREEHOUSE_NO_UPDATE_CHECK=1 in the child environment")
	}
}

// upstreamReleaseRepo is the project this fork was taken from. A fork build
// must never check it for updates or install anything from it.
const upstreamReleaseRepo = "kunchenguid/treehouse"

func TestDefaultReleaseSourceIsTheFork(t *testing.T) {
	u, err := url.Parse(defaultGitHubAPIURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "api.github.com" {
		t.Errorf("update check host = %q, want api.github.com", u.Host)
	}
	if want := "/repos/" + ReleaseRepo + "/releases/latest"; u.Path != want {
		t.Errorf("update check path = %q, want %q", u.Path, want)
	}
	if strings.Contains(strings.ToLower(defaultGitHubAPIURL), upstreamReleaseRepo) {
		t.Errorf("update check %q names upstream %s", defaultGitHubAPIURL, upstreamReleaseRepo)
	}
	if githubAPIURL != defaultGitHubAPIURL {
		t.Errorf("githubAPIURL = %q, want the default %q", githubAPIURL, defaultGitHubAPIURL)
	}
}

// releasesByRepo serves GitHub's /repos/<repo>/releases/latest for several
// repositories at once and records which ones were asked, so a test can prove
// which release source an update check really used.
type releasesByRepo struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []string
}

func newReleasesByRepo(t *testing.T, tagByRepo map[string]string) *releasesByRepo {
	t.Helper()
	r := &releasesByRepo{}
	mux := http.NewServeMux()
	for repo, tag := range tagByRepo {
		repo, tag := repo, tag
		assetName := AssetNameForVersion(tag)
		mux.HandleFunc("/repos/"+repo+"/releases/latest", func(w http.ResponseWriter, req *http.Request) {
			r.mu.Lock()
			r.hits = append(r.hits, repo)
			r.mu.Unlock()
			// Asset URLs follow GitHub's real shape, so the source check sees
			// the repository each release belongs to.
			base := "https://github.com/" + repo + "/releases/download/" + tag + "/"
			json.NewEncoder(w).Encode(githubRelease{
				TagName: tag,
				Assets: []githubAsset{
					{Name: assetName, BrowserDownloadURL: base + assetName},
					{Name: "checksums.txt", BrowserDownloadURL: base + "checksums.txt"},
				},
			})
		})
	}
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *releasesByRepo) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hits...)
}

// useDefaultReleasePath points the update check at srv but keeps the path the
// build's default release source names, so the repository the check really
// asks for is observable.
func useDefaultReleasePath(t *testing.T, srv *httptest.Server) {
	t.Helper()
	u, err := url.Parse(defaultGitHubAPIURL)
	if err != nil {
		t.Fatal(err)
	}
	orig := githubAPIURL
	githubAPIURL = srv.URL + u.Path
	t.Cleanup(func() { githubAPIURL = orig })
}

func TestCheckLatestIgnoresAHigherUpstreamRelease(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	releases := newReleasesByRepo(t, map[string]string{
		upstreamReleaseRepo: "v99.0.0",
		ReleaseRepo:         "v3.2.0",
	})
	useDefaultReleasePath(t, releases.srv)

	result, err := CheckLatest("v3.2.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if result.UpdateAvailable {
		t.Errorf("UpdateAvailable = true (latest %s): a fork build at its own latest must not be offered an upstream release", result.LatestVersion)
	}
	if result.LatestVersion != "v3.2.0" {
		t.Errorf("LatestVersion = %q, want the fork's v3.2.0", result.LatestVersion)
	}
	if got := releases.asked(); len(got) != 1 || got[0] != ReleaseRepo {
		t.Errorf("update check asked %v, want only %s", got, ReleaseRepo)
	}
	if strings.Contains(result.DownloadURL, upstreamReleaseRepo) || strings.Contains(result.ChecksumURL, upstreamReleaseRepo) {
		t.Errorf("result points at upstream: %q, %q", result.DownloadURL, result.ChecksumURL)
	}
	if cached := ReadCache("v3.2.0"); cached == nil || cached.UpdateAvailable {
		t.Errorf("cache = %+v, want the fork's latest with no update available", cached)
	}
}

func TestCheckLatestDropsAssetsServedFromAnotherRepository(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	// A release response from the fork's endpoint whose assets live elsewhere.
	mux := http.NewServeMux()
	assetName := AssetNameForVersion("v4.0.0")
	mux.HandleFunc("/repos/"+ReleaseRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		base := "https://github.com/" + upstreamReleaseRepo + "/releases/download/v4.0.0/"
		json.NewEncoder(w).Encode(githubRelease{
			TagName: "v4.0.0",
			Assets: []githubAsset{
				{Name: assetName, BrowserDownloadURL: base + assetName},
				{Name: "checksums.txt", BrowserDownloadURL: base + "checksums.txt"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	useDefaultReleasePath(t, srv)

	result, err := CheckLatest("v3.0.0")
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if result.DownloadURL != "" || result.ChecksumURL != "" {
		t.Errorf("selected foreign assets: %q, %q", result.DownloadURL, result.ChecksumURL)
	}
	if err := Apply(result); err == nil {
		t.Error("Apply succeeded without a fork download")
	}
}

func TestApplyRefusesAnUpstreamRelease(t *testing.T) {
	assetName := AssetNameForVersion("v99.0.0")
	base := "https://github.com/" + upstreamReleaseRepo + "/releases/download/v99.0.0/"

	// The same URLs under the fork's repository pass the source check, so the
	// refusal below is about the source and nothing else.
	forkBase := "https://github.com/" + ReleaseRepo + "/releases/download/v99.0.0/"
	if !isReleaseSourceURL(forkBase+assetName) || !isReleaseSourceURL(forkBase+"checksums.txt") {
		t.Fatal("the fork's own release downloads must pass the source check")
	}

	err := Apply(&CheckResult{
		CurrentVersion:  "v3.2.0",
		LatestVersion:   "v99.0.0",
		UpdateAvailable: true,
		DownloadURL:     base + assetName,
		ChecksumURL:     base + "checksums.txt",
	})
	if err == nil {
		t.Fatal("Apply accepted an upstream release")
	}
	if !strings.Contains(err.Error(), "refusing to update") {
		t.Errorf("Apply error = %v, want the source refusal", err)
	}

	// One upstream asset beside a fork asset is still refused.
	err = Apply(&CheckResult{
		DownloadURL: forkBase + assetName,
		ChecksumURL: base + "checksums.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to update") {
		t.Errorf("Apply with an upstream checksum = %v, want the source refusal", err)
	}
}

func TestCacheFromAnotherReleaseSourceIsIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	}

	// ~/.treehouse/update-check.json is shared by every treehouse build on the
	// machine. These are the entries an upstream build, and a fork build from
	// before the source was recorded, leave behind.
	for name, body := range map[string]string{
		"upstream build":       fmt.Sprintf(`{"checked_at":%q,"latest_version":"v9.9.9","source":%q}`, time.Now().Format(time.RFC3339), upstreamReleaseRepo),
		"entry without source": fmt.Sprintf(`{"checked_at":%q,"latest_version":"v9.9.9"}`, time.Now().Format(time.RFC3339)),
	} {
		if err := os.MkdirAll(filepath.Dir(cachePath()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadCache("v3.2.0"); got != nil {
			t.Errorf("%s: ReadCache = %+v, want nil so no upstream notice is shown", name, got)
		}
		if !IsCacheStale("v3.2.0") {
			t.Errorf("%s: cache should be stale so the fork's releases are checked", name)
		}
	}
}
