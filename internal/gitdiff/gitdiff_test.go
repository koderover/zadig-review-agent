package gitdiff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedArg(t *testing.T) {
	if got := unifiedArg(0, false); got != "--unified=3" {
		t.Fatalf("default unified arg = %q", got)
	}
	if got := unifiedArg(0, true); got != "--unified=0" {
		t.Fatalf("configured unified arg = %q", got)
	}
	if got := unifiedArg(30, true); got != "--unified=30" {
		t.Fatalf("configured unified arg = %q", got)
	}
}

func TestParseUnifiedDiffChangedLines(t *testing.T) {
	files, err := ParseUnifiedDiff(`diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
-old
+new
+next
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.go" {
		t.Fatalf("unexpected files: %+v", files)
	}
	if !files[0].Hunks[0].ChangedLines[1] || !files[0].Hunks[0].ChangedLines[2] {
		t.Fatalf("changed lines not mapped: %+v", files[0].Hunks[0].ChangedLines)
	}
}

func TestParseUnifiedDiffPaths(t *testing.T) {
	for _, test := range []struct {
		header string
		old    string
		new    string
	}{
		{`diff --git a/path with spaces/旧.go b/path with spaces/旧.go`, "path with spaces/旧.go", "path with spaces/旧.go"},
		{`diff --git "a/tab\tname.go" "b/tab\tname.go"`, "tab\tname.go", "tab\tname.go"},
	} {
		oldPath, newPath, ok := parseDiffHeader(test.header)
		if !ok || oldPath != test.old || newPath != test.new {
			t.Fatalf("parseDiffHeader(%q) = %q, %q, %t", test.header, oldPath, newPath, ok)
		}
	}
}

func TestParseUnifiedDiffQuotedRenamePaths(t *testing.T) {
	files, err := ParseUnifiedDiff("diff --git \"a/old\\tname.go\" \"b/new\\tname.go\"\nsimilarity index 100%\nrename from \"old\\tname.go\"\nrename to \"new\\tname.go\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || !files[0].IsRenamed || files[0].OldPath != "old\tname.go" || files[0].Path != "new\tname.go" {
		t.Fatalf("unexpected rename: %+v", files)
	}
}

func TestClientDiffRangeCommitAndWorkspace(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	gitIn(t, dir, "add", "main.go")
	gitIn(t, dir, "commit", "-m", "base")
	base := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	write(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	gitIn(t, dir, "add", "main.go")
	gitIn(t, dir, "commit", "-m", "change")
	head := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))

	client := Client{Dir: dir}
	rangeFiles, err := client.Diff(context.Background(), Request{Mode: ModeRange, From: base, To: head})
	if err != nil {
		t.Fatal(err)
	}
	if len(rangeFiles) != 1 || rangeFiles[0].Path != "main.go" || rangeFiles[0].Insertions == 0 {
		t.Fatalf("unexpected range files: %+v", rangeFiles)
	}
	commitFiles, err := client.Diff(context.Background(), Request{Mode: ModeCommit, Commit: head})
	if err != nil {
		t.Fatal(err)
	}
	if len(commitFiles) != 1 || commitFiles[0].Path != "main.go" {
		t.Fatalf("unexpected commit files: %+v", commitFiles)
	}

	write(t, filepath.Join(dir, "untracked.go"), "package main\nvar X = 1\n")
	workspaceFiles, err := client.Diff(context.Background(), Request{Mode: ModeWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	foundUntracked := false
	for _, file := range workspaceFiles {
		if file.Path == "untracked.go" && file.IsUntracked {
			foundUntracked = true
		}
	}
	if !foundUntracked {
		t.Fatalf("workspace diff did not include untracked file: %+v", workspaceFiles)
	}
}

func TestWorkspaceCombinesStagedAndUnstagedForSameFile(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	gitIn(t, dir, "add", "main.go")
	gitIn(t, dir, "commit", "-m", "base")

	write(t, filepath.Join(dir, "main.go"), "package main\nvar Staged = true\n")
	gitIn(t, dir, "add", "main.go")
	write(t, filepath.Join(dir, "main.go"), "package main\nvar Staged = true\nvar Unstaged = true\n")

	files, err := (Client{Dir: dir}).Diff(context.Background(), Request{Mode: ModeWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "main.go" || files[0].Insertions != 2 {
		t.Fatalf("expected one HEAD-to-worktree diff: %+v", files)
	}
}

func TestWorkspaceUntrackedSymlinkDoesNotReadTarget(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	target := filepath.Join(t.TempDir(), "secret.go")
	write(t, target, "SECRET_OUTSIDE_REPOSITORY\n")
	if err := os.Symlink(target, filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	files, err := (Client{Dir: dir}).Diff(context.Background(), Request{Mode: ModeWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "link.go" {
		t.Fatalf("unexpected symlink diff: %+v", files)
	}
	for _, hunk := range files[0].Hunks {
		for _, line := range hunk.Lines {
			if strings.Contains(line.Text, "SECRET_OUTSIDE_REPOSITORY") {
				t.Fatalf("external symlink content leaked into diff: %+v", files[0])
			}
		}
	}
}

func TestWorkspaceWithoutHEADIncludesStagedAndUntrackedPaths(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	write(t, filepath.Join(dir, "staged.go"), "package staged\n")
	gitIn(t, dir, "add", "staged.go")
	name := "space 旧.go"
	write(t, filepath.Join(dir, name), "package untracked\n")

	files, err := (Client{Dir: dir}).Diff(context.Background(), Request{Mode: ModeWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range files {
		paths[file.Path] = true
	}
	if !paths["staged.go"] || !paths[name] || len(files) != 2 {
		t.Fatalf("unexpected unborn workspace diff: %+v", files)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
