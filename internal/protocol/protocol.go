package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	openai "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/config"
)

const modelAPIKeyEnv = "ZADIG_REVIEW_MODEL_API_KEY"

type Request struct {
	Messages []Message
	Tools    []ToolDefinition
	// RequireTool is used after a model returns text instead of a tool call.
	RequireTool bool
}

type Response struct {
	Text         string
	ToolCalls    []ToolCall
	Usage        agent.TokenUsage
	FinishReason string
}

type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

type Message struct {
	Role       MessageRole
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	ToolName   string
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// HTTPError remains useful to callers that provide transport adapters and tests.
type HTTPError struct {
	Protocol   string
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s returned %s: %s", e.Protocol, e.Status, e.Body)
}

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if status, ok := apiStatusCode(err); ok {
		return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

func apiStatusCode(err error) (int, bool) {
	var local *HTTPError
	if errors.As(err, &local) {
		return local.StatusCode, true
	}
	var openAIErr *openai.Error
	if errors.As(err, &openAIErr) {
		return openAIErr.StatusCode, true
	}
	var anthropicErr *anthropic.Error
	if errors.As(err, &anthropicErr) {
		return anthropicErr.StatusCode, true
	}
	var geminiErr genai.APIError
	if errors.As(err, &geminiErr) {
		return geminiErr.Code, true
	}
	return 0, false
}

type LLM interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

type Registry struct{}

func NewRegistry() Registry {
	return Registry{}
}

func (Registry) Build(cfg config.ModelConfig) (LLM, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("model.name is required")
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("model.endpoint is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	switch cfg.Protocol {
	case "openai":
		return newOpenAI(cfg), nil
	case "gemini":
		return newGemini(cfg)
	case "anthropic":
		return newAnthropic(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported model protocol %q", cfg.Protocol)
	}
}

var newHTTPClient = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

type openAILLM struct {
	client openai.Client
	model  string
}

func newOpenAI(cfg config.ModelConfig) LLM {
	client := openai.NewClient(
		openaioption.WithAPIKey(apiKey(cfg)),
		openaioption.WithBaseURL(cfg.Endpoint),
		openaioption.WithHTTPClient(newHTTPClient(cfg.Timeout)),
		openaioption.WithMaxRetries(0),
	)
	return &openAILLM{client: client, model: cfg.Name}
}

func (l *openAILLM) Complete(ctx context.Context, req Request) (Response, error) {
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages))
	for _, message := range req.Messages {
		switch message.Role {
		case RoleSystem:
			messages = append(messages, openai.SystemMessage(message.Content))
		case RoleUser:
			messages = append(messages, openai.UserMessage(message.Content))
		case RoleAssistant:
			assistant := openai.AssistantMessage(message.Content)
			for _, call := range message.ToolCalls {
				assistant.OfAssistant.ToolCalls = append(assistant.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: call.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name: call.Name, Arguments: call.Arguments,
						},
					},
				})
			}
			messages = append(messages, assistant)
		case RoleTool:
			messages = append(messages, openai.ToolMessage(message.Content, message.ToolCallID))
		default:
			return Response{}, fmt.Errorf("unsupported message role %q", message.Role)
		}
	}
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(req.Tools))
	for _, tool := range req.Tools {
		tools = append(tools, openai.ChatCompletionToolUnionParam{OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name: tool.Name, Description: openai.String(tool.Description), Parameters: tool.Parameters,
			},
		}})
	}
	params := openai.ChatCompletionNewParams{
		Model:       openai.ChatModel(l.model),
		Messages:    messages,
		Tools:       tools,
		Temperature: openai.Float(0),
	}
	if req.RequireTool && len(tools) > 0 {
		params.ToolChoice.OfAuto = openai.String(string(openai.ChatCompletionToolChoiceOptionAutoRequired))
	}
	result, err := l.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, err
	}
	usage := agent.TokenUsage{
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
		TotalTokens:      result.Usage.TotalTokens,
		CacheReadTokens:  result.Usage.PromptTokensDetails.CachedTokens,
		CacheWriteTokens: result.Usage.PromptTokensDetails.CacheWriteTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	response := Response{Usage: usage}
	if len(result.Choices) == 0 {
		return response, fmt.Errorf("openai response has no choices")
	}
	response.Text = result.Choices[0].Message.Content
	response.FinishReason = string(result.Choices[0].FinishReason)
	for _, call := range result.Choices[0].Message.ToolCalls {
		if call.Type != "function" {
			continue
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
		})
	}
	return response, nil
}

