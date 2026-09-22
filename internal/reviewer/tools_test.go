package reviewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/koderover/zadig-review-agent/internal/gitdiff"
)

func TestReadOnlyToolsAndPathBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package p\nfunc target() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init")
	gitIn(t, root, "add", "src/main.go")
	executor := newToolExecutor(root, gitdiff.Request{Mode: gitdiff.ModeWorkspace})
	if got := executor.fileRead(context.Background(), "../secret", 1, 10); !strings.Contains(got, "invalid repository path") {
		t.Fatalf("path traversal was not rejected: %s", got)
	}
	if got := executor.fileRead(context.Background(), "src/main.go", 1, 10); !strings.Contains(got, "func target") {
		t.Fatalf("file_read failed: %s", got)
	}
	if got := executor.codeSearch(context.Background(), "target()", []string{"*.go"}, false, false); !strings.Contains(got, "File: src/main.go") || !strings.Contains(got, "2|func target") {
		t.Fatalf("code_search failed: %s", got)
	}
	if got := executor.fileFind(context.Background(), "main", false); !strings.Contains(got, "src/main.go") {
		t.Fatalf("file_find failed: %s", got)
	}
}

func TestCodeSearchSupportsFixedStringAlternativesAndLiteralPipes(t *testing.T) {
	root := t.TempDir()
	content := "package p\nfunc PublishAIReviewReport() {}\nfunc UpsertAIReviewComment() {}\nconst expression = left | right\nfunc formatAIReviewComment() {}\nconst configName = \"config.json\"\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init")
	gitIn(t, root, "add", "main.go")
	executor := newToolExecutor(root, gitdiff.Request{Mode: gitdiff.ModeWorkspace})

	alternatives := executor.codeSearch(context.Background(), "PublishAIReviewReport|UpsertAIReviewComment", []string{"*.go"}, true, false)
	if !strings.Contains(alternatives, "2|func PublishAIReviewReport") || !strings.Contains(alternatives, "3|func UpsertAIReviewComment") || !strings.Contains(alternatives, "Match lines: 2") {
		t.Fatalf("fixed-string alternatives did not use OR semantics:\n%s", alternatives)
	}
	literal := executor.codeSearch(context.Background(), `left \| right`, []string{"*.go"}, true, false)
	if !strings.Contains(literal, "4|const expression = left | right") || !strings.Contains(literal, "Match lines: 1") {
		t.Fatalf("escaped pipe was not searched literally:\n%s", literal)
	}
	regexEscapes := executor.codeSearch(context.Background(), `formatAIReviewComment\(|config\.json`, []string{"*.go"}, true, false)
	if !strings.Contains(regexEscapes, "5|func formatAIReviewComment") || !strings.Contains(regexEscapes, `6|const configName = "config.json"`) || !strings.Contains(regexEscapes, "Match lines: 2") {
		t.Fatalf("redundant regex escapes were not normalized in fixed-string mode:\n%s", regexEscapes)
	}
	if got := fixedSearchAlternatives(` Foo | Bar | Foo | left \| right | Call\( | config\.json `); len(got) != 5 || got[0] != "Foo" || got[1] != "Bar" || got[2] != "left | right" || got[3] != "Call(" || got[4] != "config.json" {
		t.Fatalf("unexpected normalized alternatives: %#v", got)
	}
}

func TestCodeSearchRequestsCorrectionForInvalidPerlRegexp(t *testing.T) {
	root := t.TempDir()
	content := "package p\nfunc StreamServiceLogs() {}\nfunc CollectServiceLogs() {}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init")
	gitIn(t, root, "add", "main.go")
	executor := newToolExecutor(root, gitdiff.Request{Mode: gitdiff.ModeWorkspace})
	got := executor.codeSearch(context.Background(), "StreamServiceLogs(|CollectServiceLogs(", []string{"*.go"}, true, true)
	if !strings.HasPrefix(got, "error: invalid regular expression:") || !strings.Contains(got, "missing closing parenthesis") || !strings.Contains(got, "Regenerate a valid PCRE pattern") {
		t.Fatalf("invalid regex did not request a correction:\n%s", got)
	}
	corrected := executor.codeSearch(context.Background(), `StreamServiceLogs\(|CollectServiceLogs\(`, []string{"*.go"}, true, true)
	if !strings.Contains(corrected, "2|func StreamServiceLogs") || !strings.Contains(corrected, "3|func CollectServiceLogs") {
		t.Fatalf("corrected regex did not find the functions:\n%s", corrected)
	}
}

func TestFileReadRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := newToolExecutor(root, gitdiff.Request{Mode: gitdiff.ModeWorkspace}).fileRead(context.Background(), "link.txt", 1, 10)
	if !strings.Contains(got, "path escapes repository") {
		t.Fatalf("symlink escape was not rejected: %s", got)
	}
}

