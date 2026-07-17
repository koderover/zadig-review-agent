package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/filter"
	"github.com/koderover/zadig-code-review-agent/internal/gitdiff"
)

type toolAction struct {
	Tool          string        `json:"tool"`
	FilePath      string        `json:"file_path,omitempty"`
	QueryName     string        `json:"query_name,omitempty"`
	SearchText    string        `json:"search_text,omitempty"`
	FilePatterns  []string      `json:"file_patterns,omitempty"`
	CaseSensitive bool          `json:"case_sensitive,omitempty"`
	UsePerlRegexp bool          `json:"use_perl_regexp,omitempty"`
	StartLine     int           `json:"start_line,omitempty"`
	EndLine       int           `json:"end_line,omitempty"`
	Finding       agent.Finding `json:"finding,omitempty"`
	Path          string        `json:"path,omitempty"`
	Query         string        `json:"query,omitempty"`
	Pattern       string        `json:"pattern,omitempty"`
}

type toolExecutor struct {
	root string
	ref  string
}

const maxToolOutputBytes = 32 * 1024

type toolExecution struct {
	Output      string
	OutputBytes int
	Truncated   bool
	Status      string
	Summary     string
	Cached      bool
}

func newToolExecutor(root string, request gitdiff.Request) toolExecutor {
	ref := ""
	switch request.Mode {
	case gitdiff.ModeCommit:
		ref = request.Commit
	case gitdiff.ModeRange:
		ref = request.To
	}
	return toolExecutor{root: root, ref: ref}
}

func (e toolExecutor) execute(ctx context.Context, action toolAction) toolExecution {
	var output string
	switch action.Tool {
	case "file_read":
		output = e.fileRead(ctx, action.filePath(), action.StartLine, action.EndLine)
	case "code_search":
		output = e.codeSearch(ctx, action.searchText(), action.FilePatterns, action.CaseSensitive, action.UsePerlRegexp)
	case "file_find":
		output = e.fileFind(ctx, action.queryName(), action.CaseSensitive)
	default:
		output = "error: unsupported tool"
	}
	originalBytes := len(output)
	output, truncated := truncateToolOutput(output, maxToolOutputBytes)
	status := "success"
	if strings.HasPrefix(output, "error:") {
		status = "error"
	}
	return toolExecution{Output: output, OutputBytes: originalBytes, Truncated: truncated, Status: status, Summary: summarizeToolOutput(action.Tool, output)}
}

func (a toolAction) filePath() string {
	if a.FilePath != "" {
		return a.FilePath
	}
	return a.Path
}

func (a toolAction) searchText() string {
	if a.SearchText != "" {
		return a.SearchText
	}
	return a.Query
}

func (a toolAction) queryName() string {
	if a.QueryName != "" {
		return a.QueryName
	}
	return a.Pattern
}

func truncateToolOutput(output string, limit int) (string, bool) {
	if limit < 1 || len(output) <= limit {
		return output, false
	}
	const marker = "\n... result truncated ..."
	end := limit - len(marker)
	if end < 1 {
		return marker[len(marker)-limit:], true
	}
	for end > 0 && !utf8.ValidString(output[:end]) {
		end--
	}
	return strings.TrimRight(output[:end], "\n") + marker, true
}

func summarizeToolOutput(tool, output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return "empty result"
	}
	if strings.HasPrefix(trimmed, "error:") || trimmed == "no matches" {
		return trimmed
	}
	lineCount := strings.Count(trimmed, "\n") + 1
	switch tool {
	case "code_search":
		return fmt.Sprintf("%d matches", searchMatchCount(trimmed))
	case "file_find":
		return fmt.Sprintf("%d files", lineCount)
	case "file_read":
		return fmt.Sprintf("%d lines read", readLineCount(trimmed))
	}
	firstLine := trimmed
	if index := strings.IndexByte(firstLine, '\n'); index >= 0 {
		firstLine = firstLine[:index]
	}
	if len(firstLine) > 160 {
		firstLine = firstLine[:160] + "..."
	}
	if lineCount == 1 {
		return firstLine
	}
	return fmt.Sprintf("%s (%d lines)", firstLine, lineCount)
}

func searchMatchCount(output string) int {
	total := 0
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "Match lines: ") {
			continue
		}
		count, err := strconv.Atoi(strings.TrimPrefix(line, "Match lines: "))
		if err == nil {
			total += count
		}
	}
	return total
}

func readLineCount(output string) int {
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "LINE_RANGE: ") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(line, "LINE_RANGE: "), "-", 2)
		if len(parts) != 2 {
			break
		}
		start, startErr := strconv.Atoi(parts[0])
		end, endErr := strconv.Atoi(parts[1])
		if startErr == nil && endErr == nil && end >= start {
			return end - start + 1
		}
	}
	return 0
}

