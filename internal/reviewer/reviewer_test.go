package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/config"
	"github.com/koderover/zadig-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-review-agent/internal/protocol"
	"github.com/koderover/zadig-review-agent/internal/rules"
)

func TestSystemPromptUsesConfiguredLanguage(t *testing.T) {
	prompt := systemPrompt("en-US")
	if !strings.Contains(prompt, "in en-US") || !strings.Contains(prompt, "JSON keys and enum values") {
		t.Fatalf("language instruction missing from prompt: %s", prompt)
	}
}

func TestRunnerReportsSensitiveRenamePrecautionAsIncomplete(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init")
	gitIn(t, root, "config", "user.email", "test@example.com")
	gitIn(t, root, "config", "user.name", "Test")
	gitIn(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", ".env")
	gitIn(t, root, "commit", "-m", "base")
	if err := os.Rename(filepath.Join(root, ".env"), filepath.Join(root, "public.go")); err != nil {
		t.Fatal(err)
	}
	report, err := (Runner{
		Root: root, Config: config.Default(), Git: gitdiff.Client{Dir: root},
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace},
	}).Run(context.Background())
	if err != nil || !report.Incomplete || report.ExitCode != agent.ExitIncomplete || len(report.ExcludedFiles) != 1 || report.ExcludedFiles[0].Path != "public.go" || report.ExcludedFiles[0].Reason != "sensitive_rename_precaution" || len(report.Warnings) != 1 {
		t.Fatalf("precaution was silent or marked complete: report=%+v err=%v", report, err)
	}
}

func TestRunnerFiltersInvalidFindingsAndBlocks(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{
		NewStart:     10,
		NewLines:     1,
		ChangedLines: map[int]bool{10: true},
		Lines:        []gitdiff.Line{{Kind: '+', NewLine: 10, Text: "panic(\"x\")"}},
	}}}
	cfg := config.Default()
	cfg.Output.Language = "English"
	r := Runner{
		Root:        "/repo",
		Config:      cfg,
		Git:         fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM: &sequenceLLM{responses: []protocol.Response{
			withUsage(commentAndDoneResponse(agent.Finding{Severity: "high", Category: "correctness", RuleID: "correctness", File: "main.go", StartLine: 10, EndLine: 10, ExistingCode: `panic("x")`, Title: "panic", Problem: "panic added", Evidence: "panic", Suggestion: "return error", Confidence: 0.95}), agent.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, CacheReadTokens: 3}),
			{Text: `[]`, Usage: agent.TokenUsage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15, CacheWriteTokens: 4}},
		}},
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceSystem, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: "test rule"}}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ExitCode != agent.ExitBlocked || len(report.Findings) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.Usage.PromptTokens != 22 || report.Usage.CompletionTokens != 5 || report.Usage.TotalTokens != 27 || report.Usage.LLMRequests != 2 || report.Usage.CacheReadTokens != 3 || report.Usage.CacheWriteTokens != 4 {
		t.Fatalf("unexpected usage: %+v", report.Usage)
	}
}

func TestDecideExitIncompleteWins(t *testing.T) {
	report := agent.Report{Incomplete: true, Findings: []agent.Finding{{Severity: "high"}}}
	if got := DecideExit(report, []string{"high"}); got != agent.ExitIncomplete {
		t.Fatalf("got %d", got)
	}
}

func TestRunnerDoesNotCallLLMWhenAllFilesExcluded(t *testing.T) {
	r := Runner{
		Root:         "/repo",
		Config:       config.Default(),
		Git:          fakeGit{files: []gitdiff.FileDiff{{Path: "archive.bin"}}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          failLLM{t: t},
		RuleResolver: rules.Resolver{},
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ExitCode != agent.ExitOK || len(report.Findings) != 0 || len(report.ExcludedFiles) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

type failLLM struct {
	t *testing.T
}

func (f failLLM) Complete(context.Context, protocol.Request) (protocol.Response, error) {
	f.t.Fatal("LLM should not be called when there are no kept chunks")
	return protocol.Response{}, nil
}

type fakeGit struct {
	files    []gitdiff.FileDiff
	statsErr error
}

func (f fakeGit) Head(context.Context) (string, error) {
	return "HEADSHA", nil
}

func (f fakeGit) Diff(context.Context, gitdiff.Request) ([]gitdiff.FileDiff, error) {
	return f.files, nil
}

func (f fakeGit) DiffStats(context.Context, gitdiff.Request) (gitdiff.Stats, error) {
	if f.statsErr != nil {
		return gitdiff.Stats{}, f.statsErr
	}
	var stats gitdiff.Stats
	for _, file := range f.files {
		stats.Additions += file.Insertions
		stats.Deletions += file.Deletions
	}
	return stats, nil
}

func TestRunnerDiffStatsFailureIsIncomplete(t *testing.T) {
	report, err := (Runner{Config: config.Default(), Git: fakeGit{statsErr: errors.New("stats unavailable")}, LLM: failLLM{t}}).Run(context.Background())
	if err != nil || !report.Incomplete || report.ExitCode != agent.ExitIncomplete || len(report.Errors) != 1 || !strings.Contains(report.Errors[0], "stats unavailable") {
		t.Fatalf("stats failure was hidden: report=%+v err=%v", report, err)
	}
}

type sequenceLLM struct {
	responses []protocol.Response
	errors    []error
}

func (s *sequenceLLM) Complete(_ context.Context, _ protocol.Request) (protocol.Response, error) {
	var err error
	if len(s.errors) > 0 {
		err = s.errors[0]
		s.errors = s.errors[1:]
	}
	if len(s.responses) == 0 {
		return protocol.Response{Text: "no tool call"}, err
	}
	out := s.responses[0]
	s.responses = s.responses[1:]
	return out, err
}

func TestRunnerCountsFailedLLMRequest(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "SENSITIVE_PROMPT_CONTENT"}}}}}
	r := Runner{
		Root:         "/repo",
		Config:       config.Default(),
		Git:          fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          &sequenceLLM{errors: []error{errors.New("timeout")}},
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceSystem, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: "test rule"}}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.Usage.LLMRequests != 1 {
		t.Fatalf("expected failed request to be counted: %+v", report)
	}
	if len(report.Errors) != 1 {
		t.Fatalf("expected one detailed error: %+v", report.Errors)
	}
	detail := report.Errors[0]
	for _, want := range []string{
		"llm request failed", "stage=review", `file="main.go"`, `agent_source="internal/reviewer/agent_loop.go:`, "round=1", "request_attempts=1",
		"duration=", "configured_timeout=10m0s", "review_concurrency=4",
		"message_count=2", `tools="task_done,code_comment,file_read,code_search,changed_diff_read,file_find"`,
		"require_tool=false", "estimated_tokens=", "protocol=openai", `model="configured-model"`, "timeout",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detailed error missing %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, "SENSITIVE_PROMPT_CONTENT") {
		t.Fatalf("detailed error leaked prompt content: %s", detail)
	}
	if len(report.Process.ModelResponses) != 1 || report.Process.ModelResponses[0].Stage != "review" || report.Process.ModelResponses[0].Attempt != 1 || report.Process.ModelResponses[0].Status != "error" || report.Process.ModelResponses[0].Error != detail {
		t.Fatalf("main-loop model failure was not audited: %+v", report.Process.ModelResponses)
	}
}

func TestRunnerKeepsUsageWhenModelReturnsNoToolCall(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	r := Runner{
		Root:        "/repo",
		Config:      config.Default(),
		Git:         fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM: &sequenceLLM{responses: []protocol.Response{
			{Text: "not-json", Usage: agent.TokenUsage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11}},
			{Text: "still-not-json", Usage: agent.TokenUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}},
			{Text: "still no tool", Usage: agent.TokenUsage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15}},
		}},
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceSystem, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: "test rule"}}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.Usage.LLMRequests != 3 || report.Usage.TotalTokens != 39 {
		t.Fatalf("expected empty tool-call usage to be retained: %+v", report)
	}
}

func TestRunnerDoesNotRepairPlainTextModelResponse(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	r := Runner{
		Root:         "/repo",
		Config:       config.Default(),
		Git:          fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          fixedLLM{response: protocol.Response{Text: "I found no concrete issues."}},
		RuleResolver: testRuleResolver(),
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.ExitCode != agent.ExitIncomplete || report.Usage.LLMRequests != 3 {
		t.Fatalf("plain text must reach the no-tool limit: %+v", report)
	}
}

func TestRunnerRequiresToolAfterPlainTextResponse(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	llm := &recordingLLM{responses: []protocol.Response{{Text: "review complete"}, doneResponse()}}
	r := Runner{
		Root:         "/repo",
		Config:       config.Default(),
		Git:          fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          llm,
		RuleResolver: testRuleResolver(),
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(llm.requests) != 2 || llm.requests[0].RequireTool || !llm.requests[1].RequireTool {
		t.Fatalf("second request must require a native tool call: report=%+v requests=%+v", report, llm.requests)
	}
}

func TestRunnerAcceptsModelEndTurnAfterToolRetry(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":10}`),
		{Text: "No concrete issue found."},
		{Text: "Review complete."},
	}}
	r := Runner{Root: root, Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(report.Warnings) != 0 || report.Usage.LLMRequests != 3 || len(llm.requests) != 3 || !llm.requests[2].RequireTool {
		t.Fatalf("natural end turn after a required-tool retry must complete review: report=%+v requests=%+v", report, llm.requests)
	}
}

func TestRunnerAcceptsSecondEmptyEndTurnAfterToolActivity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":10}`),
		{},
		{},
	}}
	r := Runner{Root: root, Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(report.Warnings) != 0 || report.Usage.LLMRequests != 3 {
		t.Fatalf("second empty end turn after tool activity must complete review: %+v", report)
	}
}

