package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/openai/openai-go/v3"
	"google.golang.org/genai"

	"github.com/koderover/zadig-code-review-agent/internal/config"
)

func TestOpenAIProtocol(t *testing.T) {
	oldClient := newHTTPClient
	defer func() { newHTTPClient = oldClient }()
	newHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/chat/completions" {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
				t.Fatalf("unexpected authorization header %q", got)
			}
			requestBody, _ := io.ReadAll(r.Body)
			for _, want := range []string{`"role":"system"`, `"role":"assistant"`, `"tool_calls"`, `"role":"tool"`, `"tools"`, `"tool_choice":"required"`} {
				if !bytes.Contains(requestBody, []byte(want)) {
					t.Fatalf("OpenAI request missing %s: %s", want, requestBody)
				}
			}
			var b bytes.Buffer
			_ = json.NewEncoder(&b).Encode(map[string]any{
				"choices": []any{
					map[string]any{"message": map[string]any{
						"role": "assistant", "content": "",
						"tool_calls": []any{map[string]any{
							"id": "next", "type": "function",
							"function": map[string]string{"name": "task_done", "arguments": `{}`},
						}},
					}},
				},
				"usage": map[string]any{
					"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14,
					"prompt_tokens_details": map[string]any{"cached_tokens": 6, "cache_write_tokens": 2},
				},
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(&b),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		})}
	}
	llm, err := NewRegistry().Build(config.ModelConfig{
		Protocol: "openai",
		Name:     "test",
		Endpoint: "https://example.test",
		APIKey:   "test-key",
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := llm.Complete(context.Background(), toolTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "task_done" || got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 4 || got.Usage.TotalTokens != 14 || got.Usage.CacheReadTokens != 6 || got.Usage.CacheWriteTokens != 2 {
		t.Fatalf("unexpected response %+v", got)
	}
}

func TestGeminiProtocolUsage(t *testing.T) {
	oldClient := newHTTPClient
	defer func() { newHTTPClient = oldClient }()
	newHTTPClient = responseClientInspect(t, "/v1beta/models/gemini-test:generateContent", map[string]any{
		"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"id": "next", "name": "task_done", "args": map[string]any{}}}}}}},
		"usageMetadata": map[string]any{
			"promptTokenCount": 20, "candidatesTokenCount": 5, "thoughtsTokenCount": 7,
			"totalTokenCount": 32, "cachedContentTokenCount": 8,
		},
	}, func(body []byte) {
		for _, want := range []string{`"systemInstruction"`, `"functionCall"`, `"functionResponse"`, `"tools"`, `"functionCallingConfig":{"mode":"ANY"`} {
			if !bytes.Contains(body, []byte(want)) {
				t.Fatalf("Gemini request missing %s: %s", want, body)
			}
		}
	})
	llm, err := NewRegistry().Build(config.ModelConfig{Protocol: "gemini", Name: "gemini-test", Endpoint: "https://example.test", APIKey: "test-key", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := llm.Complete(context.Background(), toolTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "task_done" || got.Usage.PromptTokens != 20 || got.Usage.CompletionTokens != 12 || got.Usage.TotalTokens != 32 || got.Usage.CacheReadTokens != 8 || got.Usage.CacheWriteTokens != 0 {
		t.Fatalf("unexpected response %+v", got)
	}
}

func TestAnthropicProtocolUsage(t *testing.T) {
	oldClient := newHTTPClient
	defer func() { newHTTPClient = oldClient }()
	newHTTPClient = responseClientInspect(t, "/v1/messages", map[string]any{
		"content": []any{map[string]any{"type": "tool_use", "id": "next", "name": "task_done", "input": map[string]any{}}},
		"usage": map[string]any{
			"input_tokens": 10, "output_tokens": 4,
			"cache_creation_input_tokens": 6, "cache_read_input_tokens": 7,
		},
	}, func(body []byte) {
		for _, want := range []string{`"system"`, `"tool_use"`, `"tool_result"`, `"tools"`, `"tool_choice":{"type":"any"`} {
			if !bytes.Contains(body, []byte(want)) {
				t.Fatalf("Anthropic request missing %s: %s", want, body)
			}
		}
	})
	llm, err := NewRegistry().Build(config.ModelConfig{Protocol: "anthropic", Name: "claude-test", Endpoint: "https://example.test", APIKey: "test-key", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got, err := llm.Complete(context.Background(), toolTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "task_done" || got.Usage.PromptTokens != 23 || got.Usage.CompletionTokens != 4 || got.Usage.TotalTokens != 27 || got.Usage.CacheReadTokens != 7 || got.Usage.CacheWriteTokens != 6 {
		t.Fatalf("unexpected response %+v", got)
	}
}

func TestCustomProtocolUnsupported(t *testing.T) {
	_, err := NewRegistry().Build(config.ModelConfig{Protocol: "custom", Name: "fake"})
	if err == nil {
		t.Fatal("expected custom protocol to be unsupported")
	}
}

func TestRetryableErrors(t *testing.T) {
	for _, err := range []error{
		&HTTPError{StatusCode: http.StatusTooManyRequests},
		&HTTPError{StatusCode: http.StatusBadGateway},
		&openai.Error{StatusCode: http.StatusTooManyRequests},
		&anthropic.Error{StatusCode: http.StatusBadGateway},
		genai.APIError{Code: http.StatusServiceUnavailable},
		testNetError{},
	} {
		if !IsRetryable(err) {
			t.Fatalf("expected retryable error: %v", err)
		}
	}
	if IsRetryable(&HTTPError{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("authentication errors must not be retried")
	}
	if IsRetryable(genai.APIError{Code: http.StatusBadRequest}) {
		t.Fatal("SDK parameter errors must not be retried")
	}
}

type testNetError struct{}

func (testNetError) Error() string   { return "timeout" }
func (testNetError) Timeout() bool   { return true }
func (testNetError) Temporary() bool { return true }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func responseClient(t *testing.T, path string, payload any) func(time.Duration) *http.Client {
	return responseClientInspect(t, path, payload, nil)
}

func responseClientInspect(t *testing.T, path string, payload any, inspect func([]byte)) func(time.Duration) *http.Client {
	t.Helper()
	return func(time.Duration) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != path {
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
			if inspect != nil {
				body, _ := io.ReadAll(r.Body)
				inspect(body)
			}
			var b bytes.Buffer
			if err := json.NewEncoder(&b).Encode(payload); err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(&b), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
		})}
	}
}

func toolTestRequest() Request {
	return Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "system"},
			{Role: RoleUser, Content: "review"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "file_read", Arguments: `{"file_path":"main.go"}`}}},
			{Role: RoleTool, ToolCallID: "call-1", ToolName: "file_read", Content: "file content"},
		},
		Tools:       []ToolDefinition{{Name: "task_done", Description: "done", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
		RequireTool: true,
	}
}
