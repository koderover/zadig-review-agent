package filter

import (
	"testing"

	"github.com/koderover/zadig-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-review-agent/internal/rules"
)

func TestCleanRelativeRejectsTraversal(t *testing.T) {
	for _, input := range []string{"../x", "/tmp/x", ""} {
		if _, ok := CleanRelative(input); ok {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
	if got, ok := CleanRelative("a/../b.go"); !ok || got != "b.go" {
		t.Fatalf("unexpected clean result %q %v", got, ok)
	}
}

func TestApplyUserExcludeWins(t *testing.T) {
	result := Apply([]gitdiff.FileDiff{{Path: "src/main.go"}}, Options{
		RuleFile: rules.RuleFile{Include: []string{"src/**/*.go"}, Exclude: []string{"src/main.go"}},
	})
	if len(result.Kept) != 0 || len(result.Excluded) != 1 || result.Excluded[0].Reason != ReasonUserExclude {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestApplyIncludeBypassesUnsupportedAndDefault(t *testing.T) {
	result := Apply([]gitdiff.FileDiff{{Path: "src/foo_test.go"}, {Path: "src/schema.custom"}}, Options{
		RuleFile: rules.RuleFile{Include: []string{"src/*_test.go", "src/*.custom"}},
	})
	if len(result.Kept) != 2 {
		t.Fatalf("expected include bypass to keep files: %+v", result)
	}
}

func TestApplyUnsupportedAndDefaultPath(t *testing.T) {
	result := Apply([]gitdiff.FileDiff{{Path: "src/foo.bin"}, {Path: "src/foo_test.go"}}, Options{})
	if len(result.Excluded) != 2 {
		t.Fatalf("expected excluded files: %+v", result)
	}
	if result.Excluded[0].Reason != ReasonUnsupportedExt || result.Excluded[1].Reason != ReasonDefaultPath {
		t.Fatalf("unexpected reasons: %+v", result.Excluded)
	}
}

func TestApplyKeepsSupportedBasenames(t *testing.T) {
	result := Apply([]gitdiff.FileDiff{{Path: "Dockerfile"}, {Path: "Makefile"}}, Options{})
	if len(result.Kept) != 2 || len(result.Excluded) != 0 {
		t.Fatalf("expected Dockerfile and Makefile to be kept: %+v", result)
	}
}

func TestApplyKeepsExtendedReviewFileTypes(t *testing.T) {
	paths := []string{"src/App.vue", "infra/main.tf", "scripts/check.ps1", "lib/types.pyi", "config/app.toml", "src/main.swift"}
	files := make([]gitdiff.FileDiff, 0, len(paths))
	for _, path := range paths {
		files = append(files, gitdiff.FileDiff{Path: path})
	}
	result := Apply(files, Options{})
	if len(result.Kept) != len(paths) || len(result.Excluded) != 0 {
		t.Fatalf("expected extended source types to be kept: %+v", result)
	}
}
