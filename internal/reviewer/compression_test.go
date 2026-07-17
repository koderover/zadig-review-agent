package reviewer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/config"
	"github.com/koderover/zadig-code-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-code-review-agent/internal/protocol"
)

func TestPartitionForCompressionPreservesRecentCompleteRounds(t *testing.T) {
	messages := []protocol.Message{
		{Role: protocol.RoleSystem, Content: "system"},
		{Role: protocol.RoleUser, Content: "diff"},
		{Role: protocol.RoleAssistant, ToolCalls: []protocol.ToolCall{{ID: "a", Name: "file_read"}}},
		{Role: protocol.RoleTool, ToolCallID: "a", Content: "old result"},
		{Role: protocol.RoleAssistant, ToolCalls: []protocol.ToolCall{{ID: "b", Name: "code_search"}}},
		{Role: protocol.RoleTool, ToolCallID: "b", Content: "recent result"},
		{Role: protocol.RoleAssistant, ToolCalls: []protocol.ToolCall{{ID: "c", Name: "file_read"}}},
		{Role: protocol.RoleTool, ToolCallID: "c", Content: "latest result"},
	}
	partition, ok := partitionForCompression(messages, 2)
	if !ok || partition.compressEnd != 4 || partition.activeStart != 4 {
		t.Fatalf("unexpected partition: %+v ok=%t", partition, ok)
	}
	if messages[partition.activeStart].ToolCalls[0].ID != "b" {
		t.Fatalf("active zone starts in the wrong round: %+v", messages[partition.activeStart])
	}
	all, ok := partitionForCompression(messages[:4], 0)
	if !ok || all.compressEnd != 4 || all.activeStart != 4 {
		t.Fatalf("hard-limit partition must allow all completed rounds to be summarized: %+v ok=%t", all, ok)
	}
}

func TestMaybeCompressMessagesRecordsUsageAndStructuredToolHistory(t *testing.T) {
	cfg := config.Default()
	cfg.Review.MaxChunkTokens = 1000
	llm := &recordingLLM{responses: []protocol.Response{{
		Text:  "Read dep.go and confirmed the caller validates the value.",
		Usage: agent.TokenUsage{PromptTokens: 800, CompletionTokens: 20, TotalTokens: 820, CacheReadTokens: 100},
	}}}
	r := Runner{Config: cfg, LLM: llm, process: newProcessRecorder(time.Now())}
	messages := compressionTestMessages(strings.Repeat("old tool output ", 300))
	before := estimateMessagesTokens(messages)
	var usage agent.TokenUsage

	rebuilt, warning := r.maybeCompressMessages(context.Background(), "main.go", 4, messages, nil, false, &usage)
	if warning != "" {
		t.Fatalf("unexpected warning: %s", warning)
	}
	if len(rebuilt) >= len(messages) || estimateMessagesTokens(rebuilt) >= before {
		t.Fatalf("context was not reduced: before=%d after=%d messages=%d/%d", before, estimateMessagesTokens(rebuilt), len(messages), len(rebuilt))
	}
	if !strings.Contains(rebuilt[2].Content, "<previous_review_summary>") {
		t.Fatalf("summary message missing: %+v", rebuilt)
	}
	if len(llm.requests) != 1 || !requestContains(llm.requests[0], `"name":"file_read"`) || !requestContains(llm.requests[0], `\"file_path\":\"dep.go\"`) {
		t.Fatalf("structured tool call was not serialized into compression request: %+v", llm.requests)
	}
	if usage.LLMRequests != 1 || usage.TotalTokens != 820 || usage.CacheReadTokens != 100 {
		t.Fatalf("compression usage not aggregated: %+v", usage)
	}
	process := r.process.snapshot()
	if len(process.Compressions) != 1 {
		t.Fatalf("compression audit missing: %+v", process)
	}
	record := process.Compressions[0]
	if record.Status != "success" || record.BeforeTokens != before || record.AfterTokens >= record.BeforeTokens || record.Usage.LLMRequests != 1 || record.Round != 4 {
		t.Fatalf("unexpected compression audit: %+v", record)
	}
}

