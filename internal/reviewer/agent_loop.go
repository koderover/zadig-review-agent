package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-review-agent/internal/protocol"
	"github.com/koderover/zadig-review-agent/internal/rules"
)

const planLineThreshold = 50
const maxLLMRetries = 1

var errTokenThreshold = errors.New("token threshold exceeded")

func (r Runner) traceToolCall(file string, call agent.ToolCall) {
	status := ""
	if call.Cached {
		status = " cached"
	} else if call.Status != "success" {
		status = " failed"
	}
	summary := call.Summary
	if call.Tool == "file_read" && (call.Arguments.StartLine > 0 || call.Arguments.EndLine > 0) {
		summary = fmt.Sprintf("lines %d-%d", call.Arguments.StartLine, call.Arguments.EndLine)
	}
	r.trace("%s%s%s (%s): %s", r.progressFilePrefix(file), toolProgressLabel(call), status, time.Duration(call.DurationMS)*time.Millisecond, summary)
}

func toolProgressLabel(call agent.ToolCall) string {
	switch call.Tool {
	case "file_read":
		return fmt.Sprintf("file_read %q", truncateProgressValue(call.Arguments.FilePath))
	case "code_search":
		scope := "repository"
		if len(call.Arguments.FilePatterns) > 0 {
			data, _ := json.Marshal(call.Arguments.FilePatterns)
			scope = string(data)
		}
		return fmt.Sprintf("code_search %q in %s", truncateProgressValue(call.Arguments.SearchText), scope)
	case "file_find":
		return fmt.Sprintf("file_find %q", truncateProgressValue(call.Arguments.QueryName))
	default:
		return call.Tool
	}
}