func TestRunnerAcceptsEmptyFinalizationAsImplicitDone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	cfg := config.Default()
	cfg.Review.MaxContextToolCalls = 1
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":10}`),
		{},
	}}
	r := Runner{Root: root, Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(report.Warnings) != 0 || report.Usage.LLMRequests != 2 || len(llm.requests) != 2 || !llm.requests[1].RequireTool {
		t.Fatalf("empty finalization response must complete without another retry: report=%+v requests=%+v", report, llm.requests)
	}
}

func TestValidateFindingsNormalizesModelEnumVariants(t *testing.T) {
	file := reviewTestFile()
	findings, err := validateFindings([]agent.Finding{{
		Severity: "Medium", Category: "Error Handling", File: "main.go",
		StartLine: 10, EndLine: 10, Title: "lost context", Problem: "raw error returned",
		Confidence: 0.9,
	}}, file, []rules.ResolvedRule{{Source: "system", Pattern: "**/*.go"}}, 0.75)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != "medium" || findings[0].Category != "reliability" {
		t.Fatalf("model enum variants were not normalized: %+v", findings)
	}
}

func TestValidateFindingCategoriesAndNames(t *testing.T) {
	file := reviewTestFile()
	for _, tc := range []struct{ input, id, name string }{
		{"style", "style", "代码规范"},
		{"reliability", "reliability", "正确性与可靠性"},
		{"maintainability", "maintainability", "可维护性"},
		{"performance", "performance", "性能效率"},
		{"security", "security", "安全风险"},
		{"tests", "tests", "测试质量"},
		{"build", "build", "构建与交付"},
		{"correctness", "reliability", "正确性与可靠性"},
		{"Concurrency", "reliability", "正确性与可靠性"},
		{"compatibility", "reliability", "正确性与可靠性"},
		{"bug", "reliability", "正确性与可靠性"},
		{"Error_Handling", "reliability", "正确性与可靠性"},
		{"resource-management", "reliability", "正确性与可靠性"},
		{"test coverage", "tests", "测试质量"},
		{"testing", "tests", "测试质量"},
		{"formatting", "style", "代码规范"},
		{"naming", "style", "代码规范"},
		{"unknown", "", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			candidate := agent.Finding{Severity: "high", Category: tc.input, CategoryName: "model supplied name", File: file.Path, StartLine: 10, EndLine: 10, Title: "issue", Problem: "problem", Confidence: 0.9}
			got, err := validateFindings([]agent.Finding{candidate}, file, nil, 0.75)
			if err != nil {
				t.Fatal(err)
			}
			if tc.id == "" {
				if len(got) != 0 {
					t.Fatalf("unknown category accepted: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Category != tc.id || got[0].CategoryName != tc.name {
				t.Fatalf("unexpected category: %+v", got)
			}
			if DecideExit(agent.Report{Findings: got}, []string{"high"}) != agent.ExitBlocked {
				t.Fatal("English severity no longer triggers the quality gate")
			}
			var serialized agent.Finding
			data, err := json.Marshal(got[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &serialized); err != nil {
				t.Fatal(err)
			}
			if serialized.Category != tc.id || serialized.CategoryName != tc.name || serialized.Severity != "high" || strings.Contains(string(data), "severity_name") {
				t.Fatalf("JSON classification mismatch: %s", data)
			}
		})
	}
}

func TestValidateFindingsMapsRuleReferencesWithoutDroppingUnattributed(t *testing.T) {
	file := reviewTestFile()
	matched := []rules.ResolvedRule{
		{ID: "project:1", Name: "Go security", Source: rules.SourceProject},
		{ID: "project:2", Name: "Go style", Source: rules.SourceProject},
	}
	var candidates []agent.Finding
	for _, ref := range []string{"R1", "R2", "unknown", ""} {
		candidates = append(candidates, agent.Finding{
			Severity: "low", Category: "style", RuleID: ref, RuleName: "untrusted model value",
			File: file.Path, StartLine: 10, EndLine: 10, Title: "issue " + ref,
			Problem: "problem", Evidence: "evidence", Suggestion: "suggestion", Confidence: 0.9,
		})
	}
	got, err := validateFindings(candidates, file, matched, 0.75)
	if err != nil || len(got) != 4 {
		t.Fatalf("findings were dropped: %+v, err=%v", got, err)
	}
	if got[0].RuleID != "project:1" || got[0].RuleName != "Go security" || len(got[0].MatchedRules) != 1 || got[1].RuleID != "project:2" || got[1].RuleName != "Go style" || len(got[1].MatchedRules) != 1 {
		t.Fatalf("rule attribution mismatch: %+v", got)
	}
	for _, finding := range got[2:] {
		if finding.RuleID != "" || finding.RuleName != "" || len(finding.MatchedRules) != 0 {
			t.Fatalf("unattributed finding retained model metadata: %+v", finding)
		}
	}
}

func TestAggregateMergesRuleAttributionForSameIssue(t *testing.T) {
	file := reviewTestFile()
	matched := []rules.ResolvedRule{
		{ID: "project:1", Name: "First rule"},
		{ID: "project:2", Name: "Second rule"},
	}
	candidate := agent.Finding{Severity: "low", Category: "style", File: file.Path, StartLine: 10, EndLine: 10, Title: "same issue", Problem: "same problem", Confidence: 0.9}
	first, second, unattributed := candidate, candidate, candidate
	first.RuleID, second.RuleID, unattributed.RuleID = "R2", "R1", "invalid"
	first.Category, second.Category, unattributed.Category = "correctness", "concurrency", "compatibility"
	findings, err := validateFindings([]agent.Finding{first, second, unattributed, second}, file, matched, 0.75)
	merged := aggregate(findings)
	if err != nil || len(merged) != 1 || merged[0].Category != "reliability" || merged[0].CategoryName != "正确性与可靠性" || len(merged[0].MatchedRules) != 2 || merged[0].MatchedRules[0].ID != "project:1" || merged[0].MatchedRules[1].ID != "project:2" || merged[0].RuleID != "project:1" {
		t.Fatalf("same issue did not merge both rule attributions: %+v err=%v", merged, err)
	}
}

func TestAggregateKeepsHighestSeverityRegardlessOfOrder(t *testing.T) {
	base := agent.Finding{File: "main.go", StartLine: 10, EndLine: 10, Category: "correctness", Title: "same issue", Problem: "same problem"}
	base.Fingerprint = fingerprint(base)
	low, high := base, base
	low.Severity = "medium"
	low.Evidence = "weaker evidence"
	low.Suggestion = "weaker suggestion"
	low.Confidence = 0.8
	low.MatchedRules = []agent.FindingRule{{ID: "project:1", Name: "First rule"}}
	high.Severity = "high"
	high.Evidence = "stronger evidence"
	high.Suggestion = "stronger suggestion"
	high.Confidence = 0.95
	high.MatchedRules = []agent.FindingRule{{ID: "project:2", Name: "Second rule"}}
	for _, findings := range [][]agent.Finding{{low, high}, {high, low}} {
		merged := aggregate(findings)
		if len(merged) != 1 || merged[0].Severity != "high" || merged[0].Evidence != high.Evidence || merged[0].Suggestion != high.Suggestion || merged[0].Confidence != high.Confidence || len(merged[0].MatchedRules) != 2 || DecideExit(agent.Report{Findings: merged}, []string{"high"}) != agent.ExitBlocked {
			t.Fatalf("duplicate severity depends on order: %+v", merged)
		}
	}
}

func TestValidateFindingAcceptsMultipleRuleReferences(t *testing.T) {
	file := reviewTestFile()
	matched := []rules.ResolvedRule{{ID: "project:1", Name: "First rule"}, {ID: "project:2", Name: "Second rule"}}
	candidate := agent.Finding{Severity: "low", Category: "style", RuleIDs: []string{"R2", "invalid", "R1", "R1"}, File: file.Path, StartLine: 10, EndLine: 10, Title: "issue", Problem: "problem", Confidence: 0.9}
	got, err := validateFindings([]agent.Finding{candidate}, file, matched, 0.75)
	if err != nil || len(got) != 1 || len(got[0].MatchedRules) != 2 || got[0].MatchedRules[0].ID != "project:1" || got[0].MatchedRules[1].ID != "project:2" || len(got[0].RuleIDs) != 0 {
		t.Fatalf("multiple rule references not normalized: %+v err=%v", got, err)
	}
}

func TestRunnerReviewsTwoMatchingRulesTogether(t *testing.T) {
	file := reviewTestFile()
	cfg := config.Default()
	cfg.Output.Language = "English"
	findings := []agent.Finding{
		{Severity: "high", Category: "correctness", RuleID: "R1", File: file.Path, StartLine: 10, EndLine: 10, ExistingCode: `panic("x")`, Title: "same issue", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion", Confidence: 0.9},
		{Severity: "high", Category: "correctness", RuleID: "R2", File: file.Path, StartLine: 10, EndLine: 10, ExistingCode: `panic("x")`, Title: "same issue", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion", Confidence: 0.9},
	}
	arguments, _ := json.Marshal(map[string]any{"findings": findings})
	llm := &recordingLLM{responses: []protocol.Response{{ToolCalls: []protocol.ToolCall{
		{ID: "comment", Name: "code_comment", Arguments: string(arguments)},
		{ID: "done", Name: "task_done", Arguments: `{}`},
	}}, {Text: `[]`}}}
	r := Runner{
		Root: t.TempDir(), Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm,
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceProject, File: rules.RuleFile{Rules: []rules.RuleEntry{
			{Name: "Go security", Path: "**/*.go", Rule: "security checks"},
			{Name: "Go style", Path: "**/*.go", Rule: "formatting checks"},
		}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil || report.Incomplete || len(report.Findings) != 1 || len(report.ResolvedRules) != 2 {
		t.Fatalf("unexpected report: %+v err=%v", report, err)
	}
	if len(report.Findings[0].MatchedRules) != 2 || report.Findings[0].MatchedRules[0].Name != "Go security" || report.Findings[0].MatchedRules[1].Name != "Go style" || len(llm.requests) != 2 || !requestContains(llm.requests[0], `ref="R1"`) || !requestContains(llm.requests[0], `ref="R2"`) || !requestContains(llm.requests[1], `"id":"c-1"`) || !requestContains(llm.requests[1], `"rule_id":"project:1"`) || !requestContains(llm.requests[1], `"rule_id":"project:2"`) {
		t.Fatalf("rules were not reviewed together: report=%+v requests=%+v", report, llm.requests)
	}
}

func TestRunnerLocalizesMergedFindingOnce(t *testing.T) {
	file := reviewTestFile()
	findings := []agent.Finding{
		{Severity: "high", Category: "correctness", RuleID: "R1", File: file.Path, StartLine: 10, EndLine: 10, ExistingCode: `panic("x")`, Title: "same issue", Problem: "same problem", Evidence: "same evidence", Suggestion: "same suggestion", Confidence: 0.9},
		{Severity: "high", Category: "correctness", RuleID: "R2", File: file.Path, StartLine: 10, EndLine: 10, ExistingCode: `panic("x")`, Title: "same issue", Problem: "same problem", Evidence: "same evidence", Suggestion: "same suggestion", Confidence: 0.9},
	}
	arguments, _ := json.Marshal(map[string]any{"findings": findings})
	llm := &recordingLLM{responses: []protocol.Response{
		{ToolCalls: []protocol.ToolCall{{ID: "comment", Name: "code_comment", Arguments: string(arguments)}, {ID: "done", Name: "task_done", Arguments: `{}`}}},
		{Text: `[]`},
		{Text: `[{"id":"c-0","title":"相同问题","problem":"同一个问题。","evidence":"同一证据。","suggestion":"同一建议。"}]`},
	}}
	r := Runner{
		Root: t.TempDir(), Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm,
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceProject, File: rules.RuleFile{Rules: []rules.RuleEntry{
			{Name: "First rule", Path: "**/*.go", Rule: "first checks"},
			{Name: "Second rule", Path: "**/*.go", Rule: "second checks"},
		}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil || report.Incomplete || len(report.Findings) != 1 || len(report.Findings[0].MatchedRules) != 2 || len(llm.requests) != 3 {
		t.Fatalf("localization separated duplicate findings: report=%+v err=%v requests=%d", report, err, len(llm.requests))
	}
	if report.Findings[0].Category != "reliability" || report.Findings[0].CategoryName != "正确性与可靠性" || report.Findings[0].Title != "相同问题" || report.Findings[0].MatchedRules[0].ID != "project:1" || report.Findings[0].MatchedRules[1].ID != "project:2" || !requestContains(llm.requests[1], `"id":"c-1"`) || requestContains(llm.requests[2], `"id":"c-1"`) {
		t.Fatalf("localized finding lost rule attribution: %+v", report.Findings[0])
	}
}

func TestRunnerFilterRetainsOnlySurvivingEvidenceAndRuleName(t *testing.T) {
	file := reviewTestFile()
	cfg := config.Default()
	cfg.Output.Language = "English"
	findings := []agent.Finding{
		{Severity: "high", Category: "correctness", RuleID: "R1", File: file.Path, StartLine: 10, EndLine: 10, Title: "same issue", Problem: "same problem", Evidence: "the changed line returns nil", Suggestion: "return an error", Confidence: 0.9},
		{Severity: "medium", Category: "correctness", RuleID: "R2", File: file.Path, StartLine: 10, EndLine: 10, Title: "same issue", Problem: "same problem", Evidence: "the changed line panics", Suggestion: "handle the error", Confidence: 0.85},
	}
	arguments, _ := json.Marshal(map[string]any{"findings": findings})
	llm := &recordingLLM{responses: []protocol.Response{
		{ToolCalls: []protocol.ToolCall{{ID: "comment", Name: "code_comment", Arguments: string(arguments)}, {ID: "done", Name: "task_done", Arguments: `{}`}}},
		{Text: `["c-0"]`},
	}}
	r := Runner{
		Root: t.TempDir(), Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm,
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceProject, File: rules.RuleFile{Rules: []rules.RuleEntry{
			{Name: "First rule", Path: "**/*.go", Rule: "first checks"},
			{Name: "Second rule", Path: "**/*.go", Rule: "second checks"},
		}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil || report.Incomplete || len(report.Findings) != 1 || len(llm.requests) != 2 {
		t.Fatalf("filter dropped the surviving issue: report=%+v err=%v requests=%+v", report, err, llm.requests)
	}
	finding := report.Findings[0]
	if finding.Category != "reliability" || finding.CategoryName != "正确性与可靠性" || finding.Severity != "medium" || finding.Evidence != "the changed line panics" || finding.Suggestion != "handle the error" || finding.Confidence != 0.85 || len(finding.MatchedRules) != 1 || finding.MatchedRules[0].ID != "project:2" || finding.RuleID != "project:2" || finding.RuleName != "Second rule" || !requestContains(llm.requests[1], `"id":"c-1"`) || !requestContains(llm.requests[1], "the changed line returns nil") || !requestContains(llm.requests[1], "the changed line panics") {
		t.Fatalf("filter lost surviving evidence or rule attribution: finding=%+v request=%+v", finding, llm.requests[1])
	}
}

func TestRunnerFinalizesAfterContextToolBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	cfg := config.Default()
	cfg.Review.MaxContextToolCalls = 2
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read-1", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":10}`),
		toolResponse("read-2", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":9}`),
		doneResponse(),
	}}
	r := Runner{Root: root, Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(report.Process.ToolCalls) != 2 || len(llm.requests) != 3 || len(llm.requests[2].Tools) != 2 || !llm.requests[2].RequireTool {
		t.Fatalf("context budget must switch to finalization tools: report=%+v requests=%+v", report, llm.requests)
	}
	for _, tool := range llm.requests[2].Tools {
		if tool.Name != "code_comment" && tool.Name != "task_done" {
			t.Fatalf("unexpected finalization tool: %+v", tool)
		}
	}
}

func TestMainLoopRepairsInvalidFinalizationArguments(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := reviewTestFile()
	cfg := config.Default()
	cfg.Review.MaxContextToolCalls = 1
	cfg.Review.MaxToolRounds = 2
	finding := agent.Finding{Severity: "high", Category: "correctness", Title: "issue", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion", Confidence: 0.9}
	arguments, _ := json.Marshal(map[string]any{"findings": []agent.Finding{finding}})
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":1}`),
		toolResponse("invalid-comment", "code_comment", `{"findings":`),
		toolResponse("repaired-comment", "code_comment", string(arguments)),
	}}
	r := Runner{Root: root, Config: cfg, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	got, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(got) != 1 || len(llm.requests) != 3 {
		t.Fatalf("valid repair must recover finalization beyond the normal round limit: findings=%+v warnings=%v requests=%d", got, warnings, len(llm.requests))
	}
	if !llm.requests[2].RequireTool || len(llm.requests[2].Tools) != 2 || !requestContains(llm.requests[2], "only repair attempt") {
		t.Fatalf("repair request must expose only terminal tools and require one: %+v", llm.requests[2])
	}
	calls := r.process.snapshot().ToolCalls
	if len(calls) != 3 || calls[1].Status != "error" || calls[1].Summary != "invalid tool arguments" || calls[2].Status != "success" {
		t.Fatalf("finalization repair was not audited correctly: %+v", calls)
	}
}

func TestMainLoopReportsFinalizationFailureAfterRepairFails(t *testing.T) {
	for _, test := range []struct {
		name     string
		response protocol.Response
	}{
		{name: "invalid arguments", response: toolResponse("invalid-again", "code_comment", `{"findings":`)},
		{name: "no tool call", response: protocol.Response{Text: "unable to repair"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			file := reviewTestFile()
			cfg := config.Default()
			cfg.Review.MaxContextToolCalls = 1
			cfg.Review.MaxToolRounds = 2
			llm := &recordingLLM{responses: []protocol.Response{
				toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":1}`),
				toolResponse("invalid-comment", "code_comment", `{"findings":`),
				test.response,
			}}
			r := Runner{Root: root, Config: cfg, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
			usage := agent.TokenUsage{}
			got, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 || len(warnings) != 1 || warnings[0] != "finalization_failed: "+file.Path || len(llm.requests) != 3 {
				t.Fatalf("failed repair must report one finalization warning: findings=%+v warnings=%v requests=%d", got, warnings, len(llm.requests))
			}
		})
	}
}

func TestMainLoopConvergesBeforeHardContextBudget(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"dep1.go", "dep2.go", "dep3.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package dep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := reviewTestFile()
	cfg := config.Default()
	cfg.Review.MaxContextToolCalls = 4
	cfg.Review.ContextConvergenceRatio = 0.5
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read-1", "file_read", `{"file_path":"dep1.go","start_line":1,"end_line":1}`),
		toolResponse("read-2", "file_read", `{"file_path":"dep2.go","start_line":1,"end_line":1}`),
		toolResponse("read-3", "file_read", `{"file_path":"dep3.go","start_line":1,"end_line":1}`),
		doneResponse(),
	}}
	r := Runner{Root: root, Config: cfg, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	_, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(llm.requests) != 4 || len(r.process.snapshot().ToolCalls) != 3 {
		t.Fatalf("review did not converge before the hard budget: warnings=%v requests=%d calls=%+v", warnings, len(llm.requests), r.process.snapshot().ToolCalls)
	}
	if !llm.requests[2].RequireTool || !requestContains(llm.requests[2], "Converge now") || len(llm.requests[2].Tools) != 6 {
		t.Fatalf("soft limit must allow exactly one final context round: %+v", llm.requests[2])
	}
	if !llm.requests[3].RequireTool || len(llm.requests[3].Tools) != 2 || !requestContains(llm.requests[3], "final context round is complete") {
		t.Fatalf("final context round must transition directly to finalization: %+v", llm.requests[3])
	}
}

func TestContextConvergenceThresholdUsesConfiguredRatio(t *testing.T) {
	for _, test := range []struct {
		hard  int
		ratio float64
		want  int
	}{
		{hard: 10, ratio: 0.7, want: 7},
		{hard: 6, ratio: 0.7, want: 5},
		{hard: 4, ratio: 0.5, want: 2},
		{hard: 1, ratio: 0.7, want: 1},
		{hard: 10, ratio: 0, want: 5},
	} {
		if got := contextConvergenceThreshold(test.hard, test.ratio); got != test.want {
			t.Fatalf("contextConvergenceThreshold(%d, %v) = %d, want %d", test.hard, test.ratio, got, test.want)
		}
	}
}

func TestRunnerStopsDispatchBeforeProjectedTokenBudgetOverrun(t *testing.T) {
	first := reviewTestFile()
	second := reviewTestFile()
	second.Path = "second.go"
	cfg := config.Default()
	cfg.Review.Concurrency = 1
	resolver := testRuleResolver()
	cfg.Review.MaxTokensBudget = estimateReviewChunkTokens(first, resolver.ResolveAll(first.Path)...)
	llm := &recordingLLM{responses: []protocol.Response{
		withUsage(doneResponse(), agent.TokenUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100}),
		doneResponse(),
	}}
	r := Runner{Root: t.TempDir(), Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{first, second}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: resolver}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.ExitCode != agent.ExitIncomplete || len(llm.requests) != 1 || report.Usage.TotalTokens != 100 {
		t.Fatalf("token budget did not stop projected overrun: report=%+v requests=%d", report, len(llm.requests))
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "token_budget_reached: second.go") {
		t.Fatalf("token budget warning missing: %+v", report.Warnings)
	}
}

func TestMainLoopAcceptsBatchedCommentsAndEndsWithoutTaskDone(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := reviewTestFile()
	cfg := config.Default()
	cfg.Review.MaxContextToolCalls = 1
	findings := []agent.Finding{
		{Severity: "high", Category: "correctness", Title: "first", Problem: "first problem", Evidence: "first evidence", Suggestion: "first suggestion", Confidence: 0.9},
		{Severity: "medium", Category: "tests", Title: "second", Problem: "second problem", Evidence: "second evidence", Suggestion: "second suggestion", Confidence: 0.85},
	}
	arguments, _ := json.Marshal(map[string]any{"findings": findings})
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":1}`),
		toolResponse("comments", "code_comment", string(arguments)),
		doneResponse(),
	}}
	r := Runner{Root: root, Config: cfg, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	got, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(got) != 2 || len(llm.requests) != 2 {
		t.Fatalf("batched comments should complete the main loop in one finalization call: findings=%+v warnings=%v requests=%d", got, warnings, len(llm.requests))
	}
	for _, finding := range got {
		if finding.File != file.Path {
			t.Fatalf("batched finding was not scoped to the current file: %+v", finding)
		}
	}
	calls := r.process.snapshot().ToolCalls
	if len(calls) != 2 || calls[1].Tool != "code_comment" || len(calls[1].Arguments.Findings) != 2 {
		t.Fatalf("batched code_comment was not audited correctly: %+v", calls)
	}
}

func TestMainLoopRejectsForeignFileFindingsWithoutDiscardingCurrentFindings(t *testing.T) {
	file := reviewTestFile()
	findings := []agent.Finding{
		{Severity: "high", Category: "correctness", File: "./main.go", Title: "current", Problem: "current problem", Evidence: "current evidence", Suggestion: "current suggestion", Confidence: 0.9},
		{Severity: "medium", Category: "correctness", File: "other.go", Title: "foreign", Problem: "foreign problem", Evidence: "foreign evidence", Suggestion: "foreign suggestion", Confidence: 0.9},
	}
	arguments, _ := json.Marshal(map[string]any{"findings": findings})
	llm := &recordingLLM{responses: []protocol.Response{{ToolCalls: []protocol.ToolCall{
		{ID: "comments", Name: "code_comment", Arguments: string(arguments)},
		{ID: "done", Name: "task_done", Arguments: `{}`},
	}}}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	got, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(got) != 1 || got[0].File != file.Path || got[0].Title != "current" || len(llm.requests) != 1 {
		t.Fatalf("mixed batch must retain only current-file findings: findings=%+v warnings=%v requests=%d", got, warnings, len(llm.requests))
	}
	calls := r.process.snapshot().ToolCalls
	if len(calls) != 1 || calls[0].Status != "success" || len(calls[0].Arguments.Findings) != 2 || calls[0].Arguments.Findings[0].File != file.Path || calls[0].Arguments.Findings[1].File != "other.go" || !strings.Contains(calls[0].Output, "Rejected 1 finding(s) targeting other files") {
		t.Fatalf("foreign finding rejection was not audited: %+v", calls)
	}
}

func TestMainLoopRejectsCodeCommentWhenEveryFindingTargetsAnotherFile(t *testing.T) {
	file := reviewTestFile()
	finding := agent.Finding{Severity: "high", Category: "correctness", File: "other.go", Title: "foreign", Problem: "foreign problem", Evidence: "foreign evidence", Suggestion: "foreign suggestion", Confidence: 0.9}
	arguments, _ := json.Marshal(map[string]any{"finding": finding})
	llm := &recordingLLM{responses: []protocol.Response{{ToolCalls: []protocol.ToolCall{
		{ID: "comment", Name: "code_comment", Arguments: string(arguments)},
		{ID: "done", Name: "task_done", Arguments: `{}`},
	}}}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	got, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(got) != 0 || len(llm.requests) != 1 {
		t.Fatalf("foreign-only code_comment must be rejected without affecting task completion: findings=%+v warnings=%v requests=%d", got, warnings, len(llm.requests))
	}
	calls := r.process.snapshot().ToolCalls
	if len(calls) != 1 || calls[0].Status != "error" || calls[0].Summary != "foreign-file findings rejected" || !strings.Contains(calls[0].Output, `current file "main.go"`) {
		t.Fatalf("foreign-only rejection was not audited: %+v", calls)
	}
}

func TestMainLoopRequiresExplicitDoneAfterCommentsBeforeFinalization(t *testing.T) {
	file := reviewTestFile()
	finding := agent.Finding{Severity: "high", Category: "correctness", Title: "issue", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion", Confidence: 0.9}
	arguments, _ := json.Marshal(map[string]any{"findings": []agent.Finding{finding}})
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("comments", "code_comment", string(arguments)),
		doneResponse(),
	}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	got, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || len(got) != 1 || len(llm.requests) != 2 {
		t.Fatalf("ordinary code_comment must continue until explicit task_done: findings=%+v warnings=%v requests=%d", got, warnings, len(llm.requests))
	}
	if !llm.requests[1].RequireTool || !requestContains(llm.requests[1], "call task_done now") || !requestContains(llm.requests[1], "Do not begin a new investigation") {
		t.Fatalf("comment result did not steer the model to task_done: %+v", llm.requests[1])
	}
}

func TestRunnerCachesIdenticalContextToolCallsWithoutSpendingBudget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	cfg := config.Default()
	cfg.Review.MaxContextToolCalls = 1
	duplicateCalls := protocol.Response{ToolCalls: []protocol.ToolCall{
		{ID: "read-1", Name: "file_read", Arguments: `{"file_path":"dep.go","start_line":1,"end_line":10}`},
		{ID: "read-2", Name: "file_read", Arguments: `{"file_path":"dep.go","start_line":1,"end_line":10}`},
	}}
	llm := &recordingLLM{responses: []protocol.Response{duplicateCalls, doneResponse()}}
	r := Runner{Root: root, Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || len(report.Process.ToolCalls) != 2 || report.Process.ToolCalls[0].Cached || !report.Process.ToolCalls[1].Cached {
		t.Fatalf("identical call was not cached: %+v", report.Process.ToolCalls)
	}
	if report.Process.ToolCalls[1].Summary != "cached: 1 lines read" || report.Process.ToolCalls[1].Output != report.Process.ToolCalls[0].Output {
		t.Fatalf("cached call does not return the original content: %+v", report.Process.ToolCalls[1])
	}
	if len(llm.requests) != 2 || len(llm.requests[1].Tools) != 2 || !llm.requests[1].RequireTool {
		t.Fatalf("cached call must not delay finalization after the real budget is spent: %+v", llm.requests)
	}
}

func TestMainLoopAllowsInvalidRegexCorrectionAtToolLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package dep\nfunc StreamServiceLogs() {}\nfunc CollectServiceLogs() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "init")
	gitIn(t, root, "add", "dep.go")
	file := reviewTestFile()
	cfg := config.Default()
	cfg.Review.MaxToolRounds = 2
	cfg.Review.MaxContextToolCalls = 1
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("bad", "code_search", `{"search_text":"StreamServiceLogs(|CollectServiceLogs(","use_perl_regexp":true}`),
		toolResponse("fixed", "code_search", `{"search_text":"StreamServiceLogs\\(|CollectServiceLogs\\(","use_perl_regexp":true}`),
		doneResponse(),
	}}
	r := Runner{Root: root, Config: cfg, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	var usage agent.TokenUsage
	_, warnings, err := r.runMainLoop(context.Background(), file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), 1, &usage)
	if err != nil || len(warnings) != 0 || len(llm.requests) != 3 {
		t.Fatalf("regex correction did not finish review: warnings=%v err=%v requests=%d", warnings, err, len(llm.requests))
	}
	if !requestContains(llm.requests[1], "Correct the pattern and call code_search again") || len(llm.requests[1].Tools) <= 2 {
		t.Fatalf("model was not given a correction round: %+v", llm.requests[1])
	}
	calls := r.process.snapshot().ToolCalls
	if len(calls) != 2 || calls[0].Status != "error" || calls[1].Status != "success" || !strings.Contains(calls[1].Output, "StreamServiceLogs") {
		t.Fatalf("unexpected search calls: %+v", calls)
	}
}

func TestMainLoopDoesNotReinjectOrChargeRepeatedChangedDiffPaths(t *testing.T) {
	current := reviewTestFile()
	first := gitdiff.FileDiff{Path: "first.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const first = true"}}, ChangedLines: map[int]bool{1: true},
	}}}
	second := gitdiff.FileDiff{Path: "second.go", Hunks: []gitdiff.Hunk{{
		NewStart: 1, NewLines: 1, Lines: []gitdiff.Line{{Kind: '+', NewLine: 1, Text: "const second = true"}}, ChangedLines: map[int]bool{1: true},
	}}}
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("diff-1", "changed_diff_read", `{"file_paths":["first.go"]}`),
		toolResponse("diff-2", "changed_diff_read", `{"file_paths":["first.go"]}`),
		toolResponse("diff-3", "changed_diff_read", `{"file_paths":["second.go"]}`),
		doneResponse(),
	}}
	cfg := config.Default()
	r := Runner{Root: t.TempDir(), Config: cfg, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	_, warnings, err := r.runMainLoop(context.Background(), current, []gitdiff.FileDiff{current, first, second}, mainLoopTestValues(current), 1, &usage)
	if err != nil {
		t.Fatal(err)
	}
	calls := r.process.snapshot().ToolCalls
	if len(warnings) != 0 || len(calls) != 3 || calls[0].Cached || !calls[1].Cached || calls[2].Cached {
		t.Fatalf("changed diff path deduplication was not audited correctly: warnings=%v calls=%+v", warnings, calls)
	}
	if strings.Contains(calls[1].Output, "==== CHANGED DIFF:") || !strings.Contains(calls[1].Output, "already provided") {
		t.Fatalf("repeated changed diff content was reinjected: %+v", calls[1])
	}
	if len(llm.requests) != 4 || llm.requests[3].RequireTool || len(llm.requests[3].Tools) != 5 {
		t.Fatalf("repeated changed diff should not spend context budget and exhausted tool should be removed: %+v", llm.requests[3])
	}
}

