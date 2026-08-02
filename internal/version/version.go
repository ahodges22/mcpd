// Package version reports the mcpd build version, advertised to backends over MCP
// and to the clients mcpd serves.
package version

import "runtime/debug"

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
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
