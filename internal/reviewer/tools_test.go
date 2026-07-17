package reviewer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/koderover/zadig-code-review-agent/internal/gitdiff"
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
		return
	}
	t.Fatal("code_comment tool definition missing")
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
