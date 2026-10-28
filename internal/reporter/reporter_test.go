package reporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/version"
)

func TestMarkdownIncludesFinding(t *testing.T) {
	report := agent.Report{DurationMS: 192345, Process: agent.ReviewProcess{ToolCalls: []agent.ToolCall{{ID: "tool-0001", File: "main.go", Round: 1, Tool: "code_search", Arguments: agent.ToolArguments{SearchText: "target"}, Status: "success", OutputBytes: 20, Summary: "1 matches", Output: "main.go:1: target"}}, Compressions: []agent.Compression{{ID: "compression-0001", File: "main.go", Round: 3, Status: "success", DurationMS: 500, BeforeTokens: 9000, AfterTokens: 3000, Usage: agent.TokenUsage{LLMRequests: 1}}}}, Usage: agent.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, LLMRequests: 2, CacheReadTokens: 3, CacheWriteTokens: 4}, Warnings: []string{"token_threshold_exceeded: huge.go"}, Findings: []agent.Finding{{
		Severity: "high",
		Category: "reliability",
		File:     "main.go",
		Title:    "Bug",
	}}}
	md := Markdown(report)
	if !strings.Contains(md, "[高] Bug") || !strings.Contains(md, "main.go") || !strings.Contains(md, "Review duration: `3m12.345s`") || !strings.Contains(md, "Tool calls: `1`") || !strings.Contains(md, "Context compressions: `1`") || !strings.Contains(md, "## Context Compressions") || !strings.Contains(md, "tokens `9000 -> 3000`") || !strings.Contains(md, "## Tool Calls") || !strings.Contains(md, "`tool-0001` `code_search`") || !strings.Contains(md, "Prompt Tokens: `10`") || !strings.Contains(md, "Cache Write Tokens: `4`") || !strings.Contains(md, "token_threshold_exceeded") {
		t.Fatalf("markdown missing finding:\n%s", md)
	}
	console := Console(report, "summary")
	if !strings.Contains(console, "Code review:") || !strings.Contains(console, "Changed files:") || !strings.Contains(console, "Review result: findings=1 status=complete") || !strings.Contains(console, "Review duration: 3m12.345s") || !strings.Contains(console, "prompt=10 completion=2 total=12 requests=2") || !strings.Contains(console, "read=3 write=4") || !strings.Contains(console, "Tool calls: 1") || !strings.Contains(console, "Context compressions: 1") {
		t.Fatalf("console missing usage:\n%s", console)
	}
	detailed := Console(report, "detailed")
	if !strings.Contains(detailed, "─── main.go:0-0 ───") || !strings.Contains(detailed, "[正确性与可靠性 · high] **Bug**") {
		t.Fatalf("console finding format is not grouped:\n%s", detailed)
	}
	if !strings.Contains(console, "excluded: 0\n\nReview result:") {
		t.Fatalf("console sections must be separated by a blank line:\n%s", console)
	}
	start := ConsoleStart(report)
	if !strings.Contains(start, "version="+strconv.Quote(version.String())) || !strings.Contains(start, "Code review:") || !strings.Contains(start, "Review process:") || strings.Contains(start, "Review result:") {
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

func TestJSONIncludesLineStats(t *testing.T) {
	for _, stats := range []agent.Stats{{}, {Additions: 2, Deletions: 1, ChangedLines: 3}} {
		path := filepath.Join(t.TempDir(), "report.json")
		if err := Write(agent.Report{Stats: stats}, path, "", nil, "none"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw["stats"], &fields); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]int{"additions": stats.Additions, "deletions": stats.Deletions, "changed_lines": stats.ChangedLines} {
			value, ok := fields[name]
			if !ok {
				t.Fatalf("missing %s: %s", name, data)
			}
			var got int
			if err := json.Unmarshal(value, &got); err != nil || got != want {
				t.Fatalf("%s=%s want %d err=%v", name, value, want, err)
			}
		}
	}
}

