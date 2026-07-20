package version

import (
	"runtime/debug"
	"testing"
)

func TestResolveBuildInfoUsesModuleVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.1.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: "2026-07-20T08:00:00Z"},
		},
	}

	version, commit, date := resolveBuildInfo("dev", "unknown", "unknown", info, true)
	if version != "v0.1.0" {
		t.Fatalf("version = %q, want %q", version, "v0.1.0")
	}
	if commit != "0123456789abcdef" {
		t.Fatalf("commit = %q, want %q", commit, "0123456789abcdef")
	}
	if date != "2026-07-20T08:00:00Z" {
		t.Fatalf("date = %q, want %q", date, "2026-07-20T08:00:00Z")
	}
}

func TestResolveBuildInfoPreservesInjectedValues(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}

	version, commit, date := resolveBuildInfo(
		"v1.2.3",
		"fedcba9876543210",
		"2026-07-21T08:00:00Z",
		info,
		true,
	)
	if version != "v1.2.3" || commit != "fedcba9876543210" || date != "2026-07-21T08:00:00Z" {
		t.Fatalf("got (%q, %q, %q), want injected values", version, commit, date)
	}
}

func TestResolveBuildInfoIgnoresDevelopmentBuild(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
		},
	}

	version, commit, date := resolveBuildInfo("dev", "unknown", "unknown", info, true)
	if version != "dev" || commit != "unknown" || date != "unknown" {
		t.Fatalf("got (%q, %q, %q), want development defaults", version, commit, date)
	}
}
