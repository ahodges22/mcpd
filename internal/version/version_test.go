package version

import (
	"runtime/debug"
	"testing"
)

func TestBuildInfoVersionUsesStandaloneMainVersion(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.5.0"}}
	if got := buildInfoVersion(info); got != "v0.5.0" {
		t.Fatalf("version = %q", got)
	}
}

func TestBuildInfoVersionUsesEmbeddedDependencyVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/ahodges22/odo", Version: "v1.3.1"},
		Deps: []*debug.Module{{Path: modulePath, Version: "v0.5.0"}},
	}
	if got := buildInfoVersion(info); got != "v0.5.0" {
		t.Fatalf("version = %q", got)
	}
}

func TestBuildInfoVersionDoesNotReportHostVersionWithoutDependency(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Path: "github.com/ahodges22/odo", Version: "v1.3.1"}}
	if got := buildInfoVersion(info); got != "dev" {
		t.Fatalf("version = %q", got)
	}
}
