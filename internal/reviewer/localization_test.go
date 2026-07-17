package reviewer

import (
	"context"
	"strings"
	"testing"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/config"
	"github.com/koderover/zadig-code-review-agent/internal/protocol"
)

func TestLocalizeFindingsTranslatesOnlyHumanReadableFields(t *testing.T) {
	llm := &recordingLLM{responses: []protocol.Response{{
		Text:  `[{"id":"c-0","title":"错误被静默丢弃","problem":"新增分支直接返回 nil。","evidence":"代码使用 ` + "`return nil`" + `。","suggestion":"返回原始错误。"}]`,
		Usage: agent.TokenUsage{PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130},
	}}}
	r := Runner{Config: config.Default(), LLM: llm}
	original := []agent.Finding{{
		Severity: "high", Category: "correctness", File: "main.go", StartLine: 10, EndLine: 10,
		Title: "Error is discarded", Problem: "The new branch returns nil.", Evidence: "It uses `return nil`.",
		Suggestion: "Return the original error.", Confidence: 0.9,
	}}
	var usage agent.TokenUsage
	localized, warning := r.localizeFindings(context.Background(), original, "Chinese", &usage)
	if warning != "" || len(localized) != 1 {
		t.Fatalf("unexpected localization result: %+v warning=%q", localized, warning)
	}
	if localized[0].Title != "错误被静默丢弃" || !strings.Contains(localized[0].Problem, "新增分支") {
		t.Fatalf("human-readable fields were not localized: %+v", localized[0])
	}
	if localized[0].Severity != original[0].Severity || localized[0].File != original[0].File || localized[0].StartLine != original[0].StartLine || localized[0].Confidence != original[0].Confidence {
		t.Fatalf("localization changed structural finding data: before=%+v after=%+v", original[0], localized[0])
	}
	if len(llm.requests) != 1 || !requestContains(llm.requests[0], "Every natural-language sentence must use Chinese") || usage.LLMRequests != 1 || usage.TotalTokens != 130 {
		t.Fatalf("localization request or usage missing: requests=%+v usage=%+v", llm.requests, usage)
	}
}

func TestLocalizeFindingsSkipsEnglishOutput(t *testing.T) {
	original := []agent.Finding{{Title: "keep"}}
	r := Runner{Config: config.Default(), LLM: failLLM{t: t}}
	localized, warning := r.localizeFindings(context.Background(), original, "en-US", &agent.TokenUsage{})
	if warning != "" || localized[0].Title != "keep" {
		t.Fatalf("English output should not invoke localization: %+v warning=%q", localized, warning)
	}
}

func TestLocalizeFindingsAcceptsWrappedArray(t *testing.T) {
	r := Runner{Config: config.Default(), LLM: &sequenceLLM{responses: []protocol.Response{{
		Text: `{"findings":[{"id":"c-0","title":"标题","problem":"问题","evidence":"证据","suggestion":"建议"}]}`,
	}}}}
	original := []agent.Finding{{File: "main.go", Title: "title", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion"}}
	localized, warning := r.localizeFindings(context.Background(), original, "Chinese", &agent.TokenUsage{})
	if warning != "" || localized[0].Title != "标题" {
		t.Fatalf("wrapped localization was not accepted: %+v warning=%q", localized, warning)
	}
}
