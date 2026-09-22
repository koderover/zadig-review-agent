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

func TestClientDiffNeverLoadsSensitivePaths(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	for _, name := range []string{".env", "server.pem", "private.key", "identity.p12", "release.keystore", "id_rsa", "nested/.env.local", "nested/.env/private.go", "nested/APP.KEY", ".npmrc", ".aws/credentials", ".kube/config", ".docker/config.json", "application_default_credentials.json", ".zadig-review-agent/config.yaml", "terraform.tfstate"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, path, "SENSITIVE_MARKER\n")
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "base")
	base := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	write(t, filepath.Join(dir, "main.go"), "package main\nvar OK = true\n")
	write(t, filepath.Join(dir, "private.key"), "CHANGED_SENSITIVE_MARKER\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "change")
	head := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	write(t, filepath.Join(dir, "main.go"), "package main\nvar OK = false\n")
	write(t, filepath.Join(dir, ".env"), "WORKSPACE_SENSITIVE_MARKER\n")
	write(t, filepath.Join(dir, "untracked.pem"), "UNTRACKED_SENSITIVE_MARKER\n")

	client := Client{Dir: dir}
	initialFiles, err := client.Diff(context.Background(), Request{Mode: ModeCommit, Commit: base})
	if err != nil || len(initialFiles) != 1 || initialFiles[0].Path != "main.go" {
		t.Fatalf("initial commit exposed sensitive files: files=%+v err=%v", initialFiles, err)
	}
	for _, request := range []Request{
		{Mode: ModeCommit, Commit: head},
		{Mode: ModeRange, From: base, To: head},
		{Mode: ModeWorkspace},
	} {
		files, err := client.Diff(context.Background(), request)
		if err != nil {
			t.Fatalf("diff %s: %v", request.Mode, err)
		}
		if len(files) != 1 || files[0].Path != "main.go" {
			t.Fatalf("diff %s exposed sensitive files: %+v", request.Mode, files)
		}
	}
}

func TestClientDiffExcludesRenamedSensitiveFile(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, ".env"), "RENAMED_SENSITIVE_MARKER\n")
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "base")
	base := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	if err := os.Rename(filepath.Join(dir, ".env"), filepath.Join(dir, "public.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "main.go"), "package main\nvar OK = true\n")
	client := Client{Dir: dir}
	check := func(request Request) {
		t.Helper()
		files, err := client.Diff(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].Path != "main.go" {
			t.Fatalf("%s exposed renamed secret: %+v", request.Mode, files)
		}
	}
	check(Request{Mode: ModeWorkspace})
	gitIn(t, dir, "add", "-A")
	check(Request{Mode: ModeWorkspace})
	gitIn(t, dir, "commit", "-m", "rename")
	head := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	check(Request{Mode: ModeCommit, Commit: head})
	check(Request{Mode: ModeRange, From: base, To: head})
}

func TestClientDiffExcludesAdditionsAfterSensitiveDeletion(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, ".env"), "OLD_SENSITIVE_MARKER\n")
	gitIn(t, dir, "add", ".env")
	gitIn(t, dir, "commit", "-m", "base")
	base := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "public.txt"), "NEW_SENSITIVE_MARKER\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-m", "replace")
	head := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	client := Client{Dir: dir}
	for _, request := range []Request{{Mode: ModeCommit, Commit: head}, {Mode: ModeRange, From: base, To: head}} {
		files, err := client.Diff(context.Background(), request)
		if err != nil || len(files) != 0 {
			t.Fatalf("%s exposed possible renamed secret: files=%+v err=%v", request.Mode, files, err)
		}
	}
}

func TestWorkspaceSkipsUntrackedAfterStagedSensitiveDeletion(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, ".env"), "SENSITIVE_MARKER\n")
	gitIn(t, dir, "add", ".env")
	gitIn(t, dir, "commit", "-m", "base")
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-u")
	write(t, filepath.Join(dir, "renamed.txt"), "SENSITIVE_MARKER\n")
	files, err := (Client{Dir: dir}).Diff(context.Background(), Request{Mode: ModeWorkspace})
	if err != nil || len(files) != 0 {
		t.Fatalf("staged deletion exposed untracked rename: files=%+v err=%v", files, err)
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
