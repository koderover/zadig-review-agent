package gitdiff

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Client struct {
	Dir string
}

type Mode string

const (
	ModeWorkspace Mode = "workspace"
	ModeCommit    Mode = "commit"
	ModeRange     Mode = "range"
)

type Request struct {
	Mode                   Mode
	From                   string
	To                     string
	Commit                 string
	ContextLines           int
	ContextLinesConfigured bool
}

type FileDiff struct {
	Path        string
	OldPath     string
	IsBinary    bool
	IsDeleted   bool
	IsRenamed   bool
	IsUntracked bool
	Insertions  int
	Deletions   int
	Hunks       []Hunk
}

type Hunk struct {
	OldStart     int
	OldLines     int
	NewStart     int
	NewLines     int
	Lines        []Line
	ChangedLines map[int]bool
}

type Line struct {
	Kind    byte
	OldLine int
	NewLine int
	Text    string
}

func (c Client) Head(ctx context.Context) (string, error) {
	return c.git(ctx, "rev-parse", "HEAD")
}

func (c Client) Root(ctx context.Context) (string, error) {
	root, err := c.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(root), nil
}

func (c Client) Diff(ctx context.Context, req Request) ([]FileDiff, error) {
	switch req.Mode {
	case "", ModeWorkspace:
		return c.diffWorkspace(ctx, req.ContextLines, req.ContextLinesConfigured)
	case ModeCommit:
		if strings.TrimSpace(req.Commit) == "" {
			return nil, fmt.Errorf("commit mode requires commit")
		}
		out, err := c.git(ctx, "-c", "core.quotepath=false", "show", "--no-ext-diff", "--no-textconv", "--format=", "--no-color", unifiedArg(req.ContextLines, req.ContextLinesConfigured), "--find-renames", "--end-of-options", req.Commit, "--")
		if err != nil {
			return nil, fmt.Errorf("git show: %w", err)
		}
		return ParseUnifiedDiff(out)
	case ModeRange:
		if strings.TrimSpace(req.From) == "" || strings.TrimSpace(req.To) == "" {
			return nil, fmt.Errorf("range mode requires from and to")
		}
		mergeBase, err := c.git(ctx, "merge-base", "--end-of-options", req.From, req.To)
		if err != nil {
			return nil, fmt.Errorf("resolve merge base: %w", err)
		}
		out, err := c.git(ctx, "-c", "core.quotepath=false", "diff", "--no-ext-diff", "--no-textconv", "--no-color", unifiedArg(req.ContextLines, req.ContextLinesConfigured), "--find-renames", "--end-of-options", strings.TrimSpace(mergeBase), req.To, "--")
		if err != nil {
			return nil, fmt.Errorf("git diff: %w", err)
		}
		return ParseUnifiedDiff(out)
	default:
		return nil, fmt.Errorf("unsupported diff mode %q", req.Mode)
	}
}

func (c Client) diffWorkspace(ctx context.Context, contextLines int, configured bool) ([]FileDiff, error) {
	args := []string{"-c", "core.quotepath=false", "diff", "--no-ext-diff", "--no-textconv", "--no-color", unifiedArg(contextLines, configured), "--find-renames"}
	tracked, err := c.git(ctx, append(args, "--end-of-options", "HEAD", "--")...)
	if err != nil {
		// An unborn repository has no HEAD. Its index is still reviewable.
		tracked, err = c.git(ctx, append(args, "--cached", "--")...)
		if err != nil {
			return nil, fmt.Errorf("git diff workspace: %w", err)
		}
	}
	untracked, err := c.untrackedDiff(ctx)
	if err != nil {
		return nil, err
	}
	files, err := ParseUnifiedDiff(tracked + "\n" + untracked)
	if err != nil {
		return nil, err
	}
	for i := range files {
		if files[i].OldPath == "/dev/null" {
			files[i].IsUntracked = true
		}
	}
	return files, nil
}