func TestToolExecutionTruncatesOutputAndRecordsErrorStatus(t *testing.T) {
	root := t.TempDir()
	large := strings.Repeat("界", maxToolOutputBytes)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := newToolExecutor(root, gitdiff.Request{Mode: gitdiff.ModeWorkspace})
	result := executor.execute(context.Background(), toolAction{Tool: "file_read", FilePath: "large.txt", StartLine: 1, EndLine: 1})
	if !result.Truncated || len(result.Output) > maxToolOutputBytes || !strings.Contains(result.Output, "result truncated") || result.OutputBytes <= maxToolOutputBytes {
		t.Fatalf("unexpected truncated result: %+v output_len=%d", result, len(result.Output))
	}
	if !utf8.ValidString(result.Output) {
		t.Fatal("truncated tool output must remain valid UTF-8")
	}
	errorResult := executor.execute(context.Background(), toolAction{Tool: "file_read", FilePath: "missing.txt"})
	if errorResult.Status != "error" || !strings.HasPrefix(errorResult.Summary, "error:") {
		t.Fatalf("unexpected error result: %+v", errorResult)
	}
}

func TestToolsReadReviewedCommitSnapshot(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init")
	gitIn(t, root, "config", "user.email", "test@example.com")
	gitIn(t, root, "config", "user.name", "Test")
	gitIn(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nconst snapshotMarker = \"reviewed\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "main.go")
	gitIn(t, root, "commit", "-m", "reviewed")
	ref := strings.TrimSpace(gitOutputIn(t, root, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nconst snapshotMarker = \"workspace\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := newToolExecutor(root, gitdiff.Request{Mode: gitdiff.ModeCommit, Commit: ref})
	read := executor.fileRead(context.Background(), "main.go", 1, 10)
	if !strings.Contains(read, `"reviewed"`) || strings.Contains(read, `"workspace"`) || !strings.Contains(read, "Total lines: 2") {
		t.Fatalf("file_read did not use reviewed snapshot:\n%s", read)
	}
	search := executor.codeSearch(context.Background(), "snapshotMarker", []string{"*.go"}, false, false)
	if !strings.Contains(search, "File: main.go") || !strings.Contains(search, "Match lines: 1") {
		t.Fatalf("code_search did not use reviewed snapshot:\n%s", search)
	}
	if found := executor.fileFind(context.Background(), "MAIN", false); found != "main.go" {
		t.Fatalf("file_find did not search reviewed snapshot: %s", found)
	}
}

func TestChangedDiffReadBatchesOnlyOtherReviewedFiles(t *testing.T) {
	current := gitdiff.FileDiff{Path: "main.go"}
	first := gitdiff.FileDiff{Path: "first.go", Hunks: []gitdiff.Hunk{{
		OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1,
		Lines:        []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const first = true"}},
		ChangedLines: map[int]bool{1: true},
	}}}
	second := gitdiff.FileDiff{Path: "second.go", Hunks: []gitdiff.Hunk{{
		OldStart: 2, OldLines: 1, NewStart: 2, NewLines: 1,
		Lines:        []gitdiff.Line{{Kind: '+', NewLine: 2, Text: "const second = true"}},
		ChangedLines: map[int]bool{2: true},
	}}}
	executor := newToolExecutor(t.TempDir(), gitdiff.Request{Mode: gitdiff.ModeWorkspace}).withChangedDiffs(current.Path, []gitdiff.FileDiff{current, first, second})
	result := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: []string{"first.go", "second.go", "first.go"}})
	if result.Status != "success" || result.Summary != "2 changed diffs read" || strings.Count(result.Output, "==== CHANGED DIFF:") != 2 || !strings.Contains(result.Output, "const first = true") || !strings.Contains(result.Output, "const second = true") {
		t.Fatalf("unexpected changed diff result: %+v", result)
	}
	for _, paths := range [][]string{{"main.go"}, {"missing.go"}, {"../escape.go"}, nil} {
		got := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: paths})
		if got.Status != "error" {
			t.Fatalf("changed_diff_read accepted invalid paths %v: %+v", paths, got)
		}
	}
}

func TestChangedDiffReadReturnsValidPathsWhenBatchContainsRejectedPaths(t *testing.T) {
	current := gitdiff.FileDiff{Path: "main.go"}
	valid := gitdiff.FileDiff{Path: "valid.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const valid = true"}}, ChangedLines: map[int]bool{1: true},
	}}}
	executor := newToolExecutor(t.TempDir(), gitdiff.Request{Mode: gitdiff.ModeWorkspace}).withChangedDiffs(current.Path, []gitdiff.FileDiff{current, valid})
	result := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: []string{"missing.go", "valid.go", "../escape.go", "main.go"}})
	if result.Status != "success" || result.Summary != "1 changed diffs read" || !strings.Contains(result.Output, "const valid = true") || !strings.Contains(result.Output, `"missing.go": file is not part of the reviewed change`) || !strings.Contains(result.Output, `"../escape.go": invalid repository-relative path`) || !strings.Contains(result.Output, `"main.go": current file diff is already present`) {
		t.Fatalf("mixed changed-diff batch did not preserve valid results and rejected-path diagnostics: %+v", result)
	}
	if !executor.changedDiffsRead["valid.go"] {
		t.Fatal("valid path was not marked as read after partial success")
	}
}

