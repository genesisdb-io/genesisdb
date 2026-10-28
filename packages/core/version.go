package core

import "runtime/debug"

// Version is the GenesisDB release version. Release builds set it with
//
//	go build -ldflags "-X github.com/genesisdb-io/genesisdb/v2/packages/core.Version=2.0.1"
//
// A build without the flag reports the module version recorded by the Go
// toolchain ("v2.0.1" for go install ...@v2.0.1, "(devel)" for a checkout),
// so an embedded user gets a meaningful version without any build setup.
var Version = ""

// ReleaseChannel is "stable" for a release build and "development" for every
// other build. Release builds set it with the same -X flag.
var ReleaseChannel = ""

func init() {
	if Version == "" {
		Version = moduleVersion()
	}
	if ReleaseChannel == "" {
		ReleaseChannel = "development"
	}
}

// moduleVersion returns the version the Go toolchain recorded for this
// module, or "0.0.0-dev" when it recorded none.
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "0.0.0-dev"
	}
	version := info.Main.Version
	if len(version) > 1 && version[0] == 'v' {
		version = version[1:]
	}
	return version
}
