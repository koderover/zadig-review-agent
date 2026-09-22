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
	"sync"
	"unicode/utf8"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/filter"
	"github.com/koderover/zadig-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-review-agent/internal/sensitive"
)

type toolAction struct {
	Tool          string          `json:"tool"`
	FilePath      string          `json:"file_path,omitempty"`
	FilePaths     []string        `json:"file_paths,omitempty"`
	QueryName     string          `json:"query_name,omitempty"`
	SearchText    string          `json:"search_text,omitempty"`
	FilePatterns  []string        `json:"file_patterns,omitempty"`
	CaseSensitive bool            `json:"case_sensitive,omitempty"`
	UsePerlRegexp bool            `json:"use_perl_regexp,omitempty"`
	StartLine     int             `json:"start_line,omitempty"`
	EndLine       int             `json:"end_line,omitempty"`
	Finding       agent.Finding   `json:"finding,omitempty"`
	Findings      []agent.Finding `json:"findings,omitempty"`
	Path          string          `json:"path,omitempty"`
	Query         string          `json:"query,omitempty"`
	Pattern       string          `json:"pattern,omitempty"`
}

type toolExecutor struct {
	root                 string
	ref                  string
	request              gitdiff.Request
	policy               *contextPathPolicy
	currentFile          string
	changedDiffs         map[string]gitdiff.FileDiff
	changedDiffsRead     map[string]bool
	changedDiffBytes     int
	changedDiffTruncated bool
}

type contextPathPolicy struct {
	once    sync.Once
	blocked map[string]bool
	err     error
}

const maxToolOutputBytes = 32 * 1024
const maxChangedDiffSessionBytes = maxToolOutputBytes

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
	executor := toolExecutor{root: root, ref: ref, request: request}
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		executor.policy = &contextPathPolicy{}
	}
	return executor
}

func (e toolExecutor) blockedContextPaths(ctx context.Context) (map[string]bool, error) {
	if e.policy == nil {
		return nil, nil
	}
	e.policy.once.Do(func() {
		e.policy.blocked, e.policy.err = (gitdiff.Client{Dir: e.root}).BlockedContextPaths(ctx, e.request)
	})
	return e.policy.blocked, e.policy.err
}

func isBlockedContextPath(blocked map[string]bool, path string) bool {
	if blocked[path] {
		return true
	}
	for blockedPath := range blocked {
		if strings.EqualFold(blockedPath, path) {
			return true
		}
	}
	return false
}

func (e toolExecutor) withChangedDiffs(currentFile string, files []gitdiff.FileDiff) toolExecutor {
	e.currentFile = currentFile
	e.changedDiffs = make(map[string]gitdiff.FileDiff, len(files))
	e.changedDiffsRead = make(map[string]bool, len(files))
	for _, file := range files {
		if sensitive.IsPath(file.Path) || sensitive.IsPath(file.OldPath) {
			continue
		}
		e.changedDiffs[file.Path] = file
	}
	return e
}

