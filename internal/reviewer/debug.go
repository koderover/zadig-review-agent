package reviewer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/protocol"
)

const llmDebugSchemaVersion = "zadig-review-agent.llm-debug/v1"

// LLMDebugRecorder records normalized LLM requests and responses for debug output.
// It is safe to share across concurrent file-review workers.
type LLMDebugRecorder struct {
	mu      sync.Mutex
	started time.Time
	nextID  int
	events  []llmDebugEvent
}

type llmDebugMeta struct {
	stage    string
	file     string
	sequence int
}

type pendingLLMDebugRequest struct {
	id               string
	stage            string
	file             string
	sequence         int
	transportAttempt int
	startedAt        time.Time
}

type llmDebugMessage struct {
	Role       protocol.MessageRole `json:"role"`
	Content    string               `json:"content,omitempty"`
	ToolCalls  []protocol.ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolName   string               `json:"tool_name,omitempty"`
}

type llmDebugRequestPayload struct {
	Messages    []llmDebugMessage         `json:"messages"`
	Tools       []protocol.ToolDefinition `json:"tools,omitempty"`
	RequireTool bool                      `json:"require_tool"`
}

type llmDebugResponsePayload struct {
	Text         string              `json:"text,omitempty"`
	ToolCalls    []protocol.ToolCall `json:"tool_calls,omitempty"`
	Usage        agent.TokenUsage    `json:"usage"`
	FinishReason string              `json:"finish_reason,omitempty"`
}

type llmDebugEvent struct {
	SchemaVersion    string                   `json:"schema_version"`
	Type             string                   `json:"type"`
	ID               string                   `json:"id"`
	Timestamp        string                   `json:"timestamp"`
	StartedOffsetMS  int64                    `json:"started_offset_ms"`
	DurationMS       int64                    `json:"duration_ms,omitempty"`
	Stage            string                   `json:"stage,omitempty"`
	File             string                   `json:"file,omitempty"`
	Sequence         int                      `json:"sequence,omitempty"`
	TransportAttempt int                      `json:"transport_attempt"`
	Protocol         string                   `json:"protocol"`
	Model            string                   `json:"model"`
	EstimatedTokens  int                      `json:"estimated_tokens,omitempty"`
	Status           string                   `json:"status,omitempty"`
	Request          *llmDebugRequestPayload  `json:"request,omitempty"`
	Response         *llmDebugResponsePayload `json:"response,omitempty"`
	Error            string                   `json:"error,omitempty"`
}

func NewLLMDebugRecorder() *LLMDebugRecorder {
	return &LLMDebugRecorder{}
}

func (r *LLMDebugRecorder) start(started time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.started.IsZero() {
		r.started = started
	}
	r.mu.Unlock()
}

func (r *LLMDebugRecorder) begin(meta llmDebugMeta, transportAttempt int, protocolName, model string, request protocol.Request) (pendingLLMDebugRequest, error) {
	if r == nil {
		return pendingLLMDebugRequest{}, nil
	}
	now := time.Now()
	messages := make([]llmDebugMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, llmDebugMessage{
			Role: message.Role, Content: message.Content, ToolCalls: append([]protocol.ToolCall(nil), message.ToolCalls...),
			ToolCallID: message.ToolCallID, ToolName: message.ToolName,
		})
	}
	payload := &llmDebugRequestPayload{
		Messages: messages, Tools: append([]protocol.ToolDefinition(nil), request.Tools...), RequireTool: request.RequireTool,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started.IsZero() {
		r.started = now
	}
	r.nextID++
	pending := pendingLLMDebugRequest{
		id: fmt.Sprintf("llm-%04d", r.nextID), stage: meta.stage, file: meta.file, sequence: meta.sequence,
		transportAttempt: transportAttempt, startedAt: now,
	}
	event := llmDebugEvent{
		SchemaVersion: llmDebugSchemaVersion, Type: "request", ID: pending.id,
		Timestamp: now.UTC().Format(time.RFC3339Nano), StartedOffsetMS: elapsedMilliseconds(now.Sub(r.started)),
		Stage: meta.stage, File: meta.file, Sequence: meta.sequence, TransportAttempt: transportAttempt,
		Protocol: protocolName, Model: model, EstimatedTokens: estimateRequestTokens(request), Request: payload,
	}
	if err := r.appendLocked(event); err != nil {
		return pendingLLMDebugRequest{}, err
	}
	return pending, nil
}

