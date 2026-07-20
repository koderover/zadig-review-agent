package version

import (
	"fmt"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func String() string {
	info, ok := debug.ReadBuildInfo()
	buildVersion, commit, date := resolveBuildInfo(Version, Commit, Date, info, ok)
	if buildVersion != "dev" && commit == "unknown" && date == "unknown" {
		return fmt.Sprintf("zadig-review-agent %s", buildVersion)
	}
	return fmt.Sprintf("zadig-review-agent %s (commit %s, built %s)", buildVersion, shortCommit(commit), date)
}

func resolveBuildInfo(version, commit, date string, info *debug.BuildInfo, ok bool) (string, string, string) {
	if !ok || version != "dev" || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return version, commit, date
	}

	version = info.Main.Version
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if commit == "unknown" && setting.Value != "" {
				commit = setting.Value
			}
		case "vcs.time":
			if date == "unknown" && setting.Value != "" {
				date = setting.Value
			}
		}
	}

	return version, commit, date
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