type geminiLLM struct {
	client *genai.Client
	model  string
}

func newGemini(cfg config.ModelConfig) (LLM, error) {
	endpoint := cfg.Endpoint
	if endpoint == "https://api.openai.com/v1" {
		endpoint = "https://generativelanguage.googleapis.com/v1beta"
	}
	httpOptions, err := geminiHTTPOptions(endpoint)
	if err != nil {
		return nil, err
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:      apiKey(cfg),
		Backend:     genai.BackendGeminiAPI,
		HTTPClient:  newHTTPClient(cfg.Timeout),
		HTTPOptions: httpOptions,
	})
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return &geminiLLM{client: client, model: cfg.Name}, nil
}

func geminiHTTPOptions(endpoint string) (genai.HTTPOptions, error) {
	parsed, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return genai.HTTPOptions{}, fmt.Errorf("invalid Gemini endpoint %q", endpoint)
	}
	options := genai.HTTPOptions{}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) > 0 && strings.HasPrefix(segments[len(segments)-1], "v1") {
		options.APIVersion = segments[len(segments)-1]
		segments = segments[:len(segments)-1]
	}
	parsed.Path = strings.Join(segments, "/")
	options.BaseURL = strings.TrimRight(parsed.String(), "/")
	return options, nil
}

func (l *geminiLLM) Complete(ctx context.Context, req Request) (Response, error) {
	temperature := float32(0)
	config := &genai.GenerateContentConfig{Temperature: &temperature}
	contents, system, err := geminiMessages(req.Messages)
	if err != nil {
		return Response{}, err
	}
	if system != "" {
		config.SystemInstruction = genai.NewContentFromText(system, genai.RoleUser)
	}
	if len(req.Tools) > 0 {
		declarations := make([]*genai.FunctionDeclaration, 0, len(req.Tools))
		for _, tool := range req.Tools {
			declarations = append(declarations, &genai.FunctionDeclaration{
				Name: tool.Name, Description: tool.Description, ParametersJsonSchema: tool.Parameters,
			})
		}
		config.Tools = []*genai.Tool{{FunctionDeclarations: declarations}}
		if req.RequireTool {
			config.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}
		}
	}
	result, err := l.client.Models.GenerateContent(ctx, l.model, contents, config)
	if err != nil {
		return Response{}, err
	}
	response := Response{}
	if result.UsageMetadata != nil {
		prompt := int64(result.UsageMetadata.PromptTokenCount)
		total := int64(result.UsageMetadata.TotalTokenCount)
		completion := total - prompt
		if completion < 0 || total == 0 {
			completion = int64(result.UsageMetadata.CandidatesTokenCount + result.UsageMetadata.ThoughtsTokenCount)
		}
		if total == 0 || total < prompt {
			total = prompt + completion
		}
		response.Usage = agent.TokenUsage{
			PromptTokens:     prompt,
			CompletionTokens: completion,
			TotalTokens:      total,
			CacheReadTokens:  int64(result.UsageMetadata.CachedContentTokenCount),
		}
	}
	response.Text = result.Text()
	if len(result.Candidates) > 0 {
		response.FinishReason = string(result.Candidates[0].FinishReason)
	}
	for index, call := range result.FunctionCalls() {
		arguments, marshalErr := json.Marshal(call.Args)
		if marshalErr != nil {
			return response, fmt.Errorf("encode Gemini tool arguments: %w", marshalErr)
		}
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("gemini-call-%d", index+1)
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{ID: id, Name: call.Name, Arguments: string(arguments)})
	}
	if response.Text == "" && len(response.ToolCalls) == 0 {
		return response, fmt.Errorf("gemini response has no text or tool call content")
	}
	return response, nil
}

func geminiMessages(messages []Message) ([]*genai.Content, string, error) {
	var contents []*genai.Content
	var systems []string
	for _, message := range messages {
		switch message.Role {
		case RoleSystem:
			systems = append(systems, message.Content)
		case RoleUser:
			contents = append(contents, genai.NewContentFromText(message.Content, genai.RoleUser))
		case RoleAssistant:
			parts := make([]*genai.Part, 0, len(message.ToolCalls)+1)
			if message.Content != "" {
				parts = append(parts, genai.NewPartFromText(message.Content))
			}
			for _, call := range message.ToolCalls {
				var args map[string]any
				if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
					return nil, "", fmt.Errorf("decode Gemini tool arguments: %w", err)
				}
				part := genai.NewPartFromFunctionCall(call.Name, args)
				part.FunctionCall.ID = call.ID
				parts = append(parts, part)
			}
			contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
		case RoleTool:
			part := genai.NewPartFromFunctionResponse(message.ToolName, map[string]any{"result": message.Content})
			part.FunctionResponse.ID = message.ToolCallID
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{part}, genai.RoleUser))
		default:
			return nil, "", fmt.Errorf("unsupported message role %q", message.Role)
		}
	}
	return contents, strings.Join(systems, "\n\n"), nil
}

