package reporter

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
)

func Write(report agent.Report, jsonPath, markdownPath string, console io.Writer, consoleMode string) error {
	return write(report, jsonPath, markdownPath, console, consoleMode, false)
}

func WriteResult(report agent.Report, jsonPath, markdownPath string, console io.Writer, consoleMode string) error {
	return write(report, jsonPath, markdownPath, console, consoleMode, true)
}

func write(report agent.Report, jsonPath, markdownPath string, console io.Writer, consoleMode string, resultOnly bool) error {
	if jsonPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFile(jsonPath, append(data, '\n')); err != nil {
			return fmt.Errorf("write json report: %w", err)
		}
	}
	if markdownPath != "" {
		if err := writeFile(markdownPath, []byte(Markdown(report))); err != nil {
			return fmt.Errorf("write markdown report: %w", err)
		}
	}
	if consoleMode != "none" && console != nil {
		output := Console(report, consoleMode)
		if resultOnly {
			output = "\n" + ConsoleResult(report, consoleMode)
		}
		_, _ = io.WriteString(console, output)
	}
	return nil
}

func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}

func Console(report agent.Report, mode string) string {
	return ConsoleHeader(report) + ConsoleResult(report, mode)
}

func ConsoleStart(report agent.Report) string {
	return ConsoleHeader(report) + "Review process:\n"
}

func ConsoleHeader(report agent.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Code review: mode=%s", report.Metadata.DiffMode)
	if report.Metadata.From != "" || report.Metadata.To != "" {
		fmt.Fprintf(&b, " from=%s to=%s", report.Metadata.From, report.Metadata.To)
	}
	if report.Metadata.Commit != "" {
		fmt.Fprintf(&b, " commit=%s", report.Metadata.Commit)
	}
	fmt.Fprintf(&b, " head=%s protocol=%s model=%s language=%s\n", short(report.Metadata.Head), report.Metadata.Protocol, report.Metadata.Model, report.Metadata.Language)
	fmt.Fprintf(&b, "Changed files: %d, chunks: %d, excluded: %d\n", report.Stats.ChangedFiles, report.Stats.Chunks, len(report.ExcludedFiles))
	b.WriteByte('\n')
	return b.String()
}

func ConsoleResult(report agent.Report, mode string) string {
	var b strings.Builder
	status := "complete"
	if report.Incomplete {
		status = "incomplete"
	}
	fmt.Fprintf(&b, "Review result: findings=%d status=%s\n", len(report.Findings), status)
	fmt.Fprintf(&b, "Review duration: %s\n", formatDuration(report.DurationMS))
	fmt.Fprintf(&b, "Token usage: prompt=%d completion=%d total=%d requests=%d\n", report.Usage.PromptTokens, report.Usage.CompletionTokens, report.Usage.TotalTokens, report.Usage.LLMRequests)
	fmt.Fprintf(&b, "Cache usage: read=%d write=%d\n", report.Usage.CacheReadTokens, report.Usage.CacheWriteTokens)
	fmt.Fprintf(&b, "Tool calls: %d\n", len(report.Process.ToolCalls))
	fmt.Fprintf(&b, "Context compressions: %d\n", len(report.Process.Compressions))
	if report.Incomplete || len(report.Warnings) > 0 || len(report.Findings) > 0 {
		b.WriteByte('\n')
	}
	if report.Incomplete {
		for _, err := range report.Errors {
			fmt.Fprintf(&b, "- %s\n", err)
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintf(&b, "Warnings: %d\n", len(report.Warnings))
		if mode == "detailed" {
			for _, warning := range report.Warnings {
				fmt.Fprintf(&b, "- %s\n", warning)
			}
		}
	}
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := report.Stats.BySeverity[sev]; n > 0 {
			fmt.Fprintf(&b, "%s: %d\n", sev, n)
		}
	}
	if mode == "summary" {
		fmt.Fprintf(&b, "Exit code: %d\n", report.ExitCode)
		return b.String()
	}
	for _, finding := range report.Findings {
		fmt.Fprintf(&b, "\n─── %s:%d-%d ───\n", finding.File, finding.StartLine, finding.EndLine)
		fmt.Fprintf(&b, "[%s · %s] **%s**\n\n", finding.Category, finding.Severity, finding.Title)
		fmt.Fprintf(&b, "%s\n", finding.Problem)
		if finding.Evidence != "" {
			fmt.Fprintf(&b, "\nEvidence: %s\n", finding.Evidence)
		}
		if finding.Suggestion != "" {
			fmt.Fprintf(&b, "\nSuggestion: %s\n", finding.Suggestion)
		}
	}
	if mode == "detailed" && len(report.ExcludedFiles) > 0 {
		b.WriteString("\nExcluded files:\n")
		for _, excluded := range report.ExcludedFiles {
			if excluded.MatchedPattern != "" {
				fmt.Fprintf(&b, "- %s [%s: %s]\n", excluded.Path, excluded.Reason, excluded.MatchedPattern)
			} else {
				fmt.Fprintf(&b, "- %s [%s]\n", excluded.Path, excluded.Reason)
			}
		}
	}
	fmt.Fprintf(&b, "\nExit code: %d\n", report.ExitCode)
	return b.String()
}

