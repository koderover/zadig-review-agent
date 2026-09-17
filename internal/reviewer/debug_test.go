package reviewer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/config"
	"github.com/koderover/zadig-review-agent/internal/protocol"
)

func TestLLMDebugRecorderCapturesEveryRetryRequestAndResponse(t *testing.T) {
	debug := NewLLMDebugRecorder()
	cfg := config.Default()
	cfg.Model.Protocol = "openai"
	cfg.Model.Name = "debug-model"
	cfg.Model.APIKey = "must-not-be-logged"
	r := Runner{
		Config: cfg,
		Debug:  debug,
		LLM: &sequenceLLM{
			errors: []error{&protocol.HTTPError{Protocol: "openai", StatusCode: 500, Status: "500", Body: "retry"}},
			responses: []protocol.Response{
				{Text: "partial"},
				{ToolCalls: []protocol.ToolCall{{ID: "done", Name: "task_done", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: agent.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}},
			},
		},
	}
	request := protocol.Request{
		Messages:    []protocol.Message{{Role: protocol.RoleSystem, Content: "SYSTEM_PROMPT"}, {Role: protocol.RoleUser, Content: "SOURCE_DIFF"}},
		Tools:       []protocol.ToolDefinition{{Name: "task_done", Description: "finish", Parameters: map[string]any{"type": "object"}}},
		RequireTool: true,
	}
	var usage agent.TokenUsage
	response, err := r.completeDiagnosed(context.Background(), "review", "main.go", 3, request, &usage)
	if err != nil || len(response.ToolCalls) != 1 || usage.LLMRequests != 2 {
		t.Fatalf("unexpected retried completion: response=%+v usage=%+v err=%v", response, usage, err)
	}

	path := filepath.Join(t.TempDir(), "llm-debug.jsonl")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := debug.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), cfg.Model.APIKey) {
		t.Fatalf("debug log leaked API key:\n%s", data)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected request/response pairs for two actual attempts, got %d:\n%s", len(lines), data)
	}
	var events []llmDebugEvent
	for _, line := range lines {
		var event llmDebugEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSONL event: %v\n%s", err, line)
		}
		events = append(events, event)
	}
	if events[0].Type != "request" || events[0].ID != "llm-0001" || events[0].Stage != "review" || events[0].File != "main.go" || events[0].Sequence != 3 || events[0].TransportAttempt != 1 || events[0].Request == nil || events[0].Request.Messages[1].Content != "SOURCE_DIFF" {
		t.Fatalf("unexpected first request event: %+v", events[0])
	}
	if events[1].Type != "response" || events[1].ID != events[0].ID || events[1].Status != "error" || !strings.Contains(events[1].Error, "retry") {
		t.Fatalf("unexpected first response event: %+v", events[1])
	}
	if events[2].Type != "request" || events[2].TransportAttempt != 2 || events[3].Type != "response" || events[3].ID != events[2].ID || events[3].Status != "success" || events[3].Response == nil || events[3].Response.ToolCalls[0].Name != "task_done" {
		t.Fatalf("unexpected retry pair: request=%+v response=%+v", events[2], events[3])
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("debug log mode = %o, want 600", info.Mode().Perm())
	}

	markdownPath := filepath.Join(t.TempDir(), "llm-debug.md")
	if err := debug.WriteFile(markdownPath); err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"# LLM Debug Log",
		"## Call llm-0001",
		"## Call llm-0002",
		"### Request",
		"### Response",
		"#### Message 1 · system",
		"SYSTEM_PROMPT",
		"SOURCE_DIFF",
		"| Status | error |",
		`"name": "task_done"`,
	} {
		if !strings.Contains(string(markdown), expected) {
			t.Fatalf("Markdown debug log is missing %q:\n%s", expected, markdown)
		}
	}
}