func TestRunnerStopsAfterThreeEmptyToolRounds(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	cfg := config.Default()
	cfg.Review.MaxToolRounds = 30
	r := Runner{
		Root:         "/repo",
		Config:       cfg,
		Git:          fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          fixedLLM{response: protocol.Response{Text: "no tools"}},
		RuleResolver: testRuleResolver(),
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.ExitCode != agent.ExitIncomplete || report.Usage.LLMRequests != 3 || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "tool_loop_empty_limit_reached") {
		t.Fatalf("unexpected empty-round result: %+v", report)
	}
}

func TestToolProgressLabelShowsSearchAndScope(t *testing.T) {
	read := agent.ToolCall{Tool: "file_read", Arguments: agent.ToolArguments{FilePath: "pkg/service.go", StartLine: 20, EndLine: 80}}
	if got := toolProgressLabel(read); got != `file_read "pkg/service.go"` {
		t.Fatalf("unexpected file read label %q", got)
	}
	changedDiff := agent.ToolCall{Tool: "changed_diff_read", Arguments: agent.ToolArguments{FilePaths: []string{"a.go", "b.go"}}}
	if got := toolProgressLabel(changedDiff); got != `changed_diff_read ["a.go","b.go"]` {
		t.Fatalf("unexpected changed diff label %q", got)
	}
	search := agent.ToolCall{Tool: "code_search", Arguments: agent.ToolArguments{SearchText: "type TestSuite struct", FilePatterns: []string{"*.go", ":(exclude)*_test.go"}}}
	if got := toolProgressLabel(search); got != `code_search "type TestSuite struct" in ["*.go",":(exclude)*_test.go"]` {
		t.Fatalf("unexpected code search label %q", got)
	}
	find := agent.ToolCall{Tool: "file_find", Arguments: agent.ToolArguments{QueryName: "StepJunitReportSpec"}}
	if got := toolProgressLabel(find); got != `file_find "StepJunitReportSpec"` {
		t.Fatalf("unexpected file find label %q", got)
	}
}