func TestMaybeCompressMessagesKeepsHistoryWhenCompressionFails(t *testing.T) {
	cfg := config.Default()
	cfg.Review.MaxChunkTokens = 1000
	r := Runner{
		Config:  cfg,
		LLM:     compressionErrorLLM{},
		process: newProcessRecorder(time.Now()),
	}
	messages := compressionTestMessages(strings.Repeat("old tool output ", 300))
	var usage agent.TokenUsage

	rebuilt, warning := r.maybeCompressMessages(context.Background(), "main.go", 4, messages, nil, false, &usage)
	if len(rebuilt) != len(messages) || !strings.HasPrefix(warning, "memory_compression_failed: main.go:") {
		t.Fatalf("compression failure must preserve history: warning=%q before=%d after=%d", warning, len(messages), len(rebuilt))
	}
	process := r.process.snapshot()
	if usage.LLMRequests != 1 || len(process.Compressions) != 1 || process.Compressions[0].Status != "failed" || process.Compressions[0].Error == "" {
		t.Fatalf("failed compression audit or usage missing: usage=%+v process=%+v", usage, process)
	}
}

func TestMainLoopCompressesBeforeHardTokenGuard(t *testing.T) {
	root := t.TempDir()
	var content strings.Builder
	for index := 0; index < 500; index++ {
		content.WriteString(strings.Repeat("x", 100))
		content.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(root, "dep.go"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"current_file_path": "main.go", "change_files": "", "system_rule": "review correctness",
		"diff": "@@ -1 +1 @@\n-old\n+new", "language": "Chinese", "plan_guidance": "none",
	}
	initialMessages, err := loadPromptMessages("main", values)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := loadToolDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	initialTokens := estimateRequestTokens(protocol.Request{Messages: initialMessages, Tools: definitions})
	cfg := config.Default()
	cfg.Review.MaxChunkTokens = initialTokens * 2
	llm := &recordingLLM{responses: []protocol.Response{
		toolResponse("read", "file_read", `{"file_path":"dep.go","start_line":1,"end_line":500}`),
		{Text: "The dependency file was read; no relevant validation was found.", Usage: agent.TokenUsage{PromptTokens: 900, CompletionTokens: 20, TotalTokens: 920}},
		doneResponse(),
	}}
	r := Runner{
		Root: root, Config: cfg, LLM: llm, process: newProcessRecorder(time.Now()),
		DiffRequest: gitdiff.Request{Mode: gitdiff.ModeWorkspace},
	}
	var usage agent.TokenUsage
	findings, warnings, err := r.runMainLoop(context.Background(), gitdiff.FileDiff{Path: "main.go"}, values, 100, &usage)
	if err != nil || len(findings) != 0 || len(warnings) != 0 {
		t.Fatalf("unexpected loop result: findings=%+v warnings=%+v err=%v", findings, warnings, err)
	}
	if len(llm.requests) != 3 || len(llm.requests[1].Tools) != 0 || !requestContains(llm.requests[1], "<conversation_history>") {
		t.Fatalf("compression request was not inserted before the next main request: %+v", llm.requests)
	}
	if !requestContains(llm.requests[2], "<previous_review_summary>") || len(llm.requests[2].Tools) == 0 {
		t.Fatalf("main loop did not resume with compressed context: %+v", llm.requests[2])
	}
	process := r.process.snapshot()
	if len(process.Compressions) != 1 || process.Compressions[0].Status != "success" || usage.LLMRequests != 3 {
		t.Fatalf("compression process or usage missing: process=%+v usage=%+v", process, usage)
	}
}

func compressionTestMessages(oldResult string) []protocol.Message {
	return []protocol.Message{
		{Role: protocol.RoleSystem, Content: "system"},
		{Role: protocol.RoleUser, Content: "review diff"},
		{Role: protocol.RoleAssistant, ToolCalls: []protocol.ToolCall{{ID: "a", Name: "file_read", Arguments: `{"file_path":"dep.go"}`}}},
		{Role: protocol.RoleTool, ToolCallID: "a", ToolName: "file_read", Content: oldResult},
		{Role: protocol.RoleAssistant, ToolCalls: []protocol.ToolCall{{ID: "b", Name: "code_search", Arguments: `{"search_text":"Caller"}`}}},
		{Role: protocol.RoleTool, ToolCallID: "b", ToolName: "code_search", Content: "three matches"},
		{Role: protocol.RoleAssistant, ToolCalls: []protocol.ToolCall{{ID: "c", Name: "file_read", Arguments: `{"file_path":"caller.go"}`}}},
		{Role: protocol.RoleTool, ToolCallID: "c", ToolName: "file_read", Content: "validated = true"},
	}
}

type compressionErrorLLM struct{}

func (compressionErrorLLM) Complete(context.Context, protocol.Request) (protocol.Response, error) {
	return protocol.Response{}, errors.New("compression unavailable")
}