func truncateProgressValue(value string) string {
	const limit = 120
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func (r Runner) runSubtask(ctx context.Context, file gitdiff.FileDiff, rule rules.ResolvedRule, allFiles []gitdiff.FileDiff) ([]agent.Finding, agent.TokenUsage, []string, error) {
	var usage agent.TokenUsage
	var warnings []string
	if estimateTokens(renderFileDiff(file)) > r.Config.Review.MaxChunkTokens*4/5 {
		r.trace("%sskipped: token budget exceeded", r.progressFilePrefix(file.Path))
		return nil, usage, []string{"token_threshold_exceeded: " + file.Path}, nil
	}

	values := map[string]string{
		"current_file_path": file.Path,
		"change_files":      changedFilesList(allFiles, file.Path),
		"system_rule":       rule.Rule,
		"diff":              renderFileDiff(file),
		"language":          outputLanguage(r.Config.Output.Language),
		"plan_guidance":     "No separate plan was required.",
	}
	changedLines := changedLineCount(file.Path, allFiles)
	if changedLines >= planLineThreshold {
		r.trace("%splanning", r.progressFilePrefix(file.Path))
		planMessages, err := loadPromptMessages("plan", values)
		if err != nil {
			return nil, usage, warnings, err
		}
		response, err := r.completeAudited(ctx, "plan", file.Path, 1, protocol.Request{Messages: planMessages}, &usage)
		if err != nil {
			r.trace("%splan failed", r.progressFilePrefix(file.Path))
			warnings = append(warnings, "plan_failed: "+file.Path+": "+err.Error())
		} else {
			r.trace("%splan completed", r.progressFilePrefix(file.Path))
			values["plan_guidance"] = response.Text
		}
	} else {
		r.trace("%splan skipped (%d lines < %d)", r.progressFilePrefix(file.Path), changedLines, planLineThreshold)
	}

	candidates, loopWarnings, err := r.runMainLoop(ctx, file, values, changedLines, &usage)
	warnings = append(warnings, loopWarnings...)
	if err != nil {
		return nil, usage, warnings, err
	}
	if len(candidates) == 0 {
		return nil, usage, warnings, nil
	}

	positioned := make([]agent.Finding, 0, len(candidates))
	for _, candidate := range candidates {
		finding, ok, relocationWarning := r.positionFinding(ctx, candidate, file, values, &usage, true)
		if relocationWarning != "" {
			warnings = append(warnings, relocationWarning)
		}
		if ok {
			positioned = append(positioned, finding)
		}
	}
	if len(positioned) == 0 {
		return nil, usage, warnings, nil
	}

	filtered, filterWarning := r.filterFindings(ctx, positioned, values, &usage)
	if filterWarning != "" {
		r.trace("%sreview filter failed", r.progressFilePrefix(file.Path))
		warnings = append(warnings, filterWarning)
	}
	removed := len(positioned) - len(filtered)
	if removed < 0 {
		removed = 0
	}
	if removed > 0 {
		r.trace("%sreview filter removed %d comment(s)", r.progressFilePrefix(file.Path), removed)
	}
	localized, localizationWarning := r.localizeFindings(ctx, filtered, r.Config.Output.Language, &usage)
	if localizationWarning != "" {
		r.trace("%sfinding localization failed", r.progressFilePrefix(file.Path))
		warnings = append(warnings, localizationWarning)
	} else {
		filtered = localized
	}
	final := make([]agent.Finding, 0, len(filtered))
	for _, candidate := range filtered {
		finding, ok, _ := r.positionFinding(ctx, candidate, file, values, &usage, false)
		if ok {
			final = append(final, finding)
		}
	}
	findings, err := validateFindings(final, file, rule, r.Config.Review.ConfidenceThreshold)
	return findings, usage, warnings, err
}

func (r Runner) runMainLoop(ctx context.Context, file gitdiff.FileDiff, values map[string]string, changedLines int, usage *agent.TokenUsage) ([]agent.Finding, []string, error) {
	executor := newToolExecutor(r.Root, r.DiffRequest)
	definitions, err := loadToolDefinitions()
	if err != nil {
		return nil, nil, err
	}
	messages, err := loadPromptMessages("main", values)
	if err != nil {
		return nil, nil, err
	}
	maxToolRounds := r.Config.Review.MaxToolRounds
	if maxToolRounds < 1 {
		maxToolRounds = 1
	}
	maxContextToolCalls := r.Config.Review.MaxContextToolCalls
	if maxContextToolCalls < 1 {
		maxContextToolCalls = 10
	}
	maxContextToolCalls = contextToolBudget(maxContextToolCalls, changedLines)
	finalizationDefinitions := finalizationTools(definitions)
	var comments []agent.Finding
	var warnings []string
	consecutiveEmptyRounds := 0
	requireTool := false
	hadToolActivity := false
	contextToolCalls := 0
	contextToolCache := make(map[string]cachedContextTool)
	finalizing := false
	compressionEnabled := true
	for round := 0; round < maxToolRounds; round++ {
		if contextToolCalls >= maxContextToolCalls && !finalizing {
			finalizing = true
			requireTool = true
			messages = append(messages, protocol.Message{Role: protocol.RoleUser, Content: "The context tool budget is exhausted. Finalize now using code_comment for each concrete finding, then task_done. Do not request more repository context."})
			r.trace("%scontext tool budget reached (%d); finalizing review", r.progressFilePrefix(file.Path), maxContextToolCalls)
		}
		requestTools := definitions
		if finalizing {
			requestTools = finalizationDefinitions
		}
		requestRequiredTool := requireTool
		if compressionEnabled {
			var compressionWarning string
			messages, compressionWarning = r.maybeCompressMessages(ctx, file.Path, round+1, messages, requestTools, requestRequiredTool, usage)
			if compressionWarning != "" {
				compressionEnabled = false
				warnings = append(warnings, compressionWarning)
				r.trace("%scontext compression disabled after failure", r.progressFilePrefix(file.Path))
			}
		}
		response, err := r.completeTracked(ctx, protocol.Request{Messages: messages, Tools: requestTools, RequireTool: requestRequiredTool}, usage)
		if err != nil {
			if errors.Is(err, errTokenThreshold) {
				warnings = append(warnings, "token_threshold_exceeded: "+file.Path)
				return comments, warnings, nil
			}
			return nil, warnings, err
		}
		if len(response.ToolCalls) == 0 {
			if hadToolActivity && requestRequiredTool && (strings.TrimSpace(response.Text) != "" || consecutiveEmptyRounds > 0) {
				if strings.TrimSpace(response.Text) == "" {
					r.trace("%sreview completed after empty required-tool retry", r.progressFilePrefix(file.Path))
				} else {
					r.trace("%sreview completed by model end turn", r.progressFilePrefix(file.Path))
				}
				return comments, warnings, nil
			}
			consecutiveEmptyRounds++
			requireTool = true
			messages = append(messages,
				protocol.Message{Role: protocol.RoleAssistant, Content: response.Text},
				protocol.Message{Role: protocol.RoleUser, Content: "You did not call a tool. Continue the review with a context tool, code_comment, or task_done."},
			)
			if consecutiveEmptyRounds >= 3 {
				warnings = append(warnings, "tool_loop_empty_limit_reached: "+file.Path)
				return comments, warnings, nil
			}
			r.trace("%smodel returned no tool call; retrying with a required tool (%d/3)", r.progressFilePrefix(file.Path), consecutiveEmptyRounds)
			continue
		}
		consecutiveEmptyRounds = 0
		requireTool = false
		hadToolActivity = true
		messages = append(messages, protocol.Message{Role: protocol.RoleAssistant, Content: response.Text, ToolCalls: response.ToolCalls})
		done := false
		for callIndex, toolCall := range response.ToolCalls {
			if toolCall.ID == "" {
				toolCall.ID = fmt.Sprintf("round-%d-call-%d", round+1, callIndex+1)
			}
			action := toolAction{Tool: toolCall.Name}
			if err := json.Unmarshal([]byte(toolCall.Arguments), &action); err != nil {
				result := toolExecution{Output: "error: invalid tool arguments: " + err.Error(), Status: "error", Summary: "invalid tool arguments"}
				call := r.process.begin(file.Path, round+1, action)
				record := r.process.finish(call, result)
				r.traceToolCall(file.Path, record)
				messages = append(messages, protocol.Message{Role: protocol.RoleTool, ToolCallID: toolCall.ID, ToolName: toolCall.Name, Content: result.Output})
				continue
			}
			action.Tool = toolCall.Name
			var result toolExecution
			switch action.Tool {
			case "code_comment":
				if action.Finding.File == "" {
					action.Finding.File = file.Path
				}
				comments = append(comments, action.Finding)
				result = toolExecution{Output: "comment collected", OutputBytes: len("comment collected"), Status: "success", Summary: "comment collected"}
				call := r.process.begin(file.Path, round+1, action)
				record := r.process.finish(call, result)
				r.traceToolCall(file.Path, record)
			case "task_done":
				done = true
				result = toolExecution{Output: "review completed", OutputBytes: len("review completed"), Status: "success", Summary: "review completed"}
			case "file_read", "code_search", "file_find":
				call := r.process.begin(file.Path, round+1, action)
				if cached, ok := findCachedContextTool(action, contextToolCache); ok {
					result = cached.result
					result.Cached = true
					result.Summary = "cached: " + result.Summary
				} else if contextToolCalls >= maxContextToolCalls {
					result = toolExecution{Output: "error: context tool budget exhausted; finalize with code_comment and task_done", Status: "error", Summary: "context tool budget exhausted"}
				} else {
					contextToolCalls++
					result = executor.execute(ctx, action)
				}
				record := r.process.finish(call, result)
				if !result.Cached && result.Status == "success" && result.Summary != "context tool budget exhausted" {
					contextToolCache[contextToolCacheKey(action)] = cachedContextTool{action: action, result: result, record: record}
				}
				r.traceToolCall(file.Path, record)
			default:
				result = toolExecution{Output: "error: unsupported tool " + action.Tool, Status: "error", Summary: "unsupported tool"}
			}
			messages = append(messages, protocol.Message{Role: protocol.RoleTool, ToolCallID: toolCall.ID, ToolName: toolCall.Name, Content: result.Output})
		}
		if done {
			return comments, warnings, nil
		}
	}
	warnings = append(warnings, "tool_loop_limit_reached: "+file.Path)
	return comments, warnings, nil
}

func contextToolBudget(configured, changedLines int) int {
	limit := configured
	if changedLines <= 10 && limit > 6 {
		limit = 6
	} else if changedLines <= 50 && limit > 8 {
		limit = 8
	}
	return limit
}

type cachedContextTool struct {
	action toolAction
	result toolExecution
	record agent.ToolCall
}

func findCachedContextTool(action toolAction, cache map[string]cachedContextTool) (cachedContextTool, bool) {
	if cached, ok := cache[contextToolCacheKey(action)]; ok {
		return cached, true
	}
	if action.Tool != "file_read" {
		return cachedContextTool{}, false
	}
	requestedStart := action.StartLine
	if requestedStart < 1 {
		requestedStart = 1
	}
	for _, cached := range cache {
		if cached.action.Tool != "file_read" || cached.action.filePath() != action.filePath() {
			continue
		}
		start, end, total, truncated, ok := fileReadOutputRange(cached.result.Output)
		if !ok || requestedStart < start {
			continue
		}
		requestedEnd := action.EndLine
		if requestedEnd < requestedStart {
			requestedEnd = total
			if truncated || start != 1 {
				continue
			}
		}
		if requestedEnd <= end {
			return cached, true
		}
	}
	return cachedContextTool{}, false
}

func fileReadOutputRange(output string) (start, end, total int, truncated, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "File: "):
			index := strings.LastIndex(line, "(")
			if index < 0 {
				continue
			}
			if _, err := fmt.Sscanf(line[index:], "(Total lines: %d)", &total); err != nil {
				total = 0
			}
		case strings.HasPrefix(line, "IS_TRUNCATED: "):
			truncated = strings.TrimSpace(strings.TrimPrefix(line, "IS_TRUNCATED: ")) == "true"
		case strings.HasPrefix(line, "LINE_RANGE: "):
			_, err := fmt.Sscanf(strings.TrimPrefix(line, "LINE_RANGE: "), "%d-%d", &start, &end)
			ok = err == nil
		}
	}
	return
}