func (e *toolExecutor) execute(ctx context.Context, action toolAction) toolExecution {
	var output string
	switch action.Tool {
	case "file_read":
		output = e.fileRead(ctx, action.filePath(), action.StartLine, action.EndLine)
	case "changed_diff_read":
		e.changedDiffTruncated = false
		output = e.changedDiffRead(action.FilePaths)
	case "code_search":
		output = e.codeSearch(ctx, action.searchText(), action.FilePatterns, action.CaseSensitive, action.UsePerlRegexp)
	case "file_find":
		output = e.fileFind(ctx, action.queryName(), action.CaseSensitive)
	default:
		output = "error: unsupported tool"
	}
	originalBytes := len(output)
	output, truncated := truncateToolOutput(output, maxToolOutputBytes)
	truncated = truncated || e.changedDiffTruncated
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
	case "changed_diff_read":
		return fmt.Sprintf("%d changed diffs read", strings.Count(trimmed, "==== CHANGED DIFF: "))
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

func (e *toolExecutor) changedDiffRead(paths []string) string {
	if len(paths) == 0 {
		return "error: file_paths must contain at least one changed file"
	}
	if len(paths) > 10 {
		return "error: file_paths may contain at most 10 changed files"
	}
	seen := make(map[string]bool, len(paths))
	cleanPaths := make([]string, 0, len(paths))
	rejected := make([]string, 0)
	for _, path := range paths {
		clean, ok := filter.CleanRelative(path)
		if !ok {
			rejected = append(rejected, fmt.Sprintf("%q: invalid repository-relative path", path))
			continue
		}
		if sensitive.IsPath(clean) {
			rejected = append(rejected, fmt.Sprintf("%q: sensitive file path is blocked", clean))
			continue
		}
		if clean == e.currentFile {
			rejected = append(rejected, fmt.Sprintf("%q: current file diff is already present in the review prompt", clean))
			continue
		}
		if _, ok := e.changedDiffs[clean]; !ok {
			rejected = append(rejected, fmt.Sprintf("%q: file is not part of the reviewed change", clean))
			continue
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		cleanPaths = append(cleanPaths, clean)
	}
	var diffs strings.Builder
	for _, clean := range cleanPaths {
		if e.changedDiffsRead[clean] {
			continue
		}
		file := e.changedDiffs[clean]
		block := fmt.Sprintf("==== CHANGED DIFF: %s ====\n%s", clean, strings.TrimRight(renderFileDiff(file), "\n"))
		separatorBytes := 0
		if diffs.Len() > 0 {
			separatorBytes = 1
		}
		remaining := maxChangedDiffSessionBytes - e.changedDiffBytes
		if remaining <= separatorBytes {
			e.changedDiffBytes = maxChangedDiffSessionBytes
			break
		}
		if len(block)+separatorBytes > remaining {
			if separatorBytes > 0 {
				diffs.WriteByte('\n')
			}
			truncated, _ := truncateToolOutput(block, remaining-separatorBytes)
			diffs.WriteString(truncated)
			e.changedDiffsRead[clean] = true
			e.changedDiffBytes = maxChangedDiffSessionBytes
			e.changedDiffTruncated = true
			break
		}
		if separatorBytes > 0 {
			diffs.WriteByte('\n')
		}
		diffs.WriteString(block)
		e.changedDiffsRead[clean] = true
		e.changedDiffBytes += len(block) + separatorBytes
	}
	if diffs.Len() == 0 {
		if len(cleanPaths) == 0 {
			return "error: no valid changed file paths\n" + formatRejectedChangedDiffPaths(rejected)
		}
		output := "all requested valid changed diffs were already provided; no new diff content"
		if len(rejected) > 0 {
			output += "\n" + formatRejectedChangedDiffPaths(rejected)
		}
		return output
	}
	output := ""
	if len(rejected) > 0 {
		output = formatRejectedChangedDiffPaths(rejected) + "\n\n"
	}
	return output + strings.TrimRight(diffs.String(), "\n")
}

func formatRejectedChangedDiffPaths(rejected []string) string {
	if len(rejected) == 0 {
		return ""
	}
	return "Rejected changed diff paths:\n- " + strings.Join(rejected, "\n- ")
}

func (e *toolExecutor) changedDiffRequestHasUnread(paths []string) bool {
	if len(paths) == 0 || len(paths) > 10 {
		return true
	}
	for _, path := range paths {
		clean, ok := filter.CleanRelative(path)
		if !ok || clean == e.currentFile {
			return true
		}
		if _, ok := e.changedDiffs[clean]; !ok {
			return true
		}
		if !e.changedDiffsRead[clean] && e.changedDiffBytes < maxChangedDiffSessionBytes {
			return true
		}
	}
	return false
}

func (e *toolExecutor) hasUnreadChangedDiffs() bool {
	if e.changedDiffBytes >= maxChangedDiffSessionBytes {
		return false
	}
	for path := range e.changedDiffs {
		if path != e.currentFile && !e.changedDiffsRead[path] {
			return true
		}
	}
	return false
}

func (e *toolExecutor) changedDiffWasUsed() bool {
	return len(e.changedDiffsRead) > 0
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
	if sensitive.IsPath(clean) {
		return "error: sensitive file path is blocked"
	}
	blocked, err := e.blockedContextPaths(ctx)
	if err != nil {
		return "error: inspect sensitive paths: " + err.Error()
	}
	if isBlockedContextPath(blocked, clean) {
		return "error: sensitive rename target is blocked"
	}
	var data []byte
	if e.ref != "" {
		data, err = e.runGit(ctx, "show", e.ref+":"+clean)
	} else {
		var full string
		full, clean, err = e.safePath(clean)
		if err == nil {
			root, rootErr := filepath.Abs(e.root)
			if rootErr == nil {
				root, rootErr = filepath.EvalSymlinks(root)
			}
			if rootErr != nil {
				err = rootErr
			} else if resolved, relErr := filepath.Rel(root, full); relErr != nil {
				err = relErr
			} else if isBlockedContextPath(blocked, filepath.ToSlash(resolved)) {
				return "error: sensitive rename target is blocked"
			} else {
				data, err = os.ReadFile(full)
			}
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
	blocked, err := e.blockedContextPaths(ctx)
	if err != nil {
		return "error: inspect sensitive paths: " + err.Error()
	}
	args := []string{"grep", "-n", "-I", "--max-count=100"}
	if !caseSensitive {
		args = append(args, "-i")
	}
	if usePerlRegexp {
		args = append(args, "-P")
		args = append(args, "-e", searchText)
	} else {
		args = append(args, "-F")
		alternatives := fixedSearchAlternatives(searchText)
		if len(alternatives) == 0 {
			return "error: search_text has no non-empty alternatives"
		}
		for _, alternative := range alternatives {
			args = append(args, "-e", alternative)
		}
	}
	if e.ref != "" {
		args = append(args, e.ref)
	}
	args = append(args, "--")
	if len(patterns) == 0 {
		args = append(args, ".")
	}
	args = append(args, patterns...)
	args = append(args, sensitive.GitExcludePathspecs()...)
	for path := range blocked {
		args = append(args, ":(exclude,literal)"+path)
	}
	data, err := e.runGit(ctx, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "No matches found"
		}
		if usePerlRegexp && strings.Contains(err.Error(), "fatal: -e option,") {
			return "error: invalid regular expression: " + err.Error() + ". Regenerate a valid PCRE pattern and call code_search again with use_perl_regexp=true."
		}
		return "error: " + err.Error()
	}
	return formatSearchMatches(string(data), e.ref)
}

// fixedSearchAlternatives makes the model's common "Foo|Bar" spelling useful
// without silently upgrading fixed-string searches to regular expressions.
// An escaped pipe remains literal, so callers can search source expressions
// such as "left \| right" deterministically.
func fixedSearchAlternatives(searchText string) []string {
	var alternatives []string
	var current strings.Builder
	appendCurrent := func() {
		alternative := strings.TrimSpace(current.String())
		current.Reset()
		if alternative == "" {
			return
		}
		for _, existing := range alternatives {
			if existing == alternative {
				return
			}
		}
		alternatives = append(alternatives, alternative)
	}
	for index := 0; index < len(searchText); index++ {
		if searchText[index] == '\\' && index+1 < len(searchText) {
			next := searchText[index+1]
			if next == '\\' {
				current.WriteByte('\\')
				index++
				continue
			}
			if strings.ContainsRune(`|.^$*+?()[]{}-`, rune(next)) {
				// Models commonly escape regex metacharacters even though the
				// default search mode is fixed-string. The escape is redundant
				// here; stripping it produces the literal text they intended.
				current.WriteByte(next)
				index++
				continue
			}
		}
		if searchText[index] == '|' {
			appendCurrent()
			continue
		}
		current.WriteByte(searchText[index])
	}
	appendCurrent()
	return alternatives
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
	blocked, err := e.blockedContextPaths(ctx)
	if err != nil {
		return "error: inspect sensitive paths: " + err.Error()
	}
	var args []string
	if e.ref != "" {
		args = []string{"ls-tree", "-r", "-z", "--name-only", e.ref}
	} else {
		args = []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard"}
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
	for _, path := range strings.Split(string(data), "\x00") {
		clean, ok := filter.CleanRelative(path)
		if !ok || sensitive.IsPath(clean) || blocked[clean] || !fileFindCandidate(clean) {
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
	if sensitive.IsPath(rel) {
		return "", "", fmt.Errorf("sensitive file path is blocked")
	}
	return resolved, clean, nil
}