func TestCompleteTrackedRetriesTimeout(t *testing.T) {
	llm := &sequenceLLM{
		responses: []protocol.Response{{}, {Text: `{"findings":[]}`, Usage: agent.TokenUsage{TotalTokens: 3}}},
		errors:    []error{timeoutError{}, nil},
	}
	r := Runner{Config: config.Default(), LLM: llm}
	var usage agent.TokenUsage
	response, err := r.completeTracked(context.Background(), protocol.Request{Messages: []protocol.Message{{Role: protocol.RoleUser, Content: "small prompt"}}}, &usage)
	if err != nil {
		t.Fatal(err)
	}
	if response.Text == "" || usage.LLMRequests != 2 || usage.TotalTokens != 3 {
		t.Fatalf("unexpected retry result: response=%+v usage=%+v", response, usage)
	}
}

func TestElapsedMilliseconds(t *testing.T) {
	if got := elapsedMilliseconds(500 * time.Microsecond); got != 1 {
		t.Fatalf("sub-millisecond duration must be observable, got %d", got)
	}
	if got := elapsedMilliseconds(1500 * time.Millisecond); got != 1500 {
		t.Fatalf("unexpected duration %d", got)
	}
}

func TestReviewFilterFailureMakesRunIncomplete(t *testing.T) {
	file := reviewTestFile()
	llm := &sequenceLLM{
		responses: []protocol.Response{
			commentAndDoneResponse(agent.Finding{Severity: "medium", Category: "correctness", File: "main.go", StartLine: 10, EndLine: 10, ExistingCode: `panic("x")`, Title: "panic", Problem: "panic added", Evidence: "panic", Suggestion: "return error", Confidence: 0.95}),
			{},
		},
		errors: []error{nil, errors.New("filter unavailable")},
	}
	cfg := config.Default()
	cfg.Output.Language = "English"
	r := Runner{Root: t.TempDir(), Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.ExitCode != agent.ExitIncomplete || len(report.Findings) != 1 || len(report.Warnings) != 1 {
		t.Fatalf("filter failure must not pass CI: %+v", report)
	}
	if len(report.Process.ModelResponses) != 1 || report.Process.ModelResponses[0].Stage != "review_filter" || report.Process.ModelResponses[0].Status != "error" {
		t.Fatalf("filter failure response was not audited: %+v", report.Process.ModelResponses)
	}
}

func TestReviewFilterDeletesByIDWithoutRewritingFindings(t *testing.T) {
	candidates := []agent.Finding{
		{File: "main.go", Title: "first", Problem: "keep original first"},
		{File: "main.go", Title: "second", Problem: "keep original second"},
	}
	llm := &recordingLLM{responses: []protocol.Response{{Text: `["c-0"]`}}}
	r := Runner{Config: config.Default(), LLM: llm}
	var usage agent.TokenUsage
	filtered, warning := r.filterFindings(context.Background(), candidates, map[string]string{
		"current_file_path": "main.go", "system_rule": "rule", "diff": "diff",
	}, &usage)
	if warning != "" || len(filtered) != 1 || !reflect.DeepEqual(filtered[0], candidates[1]) {
		t.Fatalf("filter must only delete the selected original finding: filtered=%+v warning=%q", filtered, warning)
	}
	if len(llm.requests) != 1 || !requestContains(llm.requests[0], `"id":"c-0"`) || !requestContains(llm.requests[0], `"id":"c-1"`) {
		t.Fatalf("candidate IDs missing from filter request: %+v", llm.requests)
	}
}

func TestReviewFilterAcceptsWrappedDeletedIDs(t *testing.T) {
	candidates := []agent.Finding{{File: "main.go", Title: "first"}, {File: "main.go", Title: "second"}}
	r := Runner{Config: config.Default(), LLM: &sequenceLLM{responses: []protocol.Response{{Text: `{"deleted_ids":["c-1"]}`}}}}
	filtered, warning := r.filterFindings(context.Background(), candidates, map[string]string{
		"current_file_path": "main.go", "system_rule": "rule", "diff": "diff",
	}, &agent.TokenUsage{})
	if warning != "" || len(filtered) != 1 || !reflect.DeepEqual(filtered[0], candidates[0]) {
		t.Fatalf("wrapped IDs were not accepted: filtered=%+v warning=%q", filtered, warning)
	}
}

func TestReviewFilterRetriesInvalidResponse(t *testing.T) {
	llm := &recordingLLM{responses: []protocol.Response{{Text: `{"deleted_ids":`}, {Text: `[]`}}}
	r := Runner{Config: config.Default(), LLM: llm}
	candidates := []agent.Finding{{File: "main.go", Title: "keep"}}
	filtered, warning := r.filterFindings(context.Background(), candidates, map[string]string{
		"current_file_path": "main.go", "system_rule": "rule", "diff": "diff",
	}, &agent.TokenUsage{})
	if warning != "" || len(filtered) != 1 || len(llm.requests) != 2 {
		t.Fatalf("invalid response was not repaired by retry: filtered=%+v warning=%q requests=%d", filtered, warning, len(llm.requests))
	}
}

func TestFileReadCacheMatchesContainingRange(t *testing.T) {
	action := toolAction{Tool: "file_read", FilePath: "main.go", StartLine: 20, EndLine: 40}
	cachedAction := toolAction{Tool: "file_read", FilePath: "main.go", StartLine: 1, EndLine: 100}
	output := "File: main.go (Total lines: 200)\nIS_TRUNCATED: false\nLINE_RANGE: 1-100\n1|package main\n"
	cache := map[string]cachedContextTool{
		contextToolCacheKey(cachedAction): {action: cachedAction, result: toolExecution{Output: output, Status: "success", Summary: "100 lines read"}},
	}
	got, ok := findCachedContextTool(action, cache)
	if !ok || got.result.Output != output {
		t.Fatalf("containing file range did not hit cache: ok=%v result=%+v", ok, got.result)
	}
	outside := toolAction{Tool: "file_read", FilePath: "main.go", StartLine: 90, EndLine: 120}
	if _, ok := findCachedContextTool(outside, cache); ok {
		t.Fatal("partially overlapping range must not hit cache")
	}
}

func TestContextToolBudgetScalesWithChangedLines(t *testing.T) {
	for _, test := range []struct {
		changed int
		want    int
	}{{1, 6}, {10, 6}, {11, 8}, {50, 8}, {51, 15}} {
		if got := contextToolBudget(15, test.changed); got != test.want {
			t.Fatalf("contextToolBudget(15, %d) = %d, want %d", test.changed, got, test.want)
		}
	}
	if got := contextToolBudget(4, 1); got != 4 {
		t.Fatalf("configured lower limit must be preserved, got %d", got)
	}
}

func TestReviewFilterInvalidResponseKeepsOriginalFindings(t *testing.T) {
	candidates := []agent.Finding{{File: "main.go", Title: "original"}}
	r := Runner{Config: config.Default(), LLM: &sequenceLLM{responses: []protocol.Response{{Text: `{"findings":[]}`}}}}
	var usage agent.TokenUsage
	filtered, warning := r.filterFindings(context.Background(), candidates, map[string]string{
		"current_file_path": "main.go", "system_rule": "rule", "diff": "diff",
	}, &usage)
	if len(filtered) != 1 || !reflect.DeepEqual(filtered[0], candidates[0]) || !strings.HasPrefix(warning, "review_filter_invalid_response:") {
		t.Fatalf("invalid response must preserve findings: filtered=%+v warning=%q", filtered, warning)
	}
}

func TestReviewFilterReportsTruncatedModelOutput(t *testing.T) {
	candidates := []agent.Finding{{File: "main.go", Title: "original"}}
	llm := &recordingLLM{responses: []protocol.Response{
		{Text: `["TRUNCATED_SENTINEL`, FinishReason: "length", Usage: agent.TokenUsage{CompletionTokens: 4096}},
		{Text: `["still truncated`, FinishReason: "length", Usage: agent.TokenUsage{CompletionTokens: 4096}},
	}}
	r := Runner{Config: config.Default(), LLM: llm}
	filtered, warning := r.filterFindings(context.Background(), candidates, map[string]string{
		"current_file_path": "main.go", "system_rule": "rule", "diff": "diff",
	}, &agent.TokenUsage{})
	if len(filtered) != 1 || !reflect.DeepEqual(filtered[0], candidates[0]) {
		t.Fatalf("truncated filter must preserve candidates: %+v", filtered)
	}
	for _, want := range []string{
		"review_filter_invalid_response: model output truncated", `agent_source="internal/reviewer/agent_loop.go:`,
		"finish_reason=\"length\"", "completion_tokens=4096", "visible_chars=",
	} {
		if !strings.Contains(warning, want) {
			t.Fatalf("truncation warning missing %q: %s", want, warning)
		}
	}
	if len(llm.requests) != 2 || requestContains(llm.requests[1], "TRUNCATED_SENTINEL") {
		t.Fatalf("truncated output must not be replayed into the retry: %+v", llm.requests)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestRunnerAggregatesConcurrentUsage(t *testing.T) {
	files := []gitdiff.FileDiff{
		{Path: "a.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}},
		{Path: "b.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}},
	}
	cfg := config.Default()
	cfg.Review.Concurrency = 2
	r := Runner{
		Root:         "/repo",
		Config:       cfg,
		Git:          fakeGit{files: files},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          fixedLLM{response: withUsage(doneResponse(), agent.TokenUsage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6, CacheReadTokens: 2})},
		RuleResolver: rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceSystem, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: "test rule"}}}}}},
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.PromptTokens != 10 || report.Usage.CompletionTokens != 2 || report.Usage.TotalTokens != 12 || report.Usage.LLMRequests != 2 || report.Usage.CacheReadTokens != 4 {
		t.Fatalf("unexpected concurrent usage: %+v", report.Usage)
	}
}

