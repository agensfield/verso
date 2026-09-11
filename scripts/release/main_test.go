package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRunBuildsFourDeterministicArtifactsWithMetadata(t *testing.T) {
	oldBuild := buildBinary
	t.Cleanup(func() { buildBinary = oldBuild })
	var calls []string
	buildBinary = func(path, version, commit string, target target) error {
		calls = append(calls, target.os+"/"+target.arch+":"+version+":"+commit+":release")
		return os.WriteFile(path, []byte(target.os+"/"+target.arch), 0o755)
	}
	output := t.TempDir()
	if err := run("v0.1.0-alpha.1", "abcdef0", output); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"checksums.txt", "verso_0.1.0-alpha.1_darwin_amd64.tar.gz", "verso_0.1.0-alpha.1_darwin_arm64.tar.gz", "verso_0.1.0-alpha.1_linux_amd64.tar.gz", "verso_0.1.0-alpha.1_linux_arm64.tar.gz"}
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("artifacts = %v", names)
	}
	if len(calls) != 4 {
		t.Fatalf("build calls = %v", calls)
	}
	for _, call := range calls {
		if !strings.HasSuffix(call, ":0.1.0-alpha.1:abcdef0:release") {
			t.Fatalf("metadata call = %q", call)
		}
	}
	for _, name := range names[1:] {
		if name == "checksums.txt" {
			continue
		}
		assertArchive(t, filepath.Join(output, name))
	}
}

func TestReleaseInputGuards(t *testing.T) {
	for _, tt := range []struct{ version, commit string }{{"latest", "abcdef0"}, {"0.1.0", "ABCDEF0"}, {"0.1", "abcdef0"}, {"01.0.0", "abcdef0"}, {"0.1.0-alpha.01", "abcdef0"}} {
		if err := run(tt.version, tt.commit, t.TempDir()); err == nil {
			t.Fatalf("run(%q, %q) succeeded", tt.version, tt.commit)
		}
	}
}

func assertArchive(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "verso" || header.Typeflag != tar.TypeReg || header.Mode != 0o755 {
		t.Fatalf("header = %#v", header)
	}
	if _, err := tr.Next(); err == nil {
		t.Fatal("unexpected extra archive entry")
	}
}
