package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/protocol"
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
	var parseErr error
	for attempt := 1; attempt <= 2; attempt++ {
		response, err := r.completeAudited(ctx, "localization", findings[0].File, attempt, request, usage)
		if err != nil {
			return findings, "finding_localization_failed: " + err.Error()
		}
		localized, parseErr = parseLocalizedFindings(response.Text)
		if parseErr == nil {
			break
		}
		if attempt == 1 {
			request.Messages = append(request.Messages,
				protocol.Message{Role: protocol.RoleAssistant, Content: response.Text},
				protocol.Message{Role: protocol.RoleUser, Content: "Your response was not valid. Return only the complete JSON array with one item for every supplied ID and no wrapper or explanation."},
			)
			r.trace("%sfinding localization response invalid; retrying", r.progressFilePrefix(findings[0].File))
		}
	}
	if parseErr != nil {
		return findings, "finding_localization_failed: invalid response: " + parseErr.Error()
	}
	if len(localized) != len(findings) {
		return findings, fmt.Sprintf("finding_localization_failed: expected %d items, got %d", len(findings), len(localized))
	}
	result := append([]agent.Finding(nil), findings...)
	seen := make(map[string]bool, len(localized))
	for _, item := range localized {
		var index int
		if _, err := fmt.Sscanf(item.ID, "c-%d", &index); err != nil || index < 0 || index >= len(result) || item.ID != fmt.Sprintf("c-%d", index) || seen[item.ID] {
			return findings, "finding_localization_failed: unknown or duplicate finding ID " + item.ID
		}
		if localizedFieldMissing(findings[index], item) {
			return findings, "finding_localization_failed: localized content is incomplete for " + item.ID
		}
		seen[item.ID] = true
		result[index].Title = item.Title
		result[index].Problem = item.Problem
		result[index].Evidence = item.Evidence
		result[index].Suggestion = item.Suggestion
	}
	return result, ""
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