func (e toolExecutor) fileRead(ctx context.Context, path string, start, end int) string {
	clean, ok := filter.CleanRelative(path)
	if !ok {
		return "error: invalid repository path"
	}
	var data []byte
	var err error
	if e.ref != "" {
		data, err = e.runGit(ctx, "show", e.ref+":"+clean)
	} else {
		var full string
		full, clean, err = e.safePath(clean)
		if err == nil {
			data, err = os.ReadFile(full)
		}
	}
	if err != nil {
		return "error: " + err.Error()
	}
	if start < 1 {
		start = 1
	}
	content := strings.TrimSuffix(string(data), "\n")
	lines := strings.Split(content, "\n")
	if end < start {
		end = len(lines)
	}
	if start > len(lines) {
		return "error: start_line exceeds file length"
	}
	truncated := false
	if end-start+1 > 500 {
		end = start + 499
		truncated = true
	}
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s (Total lines: %d)\n", clean, len(lines))
	fmt.Fprintf(&b, "IS_TRUNCATED: %t\n", truncated)
	fmt.Fprintf(&b, "LINE_RANGE: %d-%d\n", start, end)
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d|%s\n", i, lines[i-1])
	}
	if truncated {
		b.WriteString("Note: Results truncated to 500 lines. Please narrow your line range.\n")
	}
	return b.String()
}

func (e toolExecutor) codeSearch(ctx context.Context, searchText string, patterns []string, caseSensitive, usePerlRegexp bool) string {
	searchText = strings.TrimSpace(searchText)
	if searchText == "" {
		return "error: search_text is blank"
	}
	args := []string{"grep", "-n", "-I", "--max-count=100"}
	if !caseSensitive {
		args = append(args, "-i")
	}
	if usePerlRegexp {
		args = append(args, "-P")
	} else {
		args = append(args, "-F")
	}
	args = append(args, "-e", searchText)
	if e.ref != "" {
		args = append(args, e.ref)
	}
	args = append(args, "--")
	args = append(args, patterns...)
	data, err := e.runGit(ctx, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "No matches found"
		}
		return "error: " + err.Error()
	}
	return formatSearchMatches(string(data), e.ref)
}

func formatSearchMatches(output, ref string) string {
	groups := map[string][]string{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if ref != "" {
			line = strings.TrimPrefix(line, ref+":")
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if _, exists := groups[parts[0]]; !exists {
			order = append(order, parts[0])
		}
		groups[parts[0]] = append(groups[parts[0]], parts[1]+"|"+parts[2])
	}
	if len(order) == 0 {
		return "No matches found"
	}
	var b strings.Builder
	for _, path := range order {
		fmt.Fprintf(&b, "File: %s\nMatch lines: %d\n%s\n\n", path, len(groups[path]), strings.Join(groups[path], "\n"))
	}
	return strings.TrimSpace(b.String())
}

func (e toolExecutor) fileFind(ctx context.Context, queryName string, caseSensitive bool) string {
	queryName = strings.TrimSpace(queryName)
	if queryName == "" {
		return "// The file was not found"
	}
	var args []string
	if e.ref != "" {
		args = []string{"ls-tree", "-r", "--name-only", e.ref}
	} else {
		args = []string{"ls-files", "--cached", "--others", "--exclude-standard"}
	}
	data, err := e.runGit(ctx, args...)
	if err != nil {
		return "error: " + err.Error()
	}
	wanted := queryName
	if !caseSensitive {
		wanted = strings.ToLower(wanted)
	}
	var matches []string
	for _, path := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		clean, ok := filter.CleanRelative(path)
		if !ok || !fileFindCandidate(clean) {
			continue
		}
		name := filepath.Base(clean)
		if !caseSensitive {
			name = strings.ToLower(name)
		}
		if strings.Contains(name, wanted) {
			matches = append(matches, clean)
			if len(matches) == 100 {
				break
			}
		}
	}
	if len(matches) == 0 {
		return "// The file was not found"
	}
	return strings.Join(matches, "\n")
}

func fileFindCandidate(path string) bool {
	name := filepath.Base(path)
	if filepath.Ext(name) != "" {
		return true
	}
	switch name {
	case "Makefile", "Dockerfile", "LICENSE", "Vagrantfile", "Containerfile":
		return true
	default:
		return false
	}
}

func (e toolExecutor) runGit(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", e.root}, args...)...)
	data, err := cmd.CombinedOutput()
	if err != nil {
		if len(data) > 0 {
			return data, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(data)))
		}
		return data, err
	}
	return data, nil
}

func (e toolExecutor) safePath(path string) (string, string, error) {
	clean, ok := filter.CleanRelative(path)
	if !ok {
		return "", "", fmt.Errorf("invalid repository path")
	}
	root, err := filepath.Abs(e.root)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes repository")
	}
	return resolved, clean, nil
}
