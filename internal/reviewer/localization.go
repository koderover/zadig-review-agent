package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/protocol"
)

type localizedFinding struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Problem    string `json:"problem"`
	Evidence   string `json:"evidence"`
	Suggestion string `json:"suggestion"`
}

func (r Runner) localizeFindings(ctx context.Context, findings []agent.Finding, language string, usage *agent.TokenUsage) ([]agent.Finding, string) {
	if len(findings) == 0 || isEnglishLanguage(language) {
		return findings, ""
	}
	if findingsAlreadyUseTargetLanguage(findings, language) {
		r.trace("%sfinding localization skipped: already %s", r.progressFilePrefix(findings[0].File), outputLanguage(language))
		return findings, ""
	}
	items := make([]localizedFinding, 0, len(findings))
	for index, finding := range findings {
		items = append(items, localizedFinding{
			ID: fmt.Sprintf("c-%d", index), Title: finding.Title, Problem: finding.Problem,
			Evidence: finding.Evidence, Suggestion: finding.Suggestion,
		})
	}
	data, err := json.Marshal(items)
	if err != nil {
		return findings, "finding_localization_failed: " + err.Error()
	}
	messages, err := loadPromptMessages("localization", map[string]string{
		"language": outputLanguage(language),
		"findings": string(data),
	})
	if err != nil {
		return findings, "finding_localization_failed: " + err.Error()
	}
	request := protocol.Request{Messages: messages}
	var localized []localizedFinding
	var responseErr error
	var lastResponse protocol.Response
	for attempt := 1; attempt <= 2; attempt++ {
		response, err := r.completeAudited(ctx, "localization", findings[0].File, attempt, request, usage)
		if err != nil {
			return findings, "finding_localization_failed: " + err.Error()
		}
		lastResponse = response
		localized, responseErr = parseLocalizedFindings(response.Text)
		if responseErr == nil {
			responseErr = validateLocalizedFindings(findings, localized)
		}
		if responseErr == nil {
			break
		}
		if attempt == 1 {
			if !modelOutputTruncated(response) {
				request.Messages = append(request.Messages, protocol.Message{Role: protocol.RoleAssistant, Content: response.Text})
			}
			request.Messages = append(request.Messages, protocol.Message{Role: protocol.RoleUser, Content: "Your response was not valid: " + responseErr.Error() + ". Return only the complete JSON array with one item for every supplied ID. Preserve every non-empty field and do not add a wrapper or explanation."})
			r.trace("%sfinding localization response invalid; retrying", r.progressFilePrefix(findings[0].File))
		}
	}
	if responseErr != nil {
		if modelOutputTruncated(lastResponse) {
			return findings, fmt.Sprintf(
				"finding_localization_failed: model output truncated (agent_source=%q stage=localization file=%q attempt=2 finish_reason=%q completion_tokens=%d visible_chars=%d): %v",
				agentSourceLocation(0), findings[0].File, lastResponse.FinishReason,
				lastResponse.Usage.CompletionTokens, len([]rune(lastResponse.Text)), responseErr,
			)
		}
		return findings, "finding_localization_failed: invalid response: " + responseErr.Error()
	}
	result := append([]agent.Finding(nil), findings...)
	for _, item := range localized {
		var index int
		_, _ = fmt.Sscanf(item.ID, "c-%d", &index)
		result[index].Title = item.Title
		result[index].Problem = item.Problem
		result[index].Evidence = item.Evidence
		result[index].Suggestion = item.Suggestion
	}
	return result, ""
}

func validateLocalizedFindings(original []agent.Finding, localized []localizedFinding) error {
	if len(localized) != len(original) {
		return fmt.Errorf("expected %d items, got %d", len(original), len(localized))
	}
	seen := make(map[string]bool, len(localized))
	for _, item := range localized {
		var index int
		if _, err := fmt.Sscanf(item.ID, "c-%d", &index); err != nil || index < 0 || index >= len(original) || item.ID != fmt.Sprintf("c-%d", index) || seen[item.ID] {
			return fmt.Errorf("unknown or duplicate finding ID %q", item.ID)
		}
		if localizedFieldMissing(original[index], item) {
			return fmt.Errorf("localized content is incomplete for %s", item.ID)
		}
		seen[item.ID] = true
	}
	return nil
}

func modelOutputTruncated(response protocol.Response) bool {
	finishReason := strings.ToLower(strings.TrimSpace(response.FinishReason))
	return finishReason == "length" || finishReason == "max_tokens"
}

func findingsAlreadyUseTargetLanguage(findings []agent.Finding, language string) bool {
	if !isChineseLanguage(language) {
		return false
	}
	sawText := false
	for _, finding := range findings {
		for _, field := range []string{finding.Title, finding.Problem, finding.Evidence, finding.Suggestion} {
			if strings.TrimSpace(field) == "" {
				continue
			}
			sawText = true
			if !usesChineseScript(field) {
				return false
			}
		}
	}
	return sawText
}

func isChineseLanguage(language string) bool {
	normalized := strings.ToLower(strings.TrimSpace(language))
	return normalized == "chinese" || normalized == "zh" || normalized == "中文" ||
		strings.HasPrefix(normalized, "zh-") || strings.HasPrefix(normalized, "zh_") ||
		strings.Contains(normalized, "简体中文") || strings.Contains(normalized, "繁體中文")
}

func usesChineseScript(text string) bool {
	hasHan := false
	for _, char := range text {
		switch {
		case unicode.Is(unicode.Hiragana, char), unicode.Is(unicode.Katakana, char), unicode.Is(unicode.Hangul, char):
			return false
		case unicode.Is(unicode.Han, char):
			hasHan = true
		}
	}
	return hasHan
}

func parseLocalizedFindings(text string) ([]localizedFinding, error) {
	data := extractJSONValue(text)
	var findings []localizedFinding
	if err := json.Unmarshal(data, &findings); err == nil {
		return findings, nil
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	for _, key := range []string{"findings", "localized_findings", "translations", "items"} {
		value, ok := wrapped[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(value, &findings); err != nil {
			return nil, fmt.Errorf("%s must be an array: %w", key, err)
		}
		return findings, nil
	}
	return nil, fmt.Errorf("expected a findings array or an object containing findings")
}

func localizedFieldMissing(original agent.Finding, localized localizedFinding) bool {
	return strings.TrimSpace(localized.Title) == "" ||
		(strings.TrimSpace(original.Problem) != "" && strings.TrimSpace(localized.Problem) == "") ||
		(strings.TrimSpace(original.Evidence) != "" && strings.TrimSpace(localized.Evidence) == "") ||
		(strings.TrimSpace(original.Suggestion) != "" && strings.TrimSpace(localized.Suggestion) == "")
}

func isEnglishLanguage(language string) bool {
	normalized := strings.ToLower(strings.TrimSpace(language))
	return normalized == "english" || normalized == "en" || strings.HasPrefix(normalized, "en-") || strings.HasPrefix(normalized, "en_")
}