func (r *LLMDebugRecorder) finish(pending pendingLLMDebugRequest, protocolName, model string, response protocol.Response, requestErr error) error {
	if r == nil || pending.id == "" {
		return nil
	}
	now := time.Now()
	status := "success"
	errorText := ""
	if requestErr != nil {
		status = "error"
		errorText = requestErr.Error()
	}
	event := llmDebugEvent{
		SchemaVersion: llmDebugSchemaVersion, Type: "response", ID: pending.id,
		Timestamp: now.UTC().Format(time.RFC3339Nano), StartedOffsetMS: elapsedMilliseconds(pending.startedAt.Sub(r.started)),
		DurationMS: elapsedMilliseconds(now.Sub(pending.startedAt)), Stage: pending.stage, File: pending.file,
		Sequence: pending.sequence, TransportAttempt: pending.transportAttempt, Protocol: protocolName, Model: model,
		Status: status, Error: errorText,
		Response: &llmDebugResponsePayload{
			Text: response.Text, ToolCalls: append([]protocol.ToolCall(nil), response.ToolCalls...),
			Usage: response.Usage, FinishReason: response.FinishReason,
		},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendLocked(event)
}

func (r *LLMDebugRecorder) appendLocked(event llmDebugEvent) error {
	if _, err := json.Marshal(event); err != nil {
		return fmt.Errorf("marshal LLM debug event: %w", err)
	}
	r.events = append(r.events, event)
	return nil
}

// WriteFile writes a human-readable Markdown log by default. A .jsonl suffix
// selects the compact machine-readable event stream.
func (r *LLMDebugRecorder) WriteFile(path string) error {
	if r == nil || path == "" {
		return nil
	}
	r.mu.Lock()
	var data []byte
	var err error
	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		data, err = renderLLMDebugJSONL(r.events)
	} else {
		data, err = renderLLMDebugMarkdown(r.events)
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create LLM debug directory: %w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write LLM debug log: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict LLM debug log permissions: %w", err)
	}
	return nil
}