func unifiedArg(contextLines int, configured bool) string {
	if !configured {
		contextLines = 3
	}
	return fmt.Sprintf("--unified=%d", contextLines)
}

func (c Client) untrackedDiff(ctx context.Context) (string, error) {
	out, err := c.git(ctx, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return "", fmt.Errorf("git ls-files --others: %w", err)
	}
	var b strings.Builder
	for _, path := range strings.Split(out, "\x00") {
		if path == "" {
			continue
		}
		full := filepath.Join(c.Dir, filepath.FromSlash(path))
		info, err := os.Lstat(full)
		if err != nil {
			return "", fmt.Errorf("stat untracked file %s: %w", path, err)
		}
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(full)
			if readErr != nil {
				return "", fmt.Errorf("read untracked symlink %s: %w", path, readErr)
			}
			data = []byte(target)
		} else {
			data, err = os.ReadFile(full)
		}
		if err != nil {
			return "", fmt.Errorf("read untracked file %s: %w", path, err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			fmt.Fprintf(&b, "diff --git %s %s\nnew file mode 100644\nindex 0000000..0000000\nBinary files /dev/null and %s differ\n", quoteDiffPath("a/", path), quoteDiffPath("b/", path), quoteDiffPath("b/", path))
			continue
		}
		lines := strings.SplitAfter(string(data), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		fmt.Fprintf(&b, "diff --git %s %s\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ %s\n@@ -0,0 +1,%d @@\n", quoteDiffPath("a/", path), quoteDiffPath("b/", path), quoteDiffPath("b/", path), len(lines))
		for _, line := range lines {
			fmt.Fprintf(&b, "+%s", line)
			if !strings.HasSuffix(line, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

func quoteDiffPath(prefix, path string) string {
	return strconv.Quote(prefix + path)
}

func (c Client) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = c.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return string(out), nil
}

func ParseUnifiedDiff(diff string) ([]FileDiff, error) {
	var files []FileDiff
	var cur *FileDiff
	var hunk *Hunk
	oldLine, newLine := 0, 0

	for _, raw := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			if cur != nil {
				files = append(files, *cur)
			}
			cur = &FileDiff{}
			hunk = nil
			oldPath, newPath, ok := parseDiffHeader(raw)
			if ok {
				cur.OldPath = oldPath
				cur.Path = newPath
			}
		case cur == nil:
			continue
		case strings.HasPrefix(raw, "Binary files "):
			cur.IsBinary = true
		case strings.HasPrefix(raw, "deleted file mode "):
			cur.IsDeleted = true
		case strings.HasPrefix(raw, "rename from "):
			cur.IsRenamed = true
			cur.OldPath = unquoteGitPath(strings.TrimSpace(strings.TrimPrefix(raw, "rename from ")))
		case strings.HasPrefix(raw, "rename to "):
			cur.IsRenamed = true
			cur.Path = unquoteGitPath(strings.TrimSpace(strings.TrimPrefix(raw, "rename to ")))
		case strings.HasPrefix(raw, "--- "):
			path := trimDiffPath(strings.TrimSpace(strings.TrimPrefix(raw, "--- ")))
			cur.OldPath = path
			if path == "/dev/null" {
				cur.IsUntracked = true
			}
		case strings.HasPrefix(raw, "+++ "):
			path := trimDiffPath(strings.TrimSpace(strings.TrimPrefix(raw, "+++ ")))
			if path == "/dev/null" {
				cur.IsDeleted = true
			} else {
				cur.Path = path
			}
		case strings.HasPrefix(raw, "@@ "):
			parsed, err := parseHunkHeader(raw)
			if err != nil {
				return nil, err
			}
			cur.Hunks = append(cur.Hunks, parsed)
			hunk = &cur.Hunks[len(cur.Hunks)-1]
			oldLine = hunk.OldStart
			newLine = hunk.NewStart
		case hunk != nil && raw != "":
			kind := raw[0]
			if kind != ' ' && kind != '+' && kind != '-' && kind != '\\' {
				continue
			}
			line := Line{Kind: kind, Text: strings.TrimPrefix(raw, string(kind))}
			switch kind {
			case ' ':
				line.OldLine = oldLine
				line.NewLine = newLine
				oldLine++
				newLine++
			case '-':
				line.OldLine = oldLine
				oldLine++
				cur.Deletions++
			case '+':
				line.NewLine = newLine
				hunk.ChangedLines[newLine] = true
				newLine++
				cur.Insertions++
			case '\\':
				line.Text = raw
			}
			hunk.Lines = append(hunk.Lines, line)
		}
	}
	if cur != nil {
		files = append(files, *cur)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func parseHunkHeader(header string) (Hunk, error) {
	parts := strings.Split(header, " ")
	if len(parts) < 3 {
		return Hunk{}, fmt.Errorf("invalid hunk header %q", header)
	}
	oldStart, oldLines, err := parseRange(parts[1], '-')
	if err != nil {
		return Hunk{}, fmt.Errorf("invalid hunk header %q: %w", header, err)
	}
	newStart, newLines, err := parseRange(parts[2], '+')
	if err != nil {
		return Hunk{}, fmt.Errorf("invalid hunk header %q: %w", header, err)
	}
	return Hunk{
		OldStart:     oldStart,
		OldLines:     oldLines,
		NewStart:     newStart,
		NewLines:     newLines,
		ChangedLines: map[int]bool{},
	}, nil
}

func parseRange(s string, prefix byte) (int, int, error) {
	if s == "" || s[0] != prefix {
		return 0, 0, fmt.Errorf("range must start with %q", prefix)
	}
	s = s[1:]
	chunks := strings.SplitN(s, ",", 2)
	start, err := strconv.Atoi(chunks[0])
	if err != nil {
		return 0, 0, err
	}
	lines := 1
	if len(chunks) == 2 {
		lines, err = strconv.Atoi(chunks[1])
		if err != nil {
			return 0, 0, err
		}
	}
	return start, lines, nil
}

func parseDiffHeader(line string) (string, string, bool) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if rest == line || rest == "" {
		return "", "", false
	}
	if strings.HasPrefix(rest, "\"") {
		first, remaining, ok := consumeQuotedPath(rest)
		if !ok {
			return "", "", false
		}
		second, remaining, ok := consumeQuotedPath(strings.TrimSpace(remaining))
		if !ok || strings.TrimSpace(remaining) != "" {
			return "", "", false
		}
		return trimDiffPath(first), trimDiffPath(second), true
	}
	separator := strings.LastIndex(rest, " b/")
	if separator < 0 || !strings.HasPrefix(rest, "a/") {
		return "", "", false
	}
	return trimDiffPath(rest[:separator]), trimDiffPath(rest[separator+1:]), true
}

func consumeQuotedPath(value string) (string, string, bool) {
	if len(value) < 2 || value[0] != '"' {
		return "", value, false
	}
	escaped := false
	for index := 1; index < len(value); index++ {
		switch {
		case escaped:
			escaped = false
		case value[index] == '\\':
			escaped = true
		case value[index] == '"':
			quoted := value[:index+1]
			unquoted, err := strconv.Unquote(quoted)
			if err != nil {
				return "", value, false
			}
			return unquoted, value[index+1:], true
		}
	}
	return "", value, false
}

func trimDiffPath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"") {
		if unquoted, err := strconv.Unquote(path); err == nil {
			path = unquoted
		}
	}
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	return filepath.ToSlash(path)
}

func unquoteGitPath(path string) string {
	if strings.HasPrefix(path, "\"") && strings.HasSuffix(path, "\"") {
		if unquoted, err := strconv.Unquote(path); err == nil {
			path = unquoted
		}
	}
	return filepath.ToSlash(path)
}
