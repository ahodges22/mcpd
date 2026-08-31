// Package version reports the mcpd build version, advertised to backends over MCP
// and to the clients mcpd serves.
package version

import "runtime/debug"

const modulePath = "github.com/ahodges22/mcpd"

// stamped is set by the release build with
// -ldflags "-X github.com/ahodges22/mcpd/internal/version.stamped=<version>".
// It is empty in a plain go build or go run.
var stamped string

// String returns the release version when one was stamped in, then the module
// version recorded by go install module@version, and "dev" for an unstamped local
// build.
func String() string {
	if stamped != "" {
		return stamped
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		return buildInfoVersion(info)
	}
	return "dev"
}

func buildInfoVersion(info *debug.BuildInfo) string {
	if info.Main.Path == modulePath {
		if v := releaseVersion(info.Main.Version); v != "" {
			return v
		}
	}
	for _, dependency := range info.Deps {
		if dependency.Path == modulePath {
			if v := releaseVersion(dependency.Version); v != "" {
				return v
			}
		}
	}
	return "dev"
}

func releaseVersion(version string) string {
	if version == "" || version == "(devel)" {
		return ""
	}
	return version
}