func finalizationTools(definitions []protocol.ToolDefinition) []protocol.ToolDefinition {
	tools := make([]protocol.ToolDefinition, 0, 2)
	for _, definition := range definitions {
		if definition.Name == "code_comment" || definition.Name == "task_done" {
			tools = append(tools, definition)
		}
	}
	return tools
}

func (r Runner) filterFindings(ctx context.Context, candidates []agent.Finding, values map[string]string, usage *agent.TokenUsage) ([]agent.Finding, string) {
	type candidate struct {
		ID      string        `json:"id"`
		Finding agent.Finding `json:"finding"`
	}
	indexed := make([]candidate, 0, len(candidates))
	for index, finding := range candidates {
		indexed = append(indexed, candidate{ID: fmt.Sprintf("c-%d", index), Finding: finding})
	}
	data, _ := json.Marshal(indexed)
	filterValues := cloneValues(values)
	filterValues["comments"] = string(data)
	messages, err := loadPromptMessages("review_filter", filterValues)
	if err != nil {
		return candidates, "review_filter_failed: " + err.Error()
	}
	request := protocol.Request{Messages: messages}
	var deletedIDs []string
	var parseErr error
	for attempt := 1; attempt <= 2; attempt++ {
		response, err := r.completeAudited(ctx, "review_filter", values["current_file_path"], attempt, request, usage)
		if err != nil {
			return candidates, "review_filter_failed: " + err.Error()
		}
		deletedIDs, parseErr = parseDeletedFindingIDs(response.Text)
		if parseErr == nil {
			break
		}
		if attempt == 1 {
			request.Messages = append(request.Messages,
				protocol.Message{Role: protocol.RoleAssistant, Content: response.Text},
				protocol.Message{Role: protocol.RoleUser, Content: `Your response was not valid. Return only a complete JSON array of candidate IDs, for example ["c-0"]. An empty result must be [].`},
			)
			r.trace("%sreview filter response invalid; retrying", r.progressFilePrefix(values["current_file_path"]))
		}
	}
	if parseErr != nil {
		return candidates, "review_filter_invalid_response: " + parseErr.Error()
	}
	deleted := make(map[string]bool, len(deletedIDs))
	for _, id := range deletedIDs {
		var index int
		if _, err := fmt.Sscanf(id, "c-%d", &index); err != nil || index < 0 || index >= len(candidates) || id != fmt.Sprintf("c-%d", index) {
			return candidates, "review_filter_invalid_response: unknown candidate ID " + id
		}
		deleted[id] = true
	}
	filtered := make([]agent.Finding, 0, len(candidates)-len(deleted))
	for index, finding := range candidates {
		if !deleted[fmt.Sprintf("c-%d", index)] {
			filtered = append(filtered, finding)
		}
	}
	return filtered, ""
}