func TestReportsUseChineseLabelsAndEnglishConsoleSeverities(t *testing.T) {
	for _, language := range []string{"zh-CN", "en-US"} {
		for _, category := range []struct{ id, name string }{
			{"style", "代码规范"}, {"reliability", "正确性与可靠性"},
			{"maintainability", "可维护性"}, {"performance", "性能效率"},
			{"security", "安全风险"}, {"tests", "测试质量"}, {"build", "构建与交付"},
		} {
			for _, severity := range []struct{ id, name string }{
				{"critical", "严重"}, {"high", "高"}, {"medium", "中"}, {"low", "低"},
			} {
				t.Run(language+"/"+category.id+"/"+severity.id, func(t *testing.T) {
					report := agent.Report{
						Metadata: agent.Metadata{Language: language},
						Stats:    agent.Stats{BySeverity: map[string]int{severity.id: 1}},
						Findings: []agent.Finding{{Category: category.id, CategoryName: category.name, Severity: severity.id, Title: "issue"}},
					}
					md := Markdown(report)
					console := ConsoleResult(report, "detailed")
					if !strings.Contains(md, "- 分类："+category.name) || !strings.Contains(md, "### ["+severity.name+"] issue") || !strings.Contains(md, "- "+severity.name+": `1`") {
						t.Fatalf("Markdown labels are not Chinese:\n%s", md)
					}
					if !strings.Contains(console, "["+category.name+" · "+severity.id+"]") || !strings.Contains(console, severity.id+": 1") || !strings.Contains(ConsoleResult(report, "summary"), severity.id+": 1") {
						t.Fatalf("console category or severity labels are incorrect:\n%s", console)
					}
					data, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					for _, field := range []string{`"category":"` + category.id + `"`, `"category_name":"` + category.name + `"`, `"severity":"` + severity.id + `"`, `"by_severity":{"` + severity.id + `":1}`} {
						if !strings.Contains(string(data), field) {
							t.Fatalf("JSON field missing %s: %s", field, data)
						}
					}
				})
			}
		}
	}
}

func TestReportsShowNamedAndUnattributedFindings(t *testing.T) {
	report := agent.Report{
		Findings: []agent.Finding{
			{File: "main.go", Title: "multi", RuleID: "project:1", RuleName: "Go security", MatchedRules: []agent.FindingRule{{ID: "project:1", Name: "Go security"}, {ID: "project:2", Name: "Go style"}}},
			{File: "main.go", Title: "named", RuleID: "project:2", RuleName: "Go style"},
			{File: "main.go", Title: "unattributed"},
		},
		ResolvedRules: []agent.ResolvedRule{{File: "main.go", ID: "project:2", Name: "Go style", Source: "Project config", Pattern: "**/*.go"}},
	}
	md := Markdown(report)
	console := ConsoleResult(report, "detailed")
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "Rules: Go security (`project:1`), Go style (`project:2`)") || !strings.Contains(md, "Rule: Go style (`project:2`)") || !strings.Contains(md, "Rule: unattributed") || !strings.Contains(md, "Go style (`project:2`), Project config") {
		t.Fatalf("markdown rule attribution missing:\n%s", md)
	}
	if !strings.Contains(console, "Rules: Go security (project:1), Go style (project:2)") || !strings.Contains(console, "Rule: Go style (project:2)") || !strings.Contains(console, "Rule: unattributed") {
		t.Fatalf("console rule attribution missing:\n%s", console)
	}
	if !strings.Contains(string(data), `"matched_rules":[{"rule_id":"project:1","rule_name":"Go security"},{"rule_id":"project:2","rule_name":"Go style"}]`) || !strings.Contains(string(data), `"name":"Go style"`) {
		t.Fatalf("json rule attribution missing: %s", data)
	}
}

func TestConsoleSummaryShowsIncompleteWarning(t *testing.T) {
	report := agent.Report{
		Incomplete: true,
		ExitCode:   agent.ExitIncomplete,
		Warnings:   []string{"token_threshold_exceeded: main.go (estimated=10000 limit=9600)"},
	}
	result := ConsoleResult(report, "summary")
	if !strings.Contains(result, "Warnings: 1\n- token_threshold_exceeded: main.go (estimated=10000 limit=9600)") || !strings.Contains(result, "Exit code: 2") {
		t.Fatalf("incomplete summary must show the cause: %s", result)
	}
}
