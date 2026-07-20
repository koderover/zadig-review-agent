package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	files []gitdiff.FileDiff
}

func (f fakeGit) Head(context.Context) (string, error) {
	return "HEADSHA", nil
}

func (f fakeGit) Diff(context.Context, gitdiff.Request) ([]gitdiff.FileDiff, error) {
	return f.files, nil
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
	file := gitdiff.FileDiff{Path: "main.go", Hunks: []gitdiff.Hunk{{ChangedLines: map[int]bool{1: true}}}}
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

func TestValidateFindingsNormalizesModelEnumVariants(t *testing.T) {
	file := reviewTestFile()
	findings, err := validateFindings([]agent.Finding{{
		Severity: "Medium", Category: "Error Handling", File: "main.go",
		StartLine: 10, EndLine: 10, Title: "lost context", Problem: "raw error returned",
		Confidence: 0.9,
	}}, file, rules.ResolvedRule{Source: "system", Pattern: "**/*.go"}, 0.75)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != "medium" || findings[0].Category != "correctness" {
		t.Fatalf("model enum variants were not normalized: %+v", findings)
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
	if warning != "" || len(filtered) != 1 || filtered[0] != candidates[1] {
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
	if warning != "" || len(filtered) != 1 || filtered[0] != candidates[0] {
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
	if len(filtered) != 1 || filtered[0] != candidates[0] || !strings.HasPrefix(warning, "review_filter_invalid_response:") {
		t.Fatalf("invalid response must preserve findings: filtered=%+v warning=%q", filtered, warning)
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
	if len(llm.requests[0].Tools) != 5 {
		t.Fatalf("expected five native tools, got %+v", llm.requests[0].Tools)
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
	file.Hunks[0].Lines[0].Text = strings.Repeat("x", 4000)
	cfg := config.Default()
	cfg.Review.MaxChunkTokens = 1000
	r := Runner{Root: "/repo", Config: cfg, Git: fakeGit{files: []gitdiff.FileDiff{file}}, DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace}, LLM: failLLM{t: t}, RuleResolver: testRuleResolver()}
	report, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.LLMRequests != 0 || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "token_threshold_exceeded") || report.ExitCode != agent.ExitIncomplete {
		t.Fatalf("oversized chunk was not skipped: %+v", report)
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
	chunks := fileChunks([]gitdiff.FileDiff{file}, 1000)
	if len(chunks) < 2 {
		t.Fatalf("expected large hunk to be split, got %d chunk", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk.Hunks) != 1 || len(chunk.Hunks[0].ChangedLines) == 0 {
			t.Fatalf("chunk lost line mapping: %+v", chunk)
		}
	}
}

func reviewTestFile() gitdiff.FileDiff {
	return gitdiff.FileDiff{Path: "main.go", Insertions: 1, Hunks: []gitdiff.Hunk{{
		NewStart: 10, NewLines: 1, ChangedLines: map[int]bool{10: true},
		Lines: []gitdiff.Line{{Kind: '+', NewLine: 10, Text: `panic("x")`}},
	}}}
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