func parseDeletedFindingIDs(text string) ([]string, error) {
	data := extractJSONValue(text)
	var ids []string
	if err := json.Unmarshal(data, &ids); err == nil {
		return ids, nil
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	for _, key := range []string{"delete_ids", "deleted_ids", "removed_ids", "ids", "remove"} {
		value, ok := wrapped[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, &ids); err != nil {
			return nil, fmt.Errorf("%s must be an array of strings: %w", key, err)
		}
		return ids, nil
	}
	return nil, fmt.Errorf("expected an ID array or an object containing delete_ids")
}

func (r Runner) completeAudited(ctx context.Context, stage, file string, attempt int, request protocol.Request, usage *agent.TokenUsage) (protocol.Response, error) {
	if r.process == nil {
		return r.completeTracked(ctx, request, usage)
	}
	pending := r.process.beginModelResponse(stage, file, attempt)
	response, err := r.completeTracked(ctx, request, usage)
	r.process.finishModelResponse(pending, response, err)
	return response, err
}

func (r Runner) completeAuditedWithoutThreshold(ctx context.Context, stage, file string, attempt int, request protocol.Request, usage *agent.TokenUsage) (protocol.Response, error) {
	if r.process == nil {
		return r.completeTrackedWithoutThreshold(ctx, request, usage)
	}
	pending := r.process.beginModelResponse(stage, file, attempt)
	response, err := r.completeTrackedWithoutThreshold(ctx, request, usage)
	r.process.finishModelResponse(pending, response, err)
	return response, err
}

func (r Runner) positionFinding(ctx context.Context, finding agent.Finding, file gitdiff.FileDiff, values map[string]string, usage *agent.TokenUsage, allowRelocation bool) (agent.Finding, bool, string) {
	if start, end, ok := resolveExistingCode(file, finding.ExistingCode); ok {
		finding.StartLine, finding.EndLine = start, end
		return finding, true, ""
	}
	if overlapsChangedLine(file, finding.StartLine, finding.EndLine) {
		return finding, true, ""
	}
	if !allowRelocation {
		return finding, false, ""
	}
	r.trace("%srelocating comment", r.progressFilePrefix(file.Path))
	data, _ := json.Marshal(finding)
	relocationValues := cloneValues(values)
	relocationValues["comment"] = string(data)
	messages, err := loadPromptMessages("relocation", relocationValues)
	if err != nil {
		return finding, false, "relocation_failed: " + file.Path + ": " + err.Error()
	}
	response, err := r.completeAudited(ctx, "relocation", file.Path, 1, protocol.Request{Messages: messages}, usage)
	if err != nil {
		return finding, false, "relocation_failed: " + file.Path + ": " + err.Error()
	}
	var relocated struct {
		ExistingCode string `json:"existing_code"`
	}
	if err := json.Unmarshal(extractJSONValue(response.Text), &relocated); err != nil {
		return finding, false, "relocation_invalid_response: " + file.Path
	}
	if start, end, ok := resolveExistingCode(file, relocated.ExistingCode); ok {
		r.trace("%srelocation completed", r.progressFilePrefix(file.Path))
		finding.ExistingCode = relocated.ExistingCode
		finding.StartLine, finding.EndLine = start, end
		return finding, true, ""
	}
	r.trace("%srelocation unresolved", r.progressFilePrefix(file.Path))
	return finding, false, ""
}

func (r Runner) completeTracked(ctx context.Context, request protocol.Request, usage *agent.TokenUsage) (protocol.Response, error) {
	if estimateRequestTokens(request) > r.Config.Review.MaxChunkTokens*4/5 {
		return protocol.Response{}, errTokenThreshold
	}
	return r.completeTrackedWithoutThreshold(ctx, request, usage)
}

func (r Runner) completeTrackedWithoutThreshold(ctx context.Context, request protocol.Request, usage *agent.TokenUsage) (protocol.Response, error) {
	var response protocol.Response
	var err error
	for attempt := 0; attempt <= maxLLMRetries; attempt++ {
		usage.LLMRequests++
		response, err = r.LLM.Complete(ctx, request)
		usage.Add(response.Usage)
		if err == nil || !protocol.IsRetryable(err) || ctx.Err() != nil || attempt == maxLLMRetries {
			return response, err
		}
		r.trace("LLM request retrying (%d/%d)", attempt+2, maxLLMRetries+1)
		timer := time.NewTimer(time.Duration(attempt+1) * 500 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return response, ctx.Err()
		case <-timer.C:
		}
	}
	return response, err
}

func estimateRequestTokens(request protocol.Request) int {
	var text strings.Builder
	for _, message := range request.Messages {
		text.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			text.WriteString(call.Name)
			text.WriteString(call.Arguments)
		}
	}
	if len(request.Tools) > 0 {
		data, _ := json.Marshal(request.Tools)
		text.Write(data)
	}
	return estimateTokens(text.String())
}

