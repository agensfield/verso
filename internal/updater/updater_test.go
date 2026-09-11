package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLatestReleaseIncludesAlphaAndComparesSemver(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]release{
			{TagName: "v0.1.0-alpha.2"},
			{TagName: "v0.1.0-alpha.10"},
			{TagName: "v9.0.0", Draft: true},
		})
	}))
	defer server.Close()
	rel, _, err := latestRelease(context.Background(), Options{ReleasesURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v0.1.0-alpha.10" {
		t.Fatalf("tag = %q", rel.TagName)
	}
	if got := compareVersions("v0.1.0-alpha.2", rel.TagName); got != StatusOutdated {
		t.Fatalf("status = %s", got)
	}
}

func TestUpdateReplacesResolvedExecutable(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real-verso")
	linkPath := filepath.Join(dir, "verso")
	if err := os.WriteFile(realPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	archive := archiveFixture(t, "verso", tar.TypeReg, []byte("new binary"), false)
	server := releaseServer(t, archive, "")
	defer server.Close()
	result, err := Update(context.Background(), Options{Executable: linkPath, CurrentVersion: "v0.1.0-alpha.1", InstallKind: "release", ReleasesURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated {
		t.Fatal("expected update")
	}
	got, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	if resolved, _ := os.Readlink(linkPath); resolved != realPath {
		t.Fatalf("symlink target changed to %q", resolved)
	}
}

func TestUpdateFailuresKeepOriginal(t *testing.T) {
	tests := []struct {
		name      string
		archive   func(*testing.T) []byte
		checksum  string
		wantError string
	}{
		{name: "checksum mismatch", archive: func(t *testing.T) []byte { return archiveFixture(t, "verso", tar.TypeReg, []byte("new"), false) }, checksum: strings.Repeat("0", 64), wantError: "checksum mismatch"},
		{name: "traversal entry", archive: func(t *testing.T) []byte { return archiveFixture(t, "../verso", tar.TypeReg, []byte("new"), false) }, wantError: "exactly one regular verso"},
		{name: "extra entry", archive: func(t *testing.T) []byte { return archiveFixture(t, "verso", tar.TypeReg, []byte("new"), true) }, wantError: "unexpected entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verso")
			if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			archive := tt.archive(t)
			server := releaseServer(t, archive, tt.checksum)
			defer server.Close()
			_, err := Update(context.Background(), Options{Executable: path, CurrentVersion: "v0.1.0-alpha.1", InstallKind: "release", ReleasesURL: server.URL})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v", err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != "old" {
				t.Fatalf("original changed to %q", got)
			}
		})
	}
}

func TestInstallOwnershipRefusesHomebrewAndUnknown(t *testing.T) {
	brewPath := filepath.Join(t.TempDir(), "Cellar", "verso", "0.1.0", "bin", "verso")
	if err := os.MkdirAll(filepath.Dir(brewPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brewPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Update(context.Background(), Options{Executable: brewPath, CurrentVersion: "v0.1.0", InstallKind: "release"})
	if err == nil || !strings.Contains(err.Error(), "brew upgrade verso") {
		t.Fatalf("error = %v", err)
	}
	if isHomebrewPath(filepath.Join(t.TempDir(), "Cellar", "verso", "0.1.0", "not-bin", "verso")) {
		t.Fatal("noncanonical Cellar path classified as Homebrew")
	}

	localPath := filepath.Join(t.TempDir(), "verso")
	if err := os.WriteFile(localPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Update(context.Background(), Options{Executable: localPath, CurrentVersion: "dev", InstallKind: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "unknown local build") {
		t.Fatalf("error = %v", err)
	}
}

func TestOfficialAssetsStayBoundToVersoRepository(t *testing.T) {
	rel := release{TagName: "v0.1.0", Assets: []asset{
		{Name: "verso_0.1.0_linux_amd64.tar.gz", URL: "https://github.com/another/repo/releases/download/v0.1.0/verso_0.1.0_linux_amd64.tar.gz"},
		{Name: "checksums.txt", URL: "https://github.com/agensfield/verso/releases/download/v0.1.0/checksums.txt"},
	}}
	_, _, err := releaseAssets(rel, "verso_0.1.0_linux_amd64.tar.gz", "https://api.github.com")
	if err == nil || !strings.Contains(err.Error(), "outside the release origin") {
		t.Fatalf("error = %v", err)
	}
}

func TestOfficialAssetsStayBoundToSelectedTag(t *testing.T) {
	artifactName := "verso_0.1.0_linux_amd64.tar.gz"
	rel := release{TagName: "v0.1.0", Assets: []asset{
		{Name: artifactName, URL: "https://github.com/agensfield/verso/releases/download/v0.0.9/" + artifactName},
		{Name: "checksums.txt", URL: "https://github.com/agensfield/verso/releases/download/v0.1.0/checksums.txt"},
	}}
	_, _, err := releaseAssets(rel, artifactName, "https://api.github.com")
	if err == nil || !strings.Contains(err.Error(), "outside the release origin") {
		t.Fatalf("error = %v", err)
	}
}

func TestChecksumRequiresOneExactArtifact(t *testing.T) {
	valid := strings.Repeat("a", 64)
	for _, tt := range []struct {
		name, data, want string
	}{
		{name: "missing", data: valid + "  other.tar.gz\n", want: "missing checksum"},
		{name: "duplicate", data: valid + "  target.tar.gz\n" + valid + "  target.tar.gz\n", want: "duplicate checksum"},
		{name: "malformed", data: "nope  target.tar.gz\n", want: "invalid checksums.txt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checksumFor([]byte(tt.data), "target.tar.gz")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReplaceRefusesChangedTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "verso")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := resolveTarget(Options{Executable: path, InstallKind: "release"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external replacement"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replace(target, []byte("downloaded")); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "external replacement" {
		t.Fatalf("replacement was overwritten: %q", got)
	}
}

func TestSupportedReleaseTargets(t *testing.T) {
	for _, target := range [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		if !supportedTarget(target[0], target[1]) {
			t.Fatalf("%s/%s unsupported", target[0], target[1])
		}
	}
	if supportedTarget("windows", "amd64") || supportedTarget("linux", "386") {
		t.Fatal("unexpected target support")
	}
}

func releaseServer(t *testing.T, archive []byte, checksum string) *httptest.Server {
	t.Helper()
	name := fmt.Sprintf("verso_0.1.0-alpha.2_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if checksum == "" {
		sum := sha256.Sum256(archive)
		checksum = hex.EncodeToString(sum[:])
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_ = json.NewEncoder(w).Encode([]release{{TagName: "v0.1.0-alpha.2", Assets: []asset{{Name: name, URL: server.URL + "/artifact"}, {Name: "checksums.txt", URL: server.URL + "/checksums"}}}})
		case "/artifact":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%s  %s\n", checksum, name)
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func archiveFixture(t *testing.T, name string, kind byte, body []byte, extra bool) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if extra {
		if err := tw.WriteHeader(&tar.Header{Name: "extra", Typeflag: tar.TypeReg, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
