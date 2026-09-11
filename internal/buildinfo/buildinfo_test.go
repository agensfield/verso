package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestGoInstallMetadataFallback(t *testing.T) {
	oldVersion, oldKind, oldRead := version, installKind, readBuildInfo
	t.Cleanup(func() { version, installKind, readBuildInfo = oldVersion, oldKind, oldRead })
	version, installKind = "dev", ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.1.0-alpha.1"}}, true
	}
	if got := Version(); got != "v0.1.0-alpha.1" {
		t.Fatalf("Version() = %q", got)
	}
	if got := InstallKind(); got != "go" {
		t.Fatalf("InstallKind() = %q", got)
	}
}

func TestLocalBuildMetadataIsUnknown(t *testing.T) {
	oldVersion, oldKind, oldRead := version, installKind, readBuildInfo
	t.Cleanup(func() { version, installKind, readBuildInfo = oldVersion, oldKind, oldRead })
	version, installKind = "dev", ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, true
	}
	if got := Version(); got != "dev" {
		t.Fatalf("Version() = %q", got)
	}
	if got := InstallKind(); got != "unknown" {
		t.Fatalf("InstallKind() = %q", got)
	}
}