func TestRunnerRecordsConcurrentToolCallsInOrder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []gitdiff.FileDiff{
		{Path: "a.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}},
		{Path: "b.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}},
	}
	cfg := config.Default()
	cfg.Review.Concurrency = 2
	r := Runner{Root: root, Config: cfg, Git: fakeGit{files: files}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: contextToolLLM{}, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || len(report.Process.ToolCalls) != 2 {
		t.Fatalf("unexpected concurrent tool process: %+v", report)
	}
	if report.Process.ToolCalls[0].ID != "tool-0001" || report.Process.ToolCalls[1].ID != "tool-0002" {
		t.Fatalf("tool calls are not ordered: %+v", report.Process.ToolCalls)
	}
}

type contextToolLLM struct{}

func (contextToolLLM) Complete(_ context.Context, req protocol.Request) (protocol.Response, error) {
	for _, message := range req.Messages {
		if message.Role == protocol.RoleTool {
			return doneResponse(), nil
		}
	}
	return toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":10}`), nil
}

func TestRunnerReportsProgress(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	var mu sync.Mutex
	var messages []string
	r := Runner{
		Root:         "/repo",
		Config:       config.Default(),
		Git:          fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          fixedLLM{response: doneResponse()},
		RuleResolver: testRuleResolver(),
		Started: func(report agent.Report) {
			mu.Lock()
			defer mu.Unlock()
			messages = append(messages, fmt.Sprintf("started files=%d chunks=%d", report.Stats.ChangedFiles, report.Stats.Chunks))
		},
		Progress: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			messages = append(messages, fmt.Sprintf(format, args...))
		},
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(messages, "\n")
	mu.Unlock()
	for _, want := range []string{"started files=1 chunks=1", "Reviewing 1/1: main.go", "plan skipped", "completed", "Review completed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("progress missing %q:\n%s", want, joined)
		}
	}
	if len(messages) == 0 || !strings.HasPrefix(messages[0], "started files=") {
		t.Fatalf("review summary must precede progress events: %v", messages)
	}
	if report.ExitCode != agent.ExitOK {
		t.Fatalf("progress must not change review result: %+v", report)
	}
}

func TestRunnerReportsRuleReferenceWarningsWithoutFailingReview(t *testing.T) {
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
	resolver := testRuleResolver()
	resolver.Warnings = []string{`rule_reference_failed: rules.json rules[0] "missing.md": file not found`}
	r := Runner{
		Root:         "/repo",
		Config:       config.Default(),
		Git:          fakeGit{files: []gitdiff.FileDiff{file}},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          fixedLLM{response: doneResponse()},
		RuleResolver: resolver,
	}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "rule_reference_failed") {
		t.Fatalf("rule reference warning must be visible but non-blocking: %+v", report)
	}
}

