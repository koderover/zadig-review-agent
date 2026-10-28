package gitdiff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNumstat(t *testing.T) {
	for _, test := range []struct {
		out     string
		want    Stats
		invalid bool
	}{
		{"", Stats{}, false},
		{"2\t1\tmain.go\n3\t0\t\"tab\\tname.go\"\n-\t-\timage.png\n0\t0\told => new\n", Stats{5, 1}, false},
		{"1\tname\n", Stats{}, true},
		{"-1\t0\tname\n", Stats{}, true},
		{"-\t1\tname\n", Stats{}, true},
	} {
		got, err := parseNumstat(test.out)
		if (err != nil) != test.invalid || got != test.want {
			t.Fatalf("parseNumstat(%q) = %+v, %v; want %+v invalid=%t", test.out, got, err, test.want, test.invalid)
		}
	}
}

func TestDiffStatsAllFilesAndModes(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test")
	gitIn(t, dir, "config", "commit.gpgsign", "false")
	initial := map[string]string{
		"main.go": "old\n", "main_test.go": "test\n", ".env": "SECRET\n",
		"deleted.txt": "remove\nme\n", "old.txt": "rename only\n",
		"image.bin": "\x00old", "tab\t旧\nname.md": "before\n",
	}
	for path, content := range initial {
		write(t, filepath.Join(dir, path), content)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "base")
	base := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	client := Client{Dir: dir}
	check := func(req Request, want Stats) {
		t.Helper()
		got, err := client.DiffStats(context.Background(), req)
		if err != nil || got != want {
			t.Fatalf("DiffStats(%+v) = %+v, %v; want %+v", req, got, err, want)
		}
	}
	check(Request{Mode: ModeCommit, Commit: base}, Stats{7, 0})
	check(Request{Mode: ModeWorkspace}, Stats{})
	gitIn(t, dir, "config", "diff.external", "false")
	gitIn(t, dir, "config", "diff.stats-driver.textconv", "false")
	write(t, filepath.Join(dir, ".git", "info", "attributes"), "*.go diff=stats-driver\n.env* diff=stats-driver\n")
	for path, content := range map[string]string{
		"main.go": "new\nnext\n", "main_test.go": "test\nextra\n", ".env": "SECRET\nextra\n",
		"image.bin": "\x00new", "tab\t旧\nname.md": "after\n", "new.txt": "added\n",
	} {
		write(t, filepath.Join(dir, path), content)
	}
	if err := os.Remove(filepath.Join(dir, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "old.txt"), filepath.Join(dir, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "change")
	head := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	check(Request{Mode: ModeCommit, Commit: head}, Stats{6, 4})
	check(Request{Mode: ModeRange, From: base, To: head}, Stats{6, 4})
	// Diverging changes on the base branch must not enter the PR statistics.
	gitIn(t, dir, "checkout", "--detach", base)
	write(t, filepath.Join(dir, "base-only.txt"), "base branch\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-m", "base branch change")
	other := strings.TrimSpace(gitOutIn(t, dir, "rev-parse", "HEAD"))
	check(Request{Mode: ModeRange, From: other, To: head}, Stats{6, 4})
	gitIn(t, dir, "checkout", "--detach", head)
	write(t, filepath.Join(dir, "main.go"), "new\nnext\nstaged\n")
	gitIn(t, dir, "add", "main.go")
	write(t, filepath.Join(dir, "main.go"), "new\nnext\nstaged\nunstaged\n")
	write(t, filepath.Join(dir, ".env.local"), "UNTRACKED_SECRET\n")
	write(t, filepath.Join(dir, "untracked\t旧\nname.md"), "one\ntwo")
	write(t, filepath.Join(dir, "untracked.bin"), "\x00binary")
	write(t, filepath.Join(dir, "empty.txt"), "")
	check(Request{Mode: ModeWorkspace}, Stats{5, 0})
	check(Request{}, Stats{5, 0})
	target := filepath.Join(t.TempDir(), "outside.go")
	write(t, target, "outside\nsecret\ncontents\n")
	if err := os.Symlink(target, filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	check(Request{}, Stats{6, 0})
}

func TestDiffStatsUnbornAndErrors(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init")
	write(t, filepath.Join(dir, "staged.go"), "one\n")
	gitIn(t, dir, "add", ".")
	write(t, filepath.Join(dir, "untracked.go"), "two\n")
	client := Client{Dir: dir}
	stats, err := client.DiffStats(context.Background(), Request{Mode: ModeWorkspace})
	if err != nil || stats != (Stats{2, 0}) {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	for _, req := range []Request{
		{Mode: Mode("bad")}, {Mode: ModeCommit}, {Mode: ModeRange},
		{Mode: ModeCommit, Commit: "missing"}, {Mode: ModeRange, From: "missing", To: "HEAD"},
	} {
		if _, err := client.DiffStats(context.Background(), req); err == nil {
			t.Fatalf("expected error for %+v", req)
		}
	}
}
