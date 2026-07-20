package reviewer

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/protocol"
)

type processRecorder struct {
	mu                  sync.Mutex
	started             time.Time
	nextID              int
	nextCompressionID   int
	nextModelResponseID int
	calls               []agent.ToolCall
	compressions        []agent.Compression
	modelResponses      []agent.ModelResponse
}

type pendingCompression struct {
	id                 int
	file               string
	round              int
	startedAt          time.Time
	before             int
	compressedMessages int
	preservedMessages  int
}

type pendingToolCall struct {
	id        int
	file      string
	round     int
	action    toolAction
	startedAt time.Time
}

type pendingModelResponse struct {
	id        int
	stage     string
	file      string
	attempt   int
	startedAt time.Time
}

func newProcessRecorder(started time.Time) *processRecorder {
	return &processRecorder{started: started}
}

func (r *processRecorder) begin(file string, round int, action toolAction) pendingToolCall {
	now := time.Now()
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()
	return pendingToolCall{id: id, file: file, round: round, action: action, startedAt: now}
}

func (r *processRecorder) beginModelResponse(stage, file string, attempt int) pendingModelResponse {
	now := time.Now()
	r.mu.Lock()
	r.nextModelResponseID++
	id := r.nextModelResponseID
	r.mu.Unlock()
	return pendingModelResponse{id: id, stage: stage, file: file, attempt: attempt, startedAt: now}
}

func (r *processRecorder) finishModelResponse(p pendingModelResponse, response protocol.Response, err error) {
	record := agent.ModelResponse{
		ID: fmt.Sprintf("model-%04d", p.id), Stage: p.stage, File: p.file, Attempt: p.attempt,
		Status: "success", StartedOffsetMS: elapsedMilliseconds(p.startedAt.Sub(r.started)),
		DurationMS: elapsedMilliseconds(time.Since(p.startedAt)), FinishReason: response.FinishReason,
		Text: response.Text, Usage: response.Usage,
	}
	if err != nil {
		record.Status = "error"
		record.Error = err.Error()
	}
	r.mu.Lock()
	r.modelResponses = append(r.modelResponses, record)
	r.mu.Unlock()
}

func (r *processRecorder) finish(call pendingToolCall, result toolExecution) agent.ToolCall {
	duration := elapsedMilliseconds(time.Since(call.startedAt))
	offset := elapsedMilliseconds(call.startedAt.Sub(r.started))
	record := agent.ToolCall{
		ID:              fmt.Sprintf("tool-%04d", call.id),
		File:            call.file,
		Round:           call.round,
		Tool:            call.action.Tool,
		Arguments:       toolArguments(call.action),
		Status:          result.Status,
		Cached:          result.Cached,
		StartedOffsetMS: offset,
		DurationMS:      duration,
		OutputBytes:     result.OutputBytes,
		OutputTruncated: result.Truncated,
		Summary:         result.Summary,
		Output:          result.Output,
	}
	r.mu.Lock()
	r.calls = append(r.calls, record)
	r.mu.Unlock()
	return record
}

func contextToolCacheKey(action toolAction) string {
	key := struct {
		Tool          string   `json:"tool"`
		FilePath      string   `json:"file_path,omitempty"`
		QueryName     string   `json:"query_name,omitempty"`
		SearchText    string   `json:"search_text,omitempty"`
		FilePatterns  []string `json:"file_patterns,omitempty"`
		CaseSensitive bool     `json:"case_sensitive,omitempty"`
		UsePerlRegexp bool     `json:"use_perl_regexp,omitempty"`
		StartLine     int      `json:"start_line,omitempty"`
		EndLine       int      `json:"end_line,omitempty"`
	}{
		Tool: action.Tool, FilePath: action.filePath(), QueryName: action.queryName(),
		SearchText: action.searchText(), FilePatterns: action.FilePatterns,
		CaseSensitive: action.CaseSensitive, UsePerlRegexp: action.UsePerlRegexp,
		StartLine: action.StartLine, EndLine: action.EndLine,
	}
	data, _ := json.Marshal(key)
	return string(data)
}

func (r *processRecorder) beginCompression(file string, round, before, compressedMessages, preservedMessages int) pendingCompression {
	now := time.Now()
	r.mu.Lock()
	r.nextCompressionID++
	id := r.nextCompressionID
	r.mu.Unlock()
	return pendingCompression{
		id: id, file: file, round: round, startedAt: now, before: before,
		compressedMessages: compressedMessages, preservedMessages: preservedMessages,
	}
}

func (r *processRecorder) finishCompression(pending pendingCompression, status string, after int, usage agent.TokenUsage, err error) agent.Compression {
	record := agent.Compression{
		ID:                 fmt.Sprintf("compression-%04d", pending.id),
		File:               pending.file,
		Round:              pending.round,
		Status:             status,
		StartedOffsetMS:    elapsedMilliseconds(pending.startedAt.Sub(r.started)),
		DurationMS:         elapsedMilliseconds(time.Since(pending.startedAt)),
		BeforeTokens:       pending.before,
		AfterTokens:        after,
		CompressedMessages: pending.compressedMessages,
		PreservedMessages:  pending.preservedMessages,
		Usage:              usage,
	}
	if err != nil {
		record.Error = err.Error()
	}
	r.mu.Lock()
	r.compressions = append(r.compressions, record)
	r.mu.Unlock()
	return record
}

func (r *processRecorder) snapshot() agent.ReviewProcess {
	r.mu.Lock()
	calls := append(make([]agent.ToolCall, 0, len(r.calls)), r.calls...)
	compressions := append(make([]agent.Compression, 0, len(r.compressions)), r.compressions...)
	modelResponses := append(make([]agent.ModelResponse, 0, len(r.modelResponses)), r.modelResponses...)
	r.mu.Unlock()
	sort.Slice(calls, func(i, j int) bool { return calls[i].ID < calls[j].ID })
	sort.Slice(compressions, func(i, j int) bool { return compressions[i].ID < compressions[j].ID })
	sort.Slice(modelResponses, func(i, j int) bool { return modelResponses[i].ID < modelResponses[j].ID })
	return agent.ReviewProcess{ToolCalls: calls, Compressions: compressions, ModelResponses: modelResponses}
}

func toolArguments(action toolAction) agent.ToolArguments {
	arguments := agent.ToolArguments{}
	switch action.Tool {
	case "file_read":
		arguments.FilePath = action.filePath()
		arguments.StartLine = action.StartLine
		arguments.EndLine = action.EndLine
	case "file_find":
		arguments.QueryName = action.queryName()
		arguments.CaseSensitive = action.CaseSensitive
	case "code_search":
		arguments.SearchText = action.searchText()
		arguments.FilePatterns = action.FilePatterns
		arguments.CaseSensitive = action.CaseSensitive
		arguments.UsePerlRegexp = action.UsePerlRegexp
	case "code_comment":
		finding := action.Finding
		arguments.Finding = &finding
	}
	return arguments
}
