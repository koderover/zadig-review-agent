package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/protocol"
)

const (
	compressionThreshold  = 0.60
	compressionKeepRounds = 2
)

var errCompressionIneffective = errors.New("compressed summary did not reduce context")

type compressionPartition struct {
	compressEnd int
	activeStart int
}

type compressionMessage struct {
	Role       protocol.MessageRole `json:"role"`
	Content    string               `json:"content,omitempty"`
	ToolCalls  []protocol.ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolName   string               `json:"tool_name,omitempty"`
}

func partitionForCompression(messages []protocol.Message, keepRounds int) (compressionPartition, bool) {
	if len(messages) <= 2 || keepRounds < 0 {
		return compressionPartition{}, false
	}
	var assistantIndexes []int
	for index := 2; index < len(messages); index++ {
		if messages[index].Role == protocol.RoleAssistant {
			assistantIndexes = append(assistantIndexes, index)
		}
	}
	if len(assistantIndexes) == 0 || len(assistantIndexes) <= keepRounds {
		return compressionPartition{}, false
	}
	activeStart := len(messages)
	if keepRounds > 0 {
		activeStart = assistantIndexes[len(assistantIndexes)-keepRounds]
	}
	if activeStart <= 2 {
		return compressionPartition{}, false
	}
	return compressionPartition{compressEnd: activeStart, activeStart: activeStart}, true
}

func (r Runner) maybeCompressMessages(ctx context.Context, file string, round int, messages []protocol.Message, tools []protocol.ToolDefinition, requireTool bool, usage *agent.TokenUsage) ([]protocol.Message, string) {
	beforeTokens := estimateMessagesTokens(messages)
	requestTokens := estimateRequestTokens(protocol.Request{Messages: messages, Tools: tools, RequireTool: requireTool})
	if requestTokens < int(float64(r.Config.Review.MaxChunkTokens)*compressionThreshold) {
		return messages, ""
	}
	partition, ok := partitionForCompression(messages, compressionKeepRounds)
	if !ok && requestTokens >= r.Config.Review.MaxChunkTokens*4/5 {
		// A small number of unusually large tool results can cross the hard
		// threshold before there are two older rounds. Summarize all completed
		// rounds rather than failing without attempting recovery.
		partition, ok = partitionForCompression(messages, 0)
	}
	if !ok {
		return messages, ""
	}

	compressed := messages[2:partition.compressEnd]
	preserved := messages[partition.activeStart:]
	pending := r.process.beginCompression(file, round, beforeTokens, len(compressed), len(preserved))
	serialized := make([]compressionMessage, 0, len(compressed))
	for _, message := range compressed {
		serialized = append(serialized, compressionMessage{
			Role: message.Role, Content: message.Content, ToolCalls: message.ToolCalls,
			ToolCallID: message.ToolCallID, ToolName: message.ToolName,
		})
	}
	payload, err := json.Marshal(serialized)
	if err != nil {
		r.process.finishCompression(pending, "failed", beforeTokens, agent.TokenUsage{}, err)
		return messages, "memory_compression_failed: " + file + ": " + err.Error()
	}
	compressionMessages, err := loadPromptMessages("memory_compression", map[string]string{"conversation": string(payload)})
	if err != nil {
		r.process.finishCompression(pending, "failed", beforeTokens, agent.TokenUsage{}, err)
		return messages, "memory_compression_failed: " + file + ": " + err.Error()
	}

	usageBefore := *usage
	response, err := r.completeAuditedWithoutThreshold(ctx, "memory_compression", file, 1, protocol.Request{Messages: compressionMessages}, usage)
	compressionUsage := usageDifference(*usage, usageBefore)
	if err != nil {
		r.process.finishCompression(pending, "failed", beforeTokens, compressionUsage, err)
		return messages, "memory_compression_failed: " + file + ": " + err.Error()
	}
	summary := strings.TrimSpace(response.Text)
	if summary == "" {
		err = errors.New("model returned an empty summary")
		r.process.finishCompression(pending, "failed", beforeTokens, compressionUsage, err)
		return messages, "memory_compression_failed: " + file + ": " + err.Error()
	}

	rebuilt := make([]protocol.Message, 0, 3+len(preserved))
	rebuilt = append(rebuilt, messages[:2]...)
	rebuilt = append(rebuilt, protocol.Message{
		Role:    protocol.RoleUser,
		Content: "<previous_review_summary>\n" + summary + "\n</previous_review_summary>",
	})
	rebuilt = append(rebuilt, preserved...)
	afterTokens := estimateMessagesTokens(rebuilt)
	if afterTokens >= beforeTokens {
		r.process.finishCompression(pending, "ineffective", afterTokens, compressionUsage, errCompressionIneffective)
		return messages, "memory_compression_ineffective: " + file
	}

	r.process.finishCompression(pending, "success", afterTokens, compressionUsage, nil)
	r.trace("%scontext compressed (%d -> %d tokens)", r.progressFilePrefix(file), beforeTokens, afterTokens)
	return rebuilt, ""
}

func estimateMessagesTokens(messages []protocol.Message) int {
	return estimateRequestTokens(protocol.Request{Messages: messages})
}

func usageDifference(after, before agent.TokenUsage) agent.TokenUsage {
	return agent.TokenUsage{
		PromptTokens:     after.PromptTokens - before.PromptTokens,
		CompletionTokens: after.CompletionTokens - before.CompletionTokens,
		TotalTokens:      after.TotalTokens - before.TotalTokens,
		LLMRequests:      after.LLMRequests - before.LLMRequests,
		CacheReadTokens:  after.CacheReadTokens - before.CacheReadTokens,
		CacheWriteTokens: after.CacheWriteTokens - before.CacheWriteTokens,
	}
}