func Markdown(report agent.Report) string {
	var b strings.Builder
	b.WriteString("# Code Review Report\n\n")
	fmt.Fprintf(&b, "- Diff mode: `%s`\n", report.Metadata.DiffMode)
	if report.Metadata.From != "" {
		fmt.Fprintf(&b, "- From: `%s`\n", report.Metadata.From)
	}
	if report.Metadata.To != "" {
		fmt.Fprintf(&b, "- To: `%s`\n", report.Metadata.To)
	}
	if report.Metadata.Commit != "" {
		fmt.Fprintf(&b, "- Commit: `%s`\n", report.Metadata.Commit)
	}
	fmt.Fprintf(&b, "- Head: `%s`\n", report.Metadata.Head)
	fmt.Fprintf(&b, "- Protocol: `%s`\n", report.Metadata.Protocol)
	fmt.Fprintf(&b, "- Model: `%s`\n", report.Metadata.Model)
	fmt.Fprintf(&b, "- Language: `%s`\n", report.Metadata.Language)
	if report.Metadata.ReportDir != "" {
		fmt.Fprintf(&b, "- Report dir: `%s`\n", report.Metadata.ReportDir)
	}
	if report.Metadata.JSONReport != "" {
		fmt.Fprintf(&b, "- JSON report: `%s`\n", report.Metadata.JSONReport)
	}
	if report.Metadata.MDReport != "" {
		fmt.Fprintf(&b, "- Markdown report: `%s`\n", report.Metadata.MDReport)
	}
	fmt.Fprintf(&b, "- Changed files: `%d`\n", report.Stats.ChangedFiles)
	fmt.Fprintf(&b, "- Chunks: `%d`\n", report.Stats.Chunks)
	fmt.Fprintf(&b, "- Excluded files: `%d`\n", len(report.ExcludedFiles))
	fmt.Fprintf(&b, "- Exit code: `%d`\n", report.ExitCode)
	fmt.Fprintf(&b, "- Review duration: `%s`\n", formatDuration(report.DurationMS))
	fmt.Fprintf(&b, "- Tool calls: `%d`\n", len(report.Process.ToolCalls))
	fmt.Fprintf(&b, "- Context compressions: `%d`\n", len(report.Process.Compressions))
	if report.Incomplete {
		b.WriteString("- Status: `incomplete`\n")
	}
	b.WriteString("\n## Token Usage\n\n")
	fmt.Fprintf(&b, "- Prompt Tokens: `%d`\n", report.Usage.PromptTokens)
	fmt.Fprintf(&b, "- Completion Tokens: `%d`\n", report.Usage.CompletionTokens)
	fmt.Fprintf(&b, "- Total Tokens: `%d`\n", report.Usage.TotalTokens)
	fmt.Fprintf(&b, "- LLM Requests: `%d`\n", report.Usage.LLMRequests)
	fmt.Fprintf(&b, "- Cache Read Tokens: `%d`\n", report.Usage.CacheReadTokens)
	fmt.Fprintf(&b, "- Cache Write Tokens: `%d`\n", report.Usage.CacheWriteTokens)
	if len(report.Process.ToolCalls) > 0 {
		b.WriteString("\n## Tool Calls\n\n")
		for _, call := range report.Process.ToolCalls {
			fmt.Fprintf(&b, "- `%s` `%s` on `%s` (round %d): `%s`, %s, %d bytes", call.ID, call.Tool, call.File, call.Round, call.Status, formatDuration(call.DurationMS), call.OutputBytes)
			if call.OutputTruncated {
				b.WriteString(", truncated")
			}
			fmt.Fprintf(&b, " - %s\n", call.Summary)
		}
	}
	if len(report.Process.Compressions) > 0 {
		b.WriteString("\n## Context Compressions\n\n")
		for _, compression := range report.Process.Compressions {
			fmt.Fprintf(&b, "- `%s` on `%s` (round %d): `%s`, tokens `%d -> %d`, %s, requests `%d`",
				compression.ID, compression.File, compression.Round, compression.Status,
				compression.BeforeTokens, compression.AfterTokens, formatDuration(compression.DurationMS), compression.Usage.LLMRequests)
			if compression.Error != "" {
				fmt.Fprintf(&b, " - %s", compression.Error)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n## Findings\n\n")
	if len(report.Findings) == 0 {
		b.WriteString("No verified findings.\n")
	} else {
		for _, finding := range report.Findings {
			fmt.Fprintf(&b, "### [%s] %s\n\n", finding.Severity, finding.Title)
			fmt.Fprintf(&b, "- Location: `%s:%d-%d`\n", finding.File, finding.StartLine, finding.EndLine)
			fmt.Fprintf(&b, "- Category: `%s`\n", finding.Category)
			fmt.Fprintf(&b, "- Rule: `%s`\n", finding.RuleID)
			fmt.Fprintf(&b, "- Confidence: `%.2f`\n\n", finding.Confidence)
			fmt.Fprintf(&b, "**Problem:** %s\n\n", finding.Problem)
			fmt.Fprintf(&b, "**Evidence:** %s\n\n", finding.Evidence)
			fmt.Fprintf(&b, "**Suggestion:** %s\n\n", finding.Suggestion)
		}
	}
	if len(report.Errors) > 0 {
		b.WriteString("\n## Errors\n\n")
		for _, err := range report.Errors {
			fmt.Fprintf(&b, "- %s\n", err)
		}
	}
	if len(report.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, warning := range report.Warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}
	if len(report.ResolvedRules) > 0 {
		b.WriteString("\n## Resolved Rules\n\n")
		for _, rule := range report.ResolvedRules {
			fmt.Fprintf(&b, "- `%s`: %s `%s` `%s`\n", rule.File, rule.Source, rule.Pattern, rule.Digest)
		}
	}
	if len(report.ExcludedFiles) > 0 {
		b.WriteString("\n## Excluded Files\n\n")
		for _, excluded := range report.ExcludedFiles {
			if excluded.MatchedPattern != "" {
				fmt.Fprintf(&b, "- `%s`: `%s` via `%s`\n", excluded.Path, excluded.Reason, excluded.MatchedPattern)
			} else {
				fmt.Fprintf(&b, "- `%s`: `%s`\n", excluded.Path, excluded.Reason)
			}
		}
	}
	return b.String()
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func formatDuration(milliseconds int64) string {
	if milliseconds <= 0 {
		return "0s"
	}
	return (time.Duration(milliseconds) * time.Millisecond).String()
}
