package reporter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/koderover/zadig-review-agent/internal/agent"
)

func TestMarkdownIncludesFinding(t *testing.T) {
	report := agent.Report{DurationMS: 192345, Process: agent.ReviewProcess{ToolCalls: []agent.ToolCall{{ID: "tool-0001", File: "main.go", Round: 1, Tool: "code_search", Arguments: agent.ToolArguments{SearchText: "target"}, Status: "success", OutputBytes: 20, Summary: "1 matches", Output: "main.go:1: target"}}, Compressions: []agent.Compression{{ID: "compression-0001", File: "main.go", Round: 3, Status: "success", DurationMS: 500, BeforeTokens: 9000, AfterTokens: 3000, Usage: agent.TokenUsage{LLMRequests: 1}}}}, Usage: agent.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, LLMRequests: 2, CacheReadTokens: 3, CacheWriteTokens: 4}, Warnings: []string{"token_threshold_exceeded: huge.go"}, Findings: []agent.Finding{{
		Severity: "high",
		Category: "correctness",
		File:     "main.go",
		Title:    "Bug",
	}}}
	md := Markdown(report)
	if !strings.Contains(md, "[high] Bug") || !strings.Contains(md, "main.go") || !strings.Contains(md, "Review duration: `3m12.345s`") || !strings.Contains(md, "Tool calls: `1`") || !strings.Contains(md, "Context compressions: `1`") || !strings.Contains(md, "## Context Compressions") || !strings.Contains(md, "tokens `9000 -> 3000`") || !strings.Contains(md, "## Tool Calls") || !strings.Contains(md, "`tool-0001` `code_search`") || !strings.Contains(md, "Prompt Tokens: `10`") || !strings.Contains(md, "Cache Write Tokens: `4`") || !strings.Contains(md, "token_threshold_exceeded") {
		t.Fatalf("markdown missing finding:\n%s", md)
	}
	console := Console(report, "summary")
	if !strings.Contains(console, "Code review:") || !strings.Contains(console, "Changed files:") || !strings.Contains(console, "Review result: findings=1 status=complete") || !strings.Contains(console, "Review duration: 3m12.345s") || !strings.Contains(console, "prompt=10 completion=2 total=12 requests=2") || !strings.Contains(console, "read=3 write=4") || !strings.Contains(console, "Tool calls: 1") || !strings.Contains(console, "Context compressions: 1") {
		t.Fatalf("console missing usage:\n%s", console)
	}
	detailed := Console(report, "detailed")
	if !strings.Contains(detailed, "─── main.go:0-0 ───") || !strings.Contains(detailed, "[correctness · high] **Bug**") {
		t.Fatalf("console finding format is not grouped:\n%s", detailed)
	}
	if !strings.Contains(console, "excluded: 0\n\nReview result:") {
		t.Fatalf("console sections must be separated by a blank line:\n%s", console)
	}
	start := ConsoleStart(report)
	if !strings.Contains(start, "Code review:") || !strings.Contains(start, "Review process:") || strings.Contains(start, "Review result:") {
		t.Fatalf("unexpected review start output:\n%s", start)
	}
	result := ConsoleResult(report, "summary")
	if strings.Contains(result, "Code review:") || !strings.HasPrefix(result, "Review result:") {
		t.Fatalf("result must not repeat review header:\n%s", result)
	}
	report.Metadata.ReportDir = "/tmp/reports/run"
	report.Metadata.JSONReport = "/tmp/reports/run/review-report.json"
	report.Metadata.MDReport = "/tmp/reports/run/review-report.md"
	result = ConsoleResult(report, "detailed")
	if strings.Contains(result, "Report dir:") || strings.Contains(result, "JSON report:") || strings.Contains(result, "Markdown report:") {
		t.Fatalf("console must not print report paths:\n%s", result)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"duration_ms":192345`) {
		t.Fatalf("json missing machine-readable duration: %s", data)
	}
	if !strings.Contains(string(data), `"tool_calls":[`) || !strings.Contains(string(data), `"compressions":[`) || !strings.Contains(string(data), `"before_tokens":9000`) || !strings.Contains(string(data), `"search_text":"target"`) || !strings.Contains(string(data), `"output":"main.go:1: target"`) {
		t.Fatalf("json missing tool process details: %s", data)
	}
}

func TestJSONIncludesEmptyCompressions(t *testing.T) {
	data, err := json.Marshal(agent.Report{Process: agent.ReviewProcess{ToolCalls: []agent.ToolCall{}, Compressions: []agent.Compression{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"compressions":[]`) {
		t.Fatalf("empty compression history must remain observable: %s", data)
	}
}