func resolveExistingCode(file gitdiff.FileDiff, existingCode string) (int, int, bool) {
	snippet := normalizedSnippet(existingCode)
	if len(snippet) == 0 {
		return 0, 0, false
	}
	for _, hunk := range file.Hunks {
		var lines []gitdiff.Line
		for _, line := range hunk.Lines {
			if line.Kind != '-' && line.NewLine > 0 {
				lines = append(lines, line)
			}
		}
		for i := 0; i+len(snippet) <= len(lines); i++ {
			matched := true
			for j := range snippet {
				if strings.TrimSpace(lines[i+j].Text) != snippet[j] {
					matched = false
					break
				}
			}
			if matched {
				start, end := lines[i].NewLine, lines[i+len(snippet)-1].NewLine
				if overlapsChangedLine(file, start, end) {
					return start, end, true
				}
			}
		}
	}
	return 0, 0, false
}

func normalizedSnippet(text string) []string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return lines
}

func stripJSONFence(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	return strings.TrimSpace(text)
}

func extractJSONValue(text string) []byte {
	clean := stripJSONFence(text)
	if json.Valid([]byte(clean)) {
		return []byte(clean)
	}
	for index := 0; index < len(clean); index++ {
		if clean[index] != '{' && clean[index] != '[' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(clean[index:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == nil && len(raw) > 0 {
			return raw
		}
	}
	return []byte(clean)
}

func changedFilesList(files []gitdiff.FileDiff, current string) string {
	var lines []string
	for _, file := range files {
		if file.Path != current {
			lines = append(lines, fmt.Sprintf("- %s (+%d -%d)", file.Path, file.Insertions, file.Deletions))
		}
	}
	if len(lines) == 0 {
		return "- none"
	}
	return strings.Join(lines, "\n")
}

func changedLineCount(path string, files []gitdiff.FileDiff) int {
	for _, file := range files {
		if file.Path == path {
			return file.Insertions + file.Deletions
		}
	}
	return 0
}

func outputLanguage(language string) string {
	if strings.TrimSpace(language) == "" {
		return "zh-CN"
	}
	return language
}

func cloneValues(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func systemPrompt(language string) string {
	prompt, _ := loadPrompt("main_system", map[string]string{"language": outputLanguage(language)})
	return prompt
}
