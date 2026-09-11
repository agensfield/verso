package buildinfo

import "runtime/debug"

const modulePath = "github.com/agensfield/verso"

var (
	version       = "dev"
	commit        = "unknown"
	installKind   = ""
	readBuildInfo = debug.ReadBuildInfo
)

// Version returns the release version injected by the artifact builder, or the
// module version recorded by `go install` when no release metadata is present.
func Version() string {
	if version != "" && version != "dev" {
		return version
	}
	if info, ok := readBuildInfo(); ok && info.Main.Path == modulePath && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// Commit returns the source commit injected into an official release binary.
func Commit() string { return commit }

// InstallKind identifies binaries whose update ownership is known.
func InstallKind() string {
	if installKind != "" {
		return installKind
	}
	if info, ok := readBuildInfo(); ok && info.Main.Path == modulePath && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return "go"
	}
	return "unknown"
}