func TestRunnerProgressLabelsConcurrentFiles(t *testing.T) {
	files := []gitdiff.FileDiff{
		{Path: "pkg/a.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}},
		{Path: "internal/b.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}},
	}
	cfg := config.Default()
	cfg.Review.Concurrency = 2
	var mu sync.Mutex
	var messages []string
	r := Runner{
		Root:         "/repo",
		Config:       cfg,
		Git:          fakeGit{files: files},
		DiffRequest:  gitdiff.Request{Mode: gitdiff.ModeWorkspace},
		LLM:          fixedLLM{response: doneResponse()},
		RuleResolver: testRuleResolver(),
		Progress: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			messages = append(messages, fmt.Sprintf(format, args...))
		},
	}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(messages, "\n")
	mu.Unlock()
	for _, want := range []string{"[a.go] plan skipped", "[a.go] completed", "[b.go] plan skipped", "[b.go] completed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("concurrent progress missing %q:\n%s", want, joined)
		}
	}
}

func TestRunnerToolLoopReadsContextBeforeComment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte("package p\nfunc dependency() bool { return false }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":20}`),
		commentAndDoneResponse(agent.Finding{Severity: "high", Category: "correctness", File: "main.go", ExistingCode: `panic("x")`, Title: "panic", Problem: "panic added", Evidence: "panic", Suggestion: "return error", Confidence: 0.95}),
		{Text: `[]`},
	}}
	var progressMu sync.Mutex
	var progress []string
	cfg := config.Default()
	cfg.Output.Language = "English"
	r := Runner{Root: root, Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver(), Progress: func(format string, args ...any) {
		progressMu.Lock()
		defer progressMu.Unlock()
		progress = append(progress, fmt.Sprintf(format, args...))
	}}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || report.Findings[0].StartLine != 10 || report.Usage.LLMRequests != 3 {
		t.Fatalf("unexpected tool-loop report: %+v", report)
	}
	if len(llm.requests) < 2 || !requestContains(llm.requests[1], "dependency() bool") {
		t.Fatalf("tool result was not injected into the next round: %+v", llm.requests)
	}
	if len(report.Process.ToolCalls) != 2 {
		t.Fatalf("expected context and comment tool calls: %+v", report.Process)
	}
	call := report.Process.ToolCalls[0]
	if call.ID != "tool-0001" || call.Tool != "file_read" || call.Round != 1 || call.Arguments.FilePath != "dep.go" || call.Arguments.StartLine != 1 || call.Arguments.EndLine != 20 || call.Status != "success" || !strings.Contains(call.Output, "dependency() bool") || call.OutputBytes == 0 || call.DurationMS < 1 {
		t.Fatalf("unexpected tool call record: %+v", call)
	}
	commentCall := report.Process.ToolCalls[1]
	if commentCall.Tool != "code_comment" || commentCall.Arguments.Finding == nil || commentCall.Arguments.Finding.File != "main.go" {
		t.Fatalf("code comment was not recorded: %+v", commentCall)
	}
	progressMu.Lock()
	progressText := strings.Join(progress, "\n")
	progressMu.Unlock()
	if !strings.Contains(progressText, `file_read "dep.go" (`) || !strings.Contains(progressText, `): lines 1-20`) || !strings.Contains(progressText, `code_comment (`) || strings.Contains(progressText, "[main.go]") || strings.Contains(progressText, "tool-0001") || strings.Contains(progressText, "file_path=") {
		t.Fatalf("tool progress is missing call details:\n%s", progressText)
	}
	if strings.Contains(progressText, "dependency() bool") {
		t.Fatalf("tool progress must not print source output:\n%s", progressText)
	}
	if strings.Contains(progressText, "output_bytes=") || strings.Contains(progressText, "truncated=") {
		t.Fatalf("tool progress must keep output metadata in JSON only:\n%s", progressText)
	}
}

func TestRunnerNormalizesInvalidToolCallsBeforeReplayingHistory(t *testing.T) {
	for _, arguments := range []string{"", `{"search_text":`} {
		t.Run(fmt.Sprintf("arguments_%q", arguments), func(t *testing.T) {
			file := reviewTestFile()
			llm := &recordingLLM{responses: []protocol.Response{
				toolResponse("", "code_search", arguments),
				doneResponse(),
			}}
			r := Runner{Root: t.TempDir(), Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
			report, err := r.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Incomplete || report.ExitCode != agent.ExitOK {
				t.Fatalf("invalid tool arguments should remain recoverable: %+v", report)
			}
			if len(llm.requests) != 2 {
				t.Fatalf("expected the corrected history on a second request, got %d requests", len(llm.requests))
			}

			var assistant *protocol.Message
			var toolResult *protocol.Message
			for index := range llm.requests[1].Messages {
				message := &llm.requests[1].Messages[index]
				if message.Role == protocol.RoleAssistant && len(message.ToolCalls) > 0 {
					assistant = message
				}
				if message.Role == protocol.RoleTool {
					toolResult = message
				}
			}
			if assistant == nil || toolResult == nil || len(assistant.ToolCalls) != 1 {
				t.Fatalf("missing replayed tool-call pair: %+v", llm.requests[1].Messages)
			}
			call := assistant.ToolCalls[0]
			if call.ID == "" || call.Arguments != `{}` || toolResult.ToolCallID != call.ID {
				t.Fatalf("tool-call history was not normalized: call=%+v result=%+v", call, *toolResult)
			}
			if len(report.Process.ToolCalls) != 1 || report.Process.ToolCalls[0].Status != "error" || report.Process.ToolCalls[0].Summary != "invalid tool arguments" {
				t.Fatalf("original invalid call was not recorded: %+v", report.Process.ToolCalls)
			}
		})
	}
}

func TestMainPromptSeparatesTrustedInstructionsFromRepositoryData(t *testing.T) {
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{doneResponse()}}
	resolver := rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceSystem, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: "UNTRUSTED_RULE_DATA"}}}}}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: resolver}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) != 1 || len(llm.requests[0].Messages) != 2 || llm.requests[0].Messages[0].Role != protocol.RoleSystem || llm.requests[0].Messages[1].Role != protocol.RoleUser {
		t.Fatalf("unexpected main messages: %+v", llm.requests)
	}
	if strings.Contains(llm.requests[0].Messages[0].Content, "UNTRUSTED_RULE_DATA") || !strings.Contains(llm.requests[0].Messages[1].Content, "UNTRUSTED_RULE_DATA") || !strings.Contains(llm.requests[0].Messages[1].Content, `panic("x")`) {
		t.Fatalf("repository data crossed the system/user boundary: %+v", llm.requests[0].Messages)
	}
	if len(llm.requests[0].Tools) != 6 {
		t.Fatalf("expected six native tools, got %+v", llm.requests[0].Tools)
	}
	if llm.requests[0].Tools[0].Name != "task_done" || llm.requests[0].Tools[1].Name != "code_comment" {
		t.Fatalf("terminal tools must be listed first to bias completion: %+v", llm.requests[0].Tools)
	}
	if !strings.Contains(llm.requests[0].Messages[0].Content, "make one completion-oriented decision") || !strings.Contains(llm.requests[0].Messages[0].Content, "call task_done immediately") || !strings.Contains(llm.requests[0].Messages[0].Content, "task_done is the preferred action") || !strings.Contains(llm.requests[0].Messages[0].Content, "Do not inspect context merely to be exhaustive") {
		t.Fatalf("main prompt is missing the bounded decision protocol: %s", llm.requests[0].Messages[0].Content)
	}
}

func TestRunnerUsesPlanForLargeChange(t *testing.T) {
	file := reviewTestFile()
	file.Insertions = 50
	llm := &recordingLLM{responses: []protocol.Response{{Text: "check callers and error paths"}, doneResponse()}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.LLMRequests != 2 || len(llm.requests) != 2 || !requestContains(llm.requests[0], "plan a read-only code review") || !requestContains(llm.requests[1], "check callers and error paths") {
		t.Fatalf("plan phase was not applied: report=%+v requests=%+v", report, llm.requests)
	}
}

func TestRunnerRelocatesFinding(t *testing.T) {
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{
		commentAndDoneResponse(agent.Finding{Severity: "high", Category: "correctness", File: "main.go", StartLine: 99, EndLine: 99, Title: "panic", Problem: "panic added", Evidence: "panic", Suggestion: "return error", Confidence: 0.95}),
		{Text: `{"existing_code":"panic(\"x\")"}`},
		{Text: `[]`},
	}}
	cfg := config.Default()
	cfg.Output.Language = "English"
	r := Runner{Root: t.TempDir(), Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || report.Findings[0].StartLine != 10 || report.Usage.LLMRequests != 3 {
		t.Fatalf("finding was not relocated: %+v", report)
	}
}

func TestPositionFindingDoesNotTrustOverlappingLinesWhenExistingCodeMismatches(t *testing.T) {
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{{Text: `{"existing_code":"panic(\"x\")"}`}}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	finding := agent.Finding{File: file.Path, StartLine: 10, EndLine: 10, ExistingCode: "not in the diff"}
	got, ok, warning := r.positionFinding(context.Background(), finding, file, []gitdiff.FileDiff{file}, mainLoopTestValues(file), &usage, true)
	if !ok || warning != "" || got.StartLine != 10 || got.EndLine != 10 || got.ExistingCode != `panic("x")` || len(llm.requests) != 1 || usage.LLMRequests != 1 {
		t.Fatalf("mismatched existing_code must relocate instead of trusting coincident lines: finding=%+v ok=%t warning=%q requests=%d usage=%+v", got, ok, warning, len(llm.requests), usage)
	}
}

func TestPositionFindingDropsExistingCodeOwnedByAnotherChangedFileWithoutRelocation(t *testing.T) {
	file := reviewTestFile()
	other := gitdiff.FileDiff{Path: "other.go", Hunks: []gitdiff.Hunk{{
		NewStart: 20, NewLines: 1, ChangedLines: map[int]bool{20: true},
		Lines: []gitdiff.Line{{Kind: '+', NewLine: 20, Text: "return foreignResult"}},
	}}}
	llm := &recordingLLM{}
	r := Runner{Root: t.TempDir(), Config: config.Default(), LLM: llm, process: newProcessRecorder(time.Now())}
	usage := agent.TokenUsage{}
	finding := agent.Finding{File: file.Path, StartLine: 10, EndLine: 10, ExistingCode: "return foreignResult"}
	_, ok, warning := r.positionFinding(context.Background(), finding, file, []gitdiff.FileDiff{file, other}, mainLoopTestValues(file), &usage, true)
	if ok || warning != "" || len(llm.requests) != 0 || usage.LLMRequests != 0 {
		t.Fatalf("obvious foreign-file snippet must be dropped before relocation: ok=%t warning=%q requests=%d usage=%+v", ok, warning, len(llm.requests), usage)
	}
}

func TestRunnerRepairsInvalidRelocationResponse(t *testing.T) {
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{
		commentAndDoneResponse(agent.Finding{Severity: "high", Category: "correctness", File: "main.go", StartLine: 99, EndLine: 99, Title: "panic", Problem: "panic added", Evidence: "panic", Suggestion: "return error", Confidence: 0.95}),
		{Text: `not valid JSON`},
		{Text: `{"existing_code":"panic(\"x\")"}`},
		{Text: `[]`},
	}}
	cfg := config.Default()
	cfg.Output.Language = "English"
	r := Runner{Root: t.TempDir(), Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || len(report.Warnings) != 0 || len(report.Findings) != 1 || report.Findings[0].StartLine != 10 || report.Usage.LLMRequests != 4 {
		t.Fatalf("invalid relocation response was not repaired: %+v", report)
	}
	if len(llm.requests) != 4 || !requestContains(llm.requests[2], "Return only one complete JSON object") || !requestContains(llm.requests[2], "not valid JSON") {
		t.Fatalf("relocation repair request did not preserve and correct the invalid response: %+v", llm.requests)
	}
}

func TestRunnerReportsInvalidRelocationAfterRepairFails(t *testing.T) {
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{
		commentAndDoneResponse(agent.Finding{Severity: "medium", Category: "correctness", File: "main.go", StartLine: 99, EndLine: 99, Title: "candidate", Problem: "candidate", Evidence: "candidate", Suggestion: "candidate", Confidence: 0.9}),
		{Text: `not valid JSON`},
		{Text: `still not valid JSON`},
	}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Incomplete || report.ExitCode != agent.ExitIncomplete || len(report.Findings) != 0 || len(report.Warnings) != 1 || report.Warnings[0] != "relocation_invalid_response: "+file.Path || report.Usage.LLMRequests != 3 {
		t.Fatalf("failed relocation repair must remain visible: %+v", report)
	}
}

func TestRunnerDropsUnresolvedRelocationWithoutFailingReview(t *testing.T) {
	file := reviewTestFile()
	llm := &recordingLLM{responses: []protocol.Response{
		commentAndDoneResponse(agent.Finding{Severity: "medium", Category: "correctness", File: "main.go", StartLine: 99, EndLine: 99, Title: "candidate", Problem: "candidate", Evidence: "candidate", Suggestion: "candidate", Confidence: 0.9}),
		{Text: `{"existing_code":""}`},
	}}
	r := Runner{Root: t.TempDir(), Config: config.Default(), Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: llm, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Incomplete || report.ExitCode != agent.ExitOK || len(report.Findings) != 0 || len(report.Warnings) != 0 {
		t.Fatalf("unlocatable candidate must be dropped without failing the review: %+v", report)
	}
}

func TestRunnerSkipsOversizedChunk(t *testing.T) {
	file := reviewTestFile()
	file.Insertions = 1
	file.Hunks[0].Lines[0].Text = strings.Repeat("x", 4000)
	cfg := config.Default()
	cfg.Review.MaxChunkTokens = 1000
	r := Runner{Root: "/repo", Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: failLLM{t: t}, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.LLMRequests != 0 || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "token_threshold_exceeded") || !strings.Contains(report.Warnings[0], "estimated=") || !strings.Contains(report.Warnings[0], "limit=800") || report.ExitCode != agent.ExitIncomplete {
		t.Fatalf("oversized chunk was not skipped: %+v", report)
	}
	if report.Stats.Additions != 1 || report.Stats.Deletions != 0 || report.Stats.ChangedLines != 1 {
		t.Fatalf("incomplete review lost diff counts: %+v", report.Stats)
	}
}

func TestFileChunksSplitLargeHunk(t *testing.T) {
	file := reviewTestFile()
	file.Hunks[0].Lines = nil
	file.Hunks[0].ChangedLines = map[int]bool{}
	for i := 1; i <= 100; i++ {
		line := gitdiff.Line{Kind: '+', NewLine: i, Text: strings.Repeat("x", 40)}
		file.Hunks[0].Lines = append(file.Hunks[0].Lines, line)
		file.Hunks[0].ChangedLines[i] = true
	}
	chunks := fileChunks([]gitdiff.FileDiff{file}, 1000, rules.Resolver{})
	if len(chunks) < 2 {
		t.Fatalf("expected large hunk to be split, got %d chunk", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk.Hunks) != 1 || len(chunk.Hunks[0].ChangedLines) == 0 {
			t.Fatalf("chunk lost line mapping: %+v", chunk)
		}
	}
	file.Insertions = 100
	cfg := config.Default()
	cfg.Review.MaxChunkTokens = 1000
	cfg.Review.MaxTokensBudget = 1
	report, err := (Runner{Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, LLM: failLLM{t: t}}).Run(context.Background())
	if err != nil || !report.Incomplete || report.Stats.Chunks != len(chunks) || report.Stats.Additions != 100 || report.Stats.Deletions != 0 || report.Stats.ChangedLines != 100 {
		t.Fatalf("chunking or budget stop changed diff counts: report=%+v err=%v", report, err)
	}
}

func TestFileChunksAccountsForMatchingRules(t *testing.T) {
	const maxTokens = 1000
	file := reviewTestFile()
	file.Hunks[0].Lines = nil
	file.Hunks[0].ChangedLines = map[int]bool{}
	resolver := rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceProject, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: strings.Repeat("format rule ", 180)}}}}}}
	ruleTokens := estimateTokens(renderReviewRules(resolver.ResolveAll(file.Path)))
	if ruleTokens >= maxTokens*4/5 {
		t.Fatalf("test rule itself exceeds token limit: %d", ruleTokens)
	}
	for line := 1; estimateTokens(renderFileDiff(file)) <= maxTokens*4/5-ruleTokens; line++ {
		file.Hunks[0].Lines = append(file.Hunks[0].Lines, gitdiff.Line{Kind: '+', NewLine: line, Text: "value := calculateResult(input)"})
		file.Hunks[0].ChangedLines[line] = true
	}
	if estimateTokens(renderFileDiff(file)) > maxTokens*7/10 {
		t.Fatal("test diff unexpectedly exceeds the original chunk target")
	}
	chunks := fileChunks([]gitdiff.FileDiff{file}, maxTokens, resolver)
	if len(chunks) < 2 {
		t.Fatalf("expected rule-aware split, got %d chunk", len(chunks))
	}
	for _, chunk := range chunks {
		if tokens := ruleTokens + estimateTokens(renderFileDiff(chunk)); tokens > maxTokens*4/5 {
			t.Fatalf("chunk still exceeds review threshold: %d", tokens)
		}
	}
}

func reviewTestFile() gitdiff.FileDiff {
	return gitdiff.FileDiff{Path: "main.go", Insertions: 1, Hunks: []gitdiff.Hunk{{
		NewStart: 10, NewLines: 1, ChangedLines: map[int]bool{10: true},
		Lines: []gitdiff.Line{{Kind: '+', NewLine: 10, Text: `panic("x")`}},
	}}}
}

func mainLoopTestValues(file gitdiff.FileDiff) map[string]string {
	return map[string]string{
		"current_file_path": file.Path,
		"change_files":      "(none)",
		"system_rule":       "test rule",
		"diff":              renderFileDiff(file),
		"language":          "English",
		"plan_guidance":     "No separate plan was required.",
	}
}

func testRuleResolver() rules.Resolver {
	return rules.Resolver{Layers: []rules.Layer{{Source: rules.SourceSystem, File: rules.RuleFile{Rules: []rules.RuleEntry{{Path: "**/*.go", Rule: "test rule"}}}}}}
}

type recordingLLM struct {
	responses []protocol.Response
	requests  []protocol.Request
}

func (l *recordingLLM) Complete(_ context.Context, request protocol.Request) (protocol.Response, error) {
	l.requests = append(l.requests, request)
	if len(l.responses) == 0 {
		return doneResponse(), nil
	}
	response := l.responses[0]
	l.responses = l.responses[1:]
	return response, nil
}

type fixedLLM struct {
	response protocol.Response
}

func (f fixedLLM) Complete(context.Context, protocol.Request) (protocol.Response, error) {
	return f.response, nil
}

func toolResponse(id, name, arguments string) protocol.Response {
	return protocol.Response{ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: arguments}}}
}

func doneResponse() protocol.Response {
	return toolResponse("done", "task_done", `{}`)
}

func commentAndDoneResponse(finding agent.Finding) protocol.Response {
	arguments, _ := json.Marshal(map[string]any{"finding": finding})
	return protocol.Response{ToolCalls: []protocol.ToolCall{
		{ID: "comment", Name: "code_comment", Arguments: string(arguments)},
		{ID: "done", Name: "task_done", Arguments: `{}`},
	}}
}

func withUsage(response protocol.Response, usage agent.TokenUsage) protocol.Response {
	response.Usage = usage
	return response
}

func requestContains(request protocol.Request, text string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}
