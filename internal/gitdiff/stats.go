package gitdiff

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type Stats struct {
	Additions int
	Deletions int
}

// DiffStats counts all changed files without returning their contents.
func (c Client) DiffStats(ctx context.Context, req Request) (Stats, error) {
	flags := []string{"--no-ext-diff", "--no-textconv", "--no-color", "--numstat", "--find-renames"}
	var args []string
	switch req.Mode {
	case "", ModeWorkspace:
		args = append([]string{"diff"}, flags...)
		if _, err := c.Head(ctx); err != nil {
			args = append(args, "--cached", "--")
		} else {
			args = append(args, "--end-of-options", "HEAD", "--")
		}
	case ModeCommit:
		if strings.TrimSpace(req.Commit) == "" {
			return Stats{}, fmt.Errorf("commit mode requires commit")
		}
		args = append([]string{"show", "--format="}, flags...)
		args = append(args, "--end-of-options", req.Commit, "--")
	case ModeRange:
		if strings.TrimSpace(req.From) == "" || strings.TrimSpace(req.To) == "" {
			return Stats{}, fmt.Errorf("range mode requires from and to")
		}
		base, err := c.git(ctx, "merge-base", "--end-of-options", req.From, req.To)
		if err != nil {
			return Stats{}, fmt.Errorf("resolve merge base: %w", err)
		}
		args = append([]string{"diff"}, flags...)
		args = append(args, "--end-of-options", strings.TrimSpace(base), req.To, "--")
	default:
		return Stats{}, fmt.Errorf("unsupported diff mode %q", req.Mode)
	}
	out, err := c.git(ctx, args...)
	if err != nil {
		return Stats{}, err
	}
	stats, err := parseNumstat(out)
	if err != nil {
		return Stats{}, err
	}
	if req.Mode != "" && req.Mode != ModeWorkspace {
		return stats, nil
	}
	untracked, err := c.git(ctx, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return Stats{}, err
	}
	for _, path := range strings.Split(untracked, "\x00") {
		if path == "" {
			continue
		}
		args = append([]string{"diff", "--no-index"}, flags...)
		out, err := c.gitOutput(ctx, true, append(args, "--", "/dev/null", path)...)
		if err != nil {
			return Stats{}, err
		}
		fileStats, err := parseNumstat(out)
		if err != nil {
			return Stats{}, err
		}
		stats.Additions += fileStats.Additions
		stats.Deletions += fileStats.Deletions
	}
	return stats, nil
}

func parseNumstat(out string) (Stats, error) {
	var stats Stats
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return Stats{}, fmt.Errorf("invalid git numstat output")
		}
		if fields[0] == "-" && fields[1] == "-" {
			continue
		}
		additions, addErr := strconv.Atoi(fields[0])
		deletions, delErr := strconv.Atoi(fields[1])
		if addErr != nil || delErr != nil || additions < 0 || deletions < 0 {
			return Stats{}, fmt.Errorf("invalid git numstat counts")
		}
		stats.Additions += additions
		stats.Deletions += deletions
	}
	return stats, nil
}