type anthropicLLM struct {
	client anthropic.Client
	model  string
}

func newAnthropic(cfg config.ModelConfig) LLM {
	endpoint := cfg.Endpoint
	if endpoint == "https://api.openai.com/v1" {
		endpoint = "https://api.anthropic.com"
	}
	client := anthropic.NewClient(
		anthropicoption.WithAPIKey(apiKey(cfg)),
		anthropicoption.WithBaseURL(endpoint),
		anthropicoption.WithHTTPClient(newHTTPClient(cfg.Timeout)),
		anthropicoption.WithMaxRetries(0),
	)
	return &anthropicLLM{client: client, model: cfg.Name}
}

func (l *anthropicLLM) Complete(ctx context.Context, req Request) (Response, error) {
	messages, system, err := anthropicMessages(req.Messages)
	if err != nil {
		return Response{}, err
	}
	params := anthropic.MessageNewParams{
		Model:       anthropic.Model(l.model),
		MaxTokens:   4096,
		Temperature: anthropic.Float(0),
		Messages:    messages,
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}
	for _, tool := range req.Tools {
		var schema anthropic.ToolInputSchemaParam
		encoded, marshalErr := json.Marshal(tool.Parameters)
		if marshalErr != nil {
			return Response{}, fmt.Errorf("encode Anthropic tool schema: %w", marshalErr)
		}
		if unmarshalErr := json.Unmarshal(encoded, &schema); unmarshalErr != nil {
			return Response{}, fmt.Errorf("decode Anthropic tool schema: %w", unmarshalErr)
		}
		param := anthropic.ToolUnionParamOfTool(schema, tool.Name)
		param.OfTool.Description = anthropic.String(tool.Description)
		params.Tools = append(params.Tools, param)
	}
	if req.RequireTool && len(params.Tools) > 0 {
		params.ToolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	}
	result, err := l.client.Messages.New(ctx, params)
	if err != nil {
		return Response{}, err
	}
	prompt := result.Usage.InputTokens + result.Usage.CacheCreationInputTokens + result.Usage.CacheReadInputTokens
	response := Response{Usage: agent.TokenUsage{
		PromptTokens:     prompt,
		CompletionTokens: result.Usage.OutputTokens,
		TotalTokens:      prompt + result.Usage.OutputTokens,
		CacheReadTokens:  result.Usage.CacheReadInputTokens,
		CacheWriteTokens: result.Usage.CacheCreationInputTokens,
	}}
	response.FinishReason = string(result.StopReason)
	var parts []string
	for _, block := range result.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		} else if block.Type == "tool_use" {
			response.ToolCalls = append(response.ToolCalls, ToolCall{
				ID: block.ID, Name: block.Name, Arguments: string(block.Input),
			})
		}
	}
	response.Text = strings.Join(parts, "\n")
	if response.Text == "" && len(response.ToolCalls) == 0 {
		return response, fmt.Errorf("anthropic response has no text or tool call content")
	}
	return response, nil
}

func anthropicMessages(messages []Message) ([]anthropic.MessageParam, string, error) {
	var result []anthropic.MessageParam
	var systems []string
	for _, message := range messages {
		switch message.Role {
		case RoleSystem:
			systems = append(systems, message.Content)
		case RoleUser:
			result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(message.Content)))
		case RoleAssistant:
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(message.ToolCalls)+1)
			if message.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(message.Content))
			}
			for _, call := range message.ToolCalls {
				var input any
				if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
					return nil, "", fmt.Errorf("decode Anthropic tool arguments: %w", err)
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, input, call.Name))
			}
			result = append(result, anthropic.NewAssistantMessage(blocks...))
		case RoleTool:
			result = append(result, anthropic.NewUserMessage(anthropic.NewToolResultBlock(message.ToolCallID, message.Content, false)))
		default:
			return nil, "", fmt.Errorf("unsupported message role %q", message.Role)
		}
	}
	return result, strings.Join(systems, "\n\n"), nil
}

func apiKey(cfg config.ModelConfig) string {
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	return os.Getenv(modelAPIKeyEnv)
}