func TestChangedDiffReadOnlyInjectsEachPathOncePerSession(t *testing.T) {
	current := gitdiff.FileDiff{Path: "main.go"}
	first := gitdiff.FileDiff{Path: "first.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const first = true"}}, ChangedLines: map[int]bool{1: true},
	}}}
	second := gitdiff.FileDiff{Path: "second.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const second = true"}}, ChangedLines: map[int]bool{1: true},
	}}}
	executor := newToolExecutor(t.TempDir(), gitdiff.Request{Mode: gitdiff.ModeWorkspace}).withChangedDiffs(current.Path, []gitdiff.FileDiff{current, first, second})
	firstResult := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: []string{"first.go"}})
	if firstResult.Status != "success" || !strings.Contains(firstResult.Output, "const first = true") {
		t.Fatalf("first diff read failed: %+v", firstResult)
	}
	secondResult := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: []string{"first.go", "second.go"}})
	if secondResult.Status != "success" || strings.Contains(secondResult.Output, "const first = true") || !strings.Contains(secondResult.Output, "const second = true") || secondResult.Summary != "1 changed diffs read" {
		t.Fatalf("overlapping request did not return only the unread path: %+v", secondResult)
	}
	repeated := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: []string{"first.go", "second.go"}})
	if repeated.Status != "success" || !strings.Contains(repeated.Output, "already provided") || executor.hasUnreadChangedDiffs() {
		t.Fatalf("repeated paths should not be injected again: %+v", repeated)
	}
}

func TestChangedDiffReadHasCumulativeSessionLimit(t *testing.T) {
	current := gitdiff.FileDiff{Path: "main.go"}
	large := gitdiff.FileDiff{Path: "large.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: strings.Repeat("x", maxChangedDiffSessionBytes*2)}}, ChangedLines: map[int]bool{1: true},
	}}}
	other := gitdiff.FileDiff{Path: "other.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const other = true"}}, ChangedLines: map[int]bool{1: true},
	}}}
	executor := newToolExecutor(t.TempDir(), gitdiff.Request{Mode: gitdiff.ModeWorkspace}).withChangedDiffs(current.Path, []gitdiff.FileDiff{current, large, other})
	result := executor.execute(context.Background(), toolAction{Tool: "changed_diff_read", FilePaths: []string{"large.go", "other.go"}})
	if result.Status != "success" || !result.Truncated || executor.changedDiffBytes != maxChangedDiffSessionBytes || executor.hasUnreadChangedDiffs() || strings.Contains(result.Output, "const other = true") {
		t.Fatalf("cumulative changed-diff limit was not enforced: result=%+v bytes=%d", result, executor.changedDiffBytes)
	}
}

func TestCodeCommentCategoryUsesClosedEnum(t *testing.T) {
	definitions, err := loadToolDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if definition.Name != "code_comment" {
			continue
		}
		properties, ok := definition.Parameters["properties"].(map[string]any)
		if !ok {
			t.Fatalf("code_comment properties missing: %+v", definition.Parameters)
		}
		finding, ok := properties["finding"].(map[string]any)
		if !ok {
			t.Fatalf("finding schema missing: %+v", properties)
		}
		findingProperties, ok := finding["properties"].(map[string]any)
		if !ok {
			t.Fatalf("finding properties missing: %+v", finding)
		}
		category, ok := findingProperties["category"].(map[string]any)
		if !ok {
			t.Fatalf("category schema missing: %+v", findingProperties)
		}
		values, ok := category["enum"].([]any)
		if !ok || len(values) != 6 {
			t.Fatalf("category must use the six-value enum: %+v", category)
		}
		findings, ok := properties["findings"].(map[string]any)
		if !ok || findings["minItems"] != float64(1) || findings["maxItems"] != float64(10) {
			t.Fatalf("code_comment findings must support bounded batching: %+v", findings)
		}
		return
	}
	t.Fatal("code_comment tool definition missing")
}

func TestChangedDiffReadToolDefinitionIsBounded(t *testing.T) {
	definitions, err := loadToolDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if definition.Name != "changed_diff_read" {
			continue
		}
		properties, ok := definition.Parameters["properties"].(map[string]any)
		if !ok {
			t.Fatalf("changed_diff_read properties missing: %+v", definition.Parameters)
		}
		paths, ok := properties["file_paths"].(map[string]any)
		if !ok || paths["minItems"] != float64(1) || paths["maxItems"] != float64(10) {
			t.Fatalf("changed_diff_read file_paths must be bounded: %+v", paths)
		}
		return
	}
	t.Fatal("changed_diff_read tool definition missing")
}

func gitIn(t *testing.T, root string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	if output, err := exec.Command("git", commandArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitOutputIn(t *testing.T, root string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(output)
}