func renderLLMDebugJSONL(events []llmDebugEvent) ([]byte, error) {
	var b bytes.Buffer
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("marshal LLM debug event: %w", err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

func renderLLMDebugMarkdown(events []llmDebugEvent) ([]byte, error) {
	requests := make([]llmDebugEvent, 0, len(events)/2)
	responses := make(map[string]llmDebugEvent, len(events)/2)
	for _, event := range events {
		switch event.Type {
		case "request":
			requests = append(requests, event)
		case "response":
			responses[event.ID] = event
		}
	}

	var b bytes.Buffer
	b.WriteString("# LLM Debug Log\n\n")
	b.WriteString("> This file may contain complete prompts, source diffs, repository context, tool results, and model output. Treat it as sensitive source code.\n\n")
	fmt.Fprintf(&b, "- Schema: `%s`\n", llmDebugSchemaVersion)
	fmt.Fprintf(&b, "- Calls: %d\n\n", len(requests))
	if len(requests) == 0 {
		b.WriteString("_No LLM requests were made._\n")
		return b.Bytes(), nil
	}

	for _, requestEvent := range requests {
		responseEvent, hasResponse := responses[requestEvent.ID]
		fmt.Fprintf(&b, "## Call %s\n\n", requestEvent.ID)
		b.WriteString("| Field | Value |\n| --- | --- |\n")
		writeDebugTableRow(&b, "Stage", requestEvent.Stage)
		writeDebugTableRow(&b, "File", requestEvent.File)
		writeDebugTableRow(&b, "Sequence", fmt.Sprint(requestEvent.Sequence))
		writeDebugTableRow(&b, "Transport attempt", fmt.Sprint(requestEvent.TransportAttempt))
		writeDebugTableRow(&b, "Protocol", requestEvent.Protocol)
		writeDebugTableRow(&b, "Model", requestEvent.Model)
		writeDebugTableRow(&b, "Started", requestEvent.Timestamp)
		writeDebugTableRow(&b, "Started offset", fmt.Sprintf("%d ms", requestEvent.StartedOffsetMS))
		writeDebugTableRow(&b, "Estimated request tokens", fmt.Sprint(requestEvent.EstimatedTokens))
		if hasResponse {
			writeDebugTableRow(&b, "Status", responseEvent.Status)
			writeDebugTableRow(&b, "Duration", fmt.Sprintf("%d ms", responseEvent.DurationMS))
		} else {
			writeDebugTableRow(&b, "Status", "response not captured")
		}
		b.WriteString("\n### Request\n\n")
		writeDebugRequestMarkdown(&b, requestEvent.Request)
		b.WriteString("\n### Response\n\n")
		if hasResponse {
			writeDebugResponseMarkdown(&b, responseEvent)
		} else {
			b.WriteString("_No response was captured._\n")
		}
		b.WriteString("\n---\n\n")
	}
	return b.Bytes(), nil
}

func writeDebugRequestMarkdown(b *bytes.Buffer, request *llmDebugRequestPayload) {
	if request == nil {
		b.WriteString("_Request payload is unavailable._\n")
		return
	}
	fmt.Fprintf(b, "- Require tool call: `%t`\n", request.RequireTool)
	fmt.Fprintf(b, "- Messages: %d\n", len(request.Messages))
	fmt.Fprintf(b, "- Available tools: %d\n\n", len(request.Tools))
	for index, message := range request.Messages {
		fmt.Fprintf(b, "#### Message %d · %s\n\n", index+1, message.Role)
		if message.ToolName != "" {
			fmt.Fprintf(b, "- Tool name: `%s`\n", escapeMarkdownCode(message.ToolName))
		}
		if message.ToolCallID != "" {
			fmt.Fprintf(b, "- Tool call ID: `%s`\n", escapeMarkdownCode(message.ToolCallID))
		}
		if message.ToolName != "" || message.ToolCallID != "" {
			b.WriteByte('\n')
		}
		if message.Content == "" {
			b.WriteString("_No text content._\n")
		} else {
			writeMarkdownFence(b, "text", message.Content)
		}
		if len(message.ToolCalls) > 0 {
			b.WriteString("\n**Tool calls**\n\n")
			writeDebugJSONBlock(b, message.ToolCalls)
		}
		b.WriteByte('\n')
	}
	if len(request.Tools) > 0 {
		b.WriteString("#### Tool definitions\n\n")
		writeDebugJSONBlock(b, request.Tools)
	}
}

func writeDebugResponseMarkdown(b *bytes.Buffer, event llmDebugEvent) {
	if event.Error != "" {
		b.WriteString("#### Error\n\n")
		writeMarkdownFence(b, "text", event.Error)
		b.WriteByte('\n')
	}
	if event.Response == nil {
		b.WriteString("_Response payload is unavailable._\n")
		return
	}
	response := event.Response
	b.WriteString("| Usage | Tokens |\n| --- | ---: |\n")
	writeDebugTableRow(b, "Prompt", fmt.Sprint(response.Usage.PromptTokens))
	writeDebugTableRow(b, "Completion", fmt.Sprint(response.Usage.CompletionTokens))
	writeDebugTableRow(b, "Total", fmt.Sprint(response.Usage.TotalTokens))
	if response.FinishReason != "" {
		fmt.Fprintf(b, "\n- Finish reason: `%s`\n", escapeMarkdownCode(response.FinishReason))
	}
	if response.Text != "" {
		b.WriteString("\n#### Text\n\n")
		writeMarkdownFence(b, "text", response.Text)
	}
	if len(response.ToolCalls) > 0 {
		b.WriteString("\n#### Tool calls\n\n")
		writeDebugJSONBlock(b, response.ToolCalls)
	}
	if response.Text == "" && len(response.ToolCalls) == 0 && event.Error == "" {
		b.WriteString("\n_No text or tool calls returned._\n")
	}
}

func writeDebugJSONBlock(b *bytes.Buffer, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		writeMarkdownFence(b, "text", "Unable to render JSON: "+err.Error())
		return
	}
	writeMarkdownFence(b, "json", string(data))
}

func writeMarkdownFence(b *bytes.Buffer, language, value string) {
	fence := "```"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	fmt.Fprintf(b, "%s%s\n%s", fence, language, value)
	if !strings.HasSuffix(value, "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "%s\n", fence)
}

func writeDebugTableRow(b *bytes.Buffer, field, value string) {
	if value == "" {
		value = "—"
	}
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\n", "<br>")
	fmt.Fprintf(b, "| %s | %s |\n", field, value)
}

func escapeMarkdownCode(value string) string {
	return strings.ReplaceAll(value, "`", "\\`")
}
