package reviewer

import (
	"context"
	"strings"
	"testing"

	"github.com/koderover/zadig-review-agent/internal/agent"
	"github.com/koderover/zadig-review-agent/internal/config"
	"github.com/koderover/zadig-review-agent/internal/protocol"
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

func TestLocalizeFindingsSkipsAlreadyChineseOutput(t *testing.T) {
	original := []agent.Finding{{
		Title: "错误被静默丢弃", Problem: "新增分支直接返回 nil。",
		Evidence: "代码使用 `return nil`。", Suggestion: "返回原始错误。",
	}}
	r := Runner{Config: config.Default(), LLM: failLLM{t: t}}
	localized, warning := r.localizeFindings(context.Background(), original, "Chinese", &agent.TokenUsage{})
	if warning != "" || localized[0].Title != original[0].Title {
		t.Fatalf("already-Chinese findings should not invoke localization: %+v warning=%q", localized, warning)
	}
}

func TestLocalizeFindingsReportsTruncatedModelOutput(t *testing.T) {
	llm := &recordingLLM{responses: []protocol.Response{
		{Text: `[{"id":"c-0","title":"截断`, FinishReason: "length", Usage: agent.TokenUsage{CompletionTokens: 4096}},
		{Text: `[{"id":"c-0","title":"仍然截断`, FinishReason: "length", Usage: agent.TokenUsage{CompletionTokens: 4096}},
	}}
	r := Runner{Config: config.Default(), LLM: llm}
	original := []agent.Finding{{File: "main.go", Title: "English title", Problem: "English problem", Evidence: "English evidence", Suggestion: "English suggestion"}}
	localized, warning := r.localizeFindings(context.Background(), original, "Chinese", &agent.TokenUsage{})
	if localized[0].Title != original[0].Title {
		t.Fatalf("truncated localization must preserve original finding: %+v", localized)
	}
	for _, want := range []string{
		"finding_localization_failed: model output truncated", `agent_source="internal/reviewer/localization.go:`,
		"finish_reason=\"length\"", "completion_tokens=4096", "visible_chars=",
	} {
		if !strings.Contains(warning, want) {
			t.Fatalf("truncation warning missing %q: %s", want, warning)
		}
	}
	if len(llm.requests) != 2 || requestContains(llm.requests[1], "截断") {
		t.Fatalf("truncated output must not be replayed into the retry: %+v", llm.requests)
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

func TestLocalizeFindingsRetriesMissingFields(t *testing.T) {
	llm := &recordingLLM{responses: []protocol.Response{
		{Text: `[{"id":"c-0","title":"标题","problem":"","evidence":"证据","suggestion":"建议"}]`},
		{Text: `[{"id":"c-0","title":"标题","problem":"问题","evidence":"证据","suggestion":"建议"}]`},
	}}
	r := Runner{Config: config.Default(), LLM: llm}
	original := []agent.Finding{{File: "main.go", Title: "title", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion"}}
	localized, warning := r.localizeFindings(context.Background(), original, "Chinese", &agent.TokenUsage{})
	if warning != "" || localized[0].Problem != "问题" || len(llm.requests) != 2 || !requestContains(llm.requests[1], "localized content is incomplete for c-0") {
		t.Fatalf("missing field was not retried: localized=%+v warning=%q requests=%+v", localized, warning, llm.requests)
	}
}

func TestLocalizeFindingsReportsMissingFieldsAfterRetry(t *testing.T) {
	llm := &recordingLLM{responses: []protocol.Response{
		{Text: `[{"id":"c-0","title":"标题","problem":"","evidence":"证据","suggestion":"建议"}]`},
		{Text: `[{"id":"c-0","title":"标题","problem":"","evidence":"证据","suggestion":"建议"}]`},
	}}
	r := Runner{Config: config.Default(), LLM: llm}
	original := []agent.Finding{{File: "main.go", Title: "title", Problem: "problem", Evidence: "evidence", Suggestion: "suggestion"}}
	localized, warning := r.localizeFindings(context.Background(), original, "Chinese", &agent.TokenUsage{})
	if localized[0].Problem != original[0].Problem || !strings.Contains(warning, "localized content is incomplete for c-0") || len(llm.requests) != 2 {
		t.Fatalf("failed retry should preserve original finding: localized=%+v warning=%q requests=%d", localized, warning, len(llm.requests))
	}
}
