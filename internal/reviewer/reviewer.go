package reviewer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/config"
	"github.com/koderover/zadig-code-review-agent/internal/filter"
	"github.com/koderover/zadig-code-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-code-review-agent/internal/protocol"
	"github.com/koderover/zadig-code-review-agent/internal/rules"
)

type Runner struct {
	Root          string
	Config        config.Config
	Git           GitClient
	LLM           protocol.LLM
	RuleResolver  rules.Resolver
	DiffRequest   gitdiff.Request
	Now           func() time.Time
	HeadSHA       string
	Progress      func(string, ...any)
	Started       func(agent.Report)
	process       *processRecorder
	showFileLabel bool
}

type GitClient interface {
	Head(context.Context) (string, error)
	Diff(context.Context, gitdiff.Request) ([]gitdiff.FileDiff, error)
}

func (r Runner) Run(ctx context.Context) (report agent.Report, runErr error) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	r.process = newProcessRecorder(started)
	defer func() {
		report.DurationMS = elapsedMilliseconds(now().Sub(started))
		report.Process = r.process.snapshot()
	}()
	head := r.HeadSHA
	if head == "" && r.Git != nil {
		var err error
		head, err = r.Git.Head(ctx)
		if err != nil {
			return incomplete(r, head, fmt.Errorf("git head: %w", err)), nil
		}
		head = strings.TrimSpace(head)
	}
	files, err := r.Git.Diff(ctx, r.DiffRequest)
	if err != nil {
		return incomplete(r, head, err), nil
	}
	filtered := filter.Apply(files, filter.Options{RuleFile: r.RuleResolver.FilterFile})
	chunks := fileChunks(filtered.Kept, r.Config.Review.MaxChunkTokens)
	r.showFileLabel = len(chunks) > 1

	report = agent.Report{
		Metadata: agent.Metadata{
			DiffMode:   string(normalizeMode(r.DiffRequest.Mode)),
			From:       r.DiffRequest.From,
			To:         r.DiffRequest.To,
			Commit:     r.DiffRequest.Commit,
			Head:       head,
			Protocol:   r.Config.Model.Protocol,
			Model:      r.Config.Model.Name,
			Zadig:      isZadig(),
			Repository: repositoryPath(r.Root),
			Language:   r.Config.Output.Language,
		},
		Stats: agent.Stats{
			ChangedFiles: len(filtered.Kept),
			Chunks:       len(chunks),
			BySeverity:   map[string]int{},
		},
		ExcludedFiles: filtered.Excluded,
		Warnings:      append([]string(nil), r.RuleResolver.Warnings...),
	}
	if r.Started != nil {
		r.Started(report)
	}
	if len(chunks) == 0 {
		report.ExitCode = agent.ExitOK
		r.trace("Review completed (%s)", formatElapsed(now().Sub(started)))
		return report, nil
	}

	jobs := make(chan reviewJob)
	results := make(chan chunkResult)
	workers := r.Config.Review.Concurrency
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				started := time.Now()
				r.trace("Reviewing %d/%d: %s", job.index, job.total, job.file.Path)
				findings, usage, warnings, err := r.reviewFile(ctx, job.file, job.rule, filtered.Kept)
				r.trace("%scompleted: %d finding(s) (%s)", r.progressFilePrefix(job.file.Path), len(findings), time.Since(started).Round(time.Millisecond))
				results <- chunkResult{findings: findings, rule: job.ruleMeta, usage: usage, warnings: warnings, err: err}
			}
		}()
	}
	go func() {
	loop:
		for index, chunk := range chunks {
			resolved := r.RuleResolver.Resolve(chunk.Path)
			select {
			case <-ctx.Done():
				break loop
			case jobs <- reviewJob{
				file:  chunk,
				index: index + 1,
				total: len(chunks),
				rule:  resolved,
				ruleMeta: agent.ResolvedRule{
					File:       chunk.Path,
					Source:     resolved.Source,
					SourcePath: resolved.SourcePath,
					Pattern:    resolved.Pattern,
					Digest:     resolved.Digest,
				},
			}:
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var all []agent.Finding
	for result := range results {
		report.Usage.Add(result.usage)
		report.Warnings = append(report.Warnings, result.warnings...)
		for _, warning := range result.warnings {
			if warningMakesIncomplete(warning) {
				report.Incomplete = true
			}
		}
		if result.rule.File != "" {
			report.ResolvedRules = append(report.ResolvedRules, result.rule)
		}
		if result.err != nil {
			report.Incomplete = true
			report.Errors = append(report.Errors, result.err.Error())
			continue
		}
		all = append(all, result.findings...)
	}
	report.Findings = aggregate(all)
	for _, finding := range report.Findings {
		report.Stats.BySeverity[finding.Severity]++
	}
	if ctx.Err() != nil {
		report.Incomplete = true
		report.Errors = append(report.Errors, ctx.Err().Error())
		report.ExitCode = agent.ExitCanceled
		return report, nil
	}
	report.ExitCode = DecideExit(report, r.Config.Review.FailOn)
	r.trace("Review completed (%s)", formatElapsed(now().Sub(started)))
	return report, nil
}

func elapsedMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	if duration < time.Millisecond {
		return 1
	}
	return duration.Milliseconds()
}

func formatElapsed(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	return duration.Round(time.Millisecond)
}

func fileLabel(path string) string {
	label := filepath.Base(path)
	if label == "." || label == "" {
		return path
	}
	return label
}

func (r Runner) progressFilePrefix(path string) string {
	if !r.showFileLabel {
		return ""
	}
	return "[" + fileLabel(path) + "] "
}

func (r Runner) trace(format string, args ...any) {
	if r.Progress != nil {
		r.Progress(format, args...)
	}
}

func warningMakesIncomplete(warning string) bool {
	return strings.HasPrefix(warning, "token_threshold_exceeded:") ||
		strings.HasPrefix(warning, "context_request_ignored_at_limit:") ||
		strings.HasPrefix(warning, "tool_loop_empty_limit_reached:") ||
		strings.HasPrefix(warning, "tool_loop_limit_reached:") ||
		strings.HasPrefix(warning, "review_filter_failed:") ||
		strings.HasPrefix(warning, "review_filter_invalid_response:") ||
		strings.HasPrefix(warning, "finding_localization_failed:") ||
		strings.HasPrefix(warning, "relocation_failed:") ||
		strings.HasPrefix(warning, "relocation_invalid_response:")
}

type chunkResult struct {
	findings []agent.Finding
	rule     agent.ResolvedRule
	usage    agent.TokenUsage
	warnings []string
	err      error
}

func repositoryPath(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "repository"
	}
	abs, err := filepath.Abs(root)
	if err == nil {
		root = abs
	}
	root = filepath.Clean(root)
	if root == "." || root == "" {
		return "repository"
	}
	return root
}

type reviewJob struct {
	file     gitdiff.FileDiff
	index    int
	total    int
	rule     rules.ResolvedRule
	ruleMeta agent.ResolvedRule
}

func (r Runner) reviewFile(ctx context.Context, file gitdiff.FileDiff, rule rules.ResolvedRule, allFiles []gitdiff.FileDiff) ([]agent.Finding, agent.TokenUsage, []string, error) {
	return r.runSubtask(ctx, file, rule, allFiles)
}

func fileChunks(files []gitdiff.FileDiff, maxTokens int) []gitdiff.FileDiff {
	if maxTokens < 1000 {
		maxTokens = 12000
	}
	target := maxTokens * 7 / 10
	var chunks []gitdiff.FileDiff
	for _, file := range files {
		if estimateTokens(renderFileDiff(file)) <= target || len(file.Hunks) == 0 {
			chunks = append(chunks, file)
			continue
		}
		for _, hunk := range file.Hunks {
			for _, part := range splitHunk(hunk, target) {
				chunk := file
				chunk.Hunks = []gitdiff.Hunk{part}
				chunk.Insertions, chunk.Deletions = countChanges(part.Lines)
				chunks = append(chunks, chunk)
			}
		}
	}
	return chunks
}

func splitHunk(hunk gitdiff.Hunk, targetTokens int) []gitdiff.Hunk {
	var parts []gitdiff.Hunk
	var lines []gitdiff.Line
	tokens := 0
	flush := func() {
		if len(lines) == 0 {
			return
		}
		parts = append(parts, buildHunk(lines))
		lines = nil
		tokens = 0
	}
	for _, line := range hunk.Lines {
		lineTokens := estimateTokens(line.Text) + 4
		if len(lines) > 0 && tokens+lineTokens > targetTokens {
			flush()
		}
		lines = append(lines, line)
		tokens += lineTokens
	}
	flush()
	return parts
}

func buildHunk(lines []gitdiff.Line) gitdiff.Hunk {
	hunk := gitdiff.Hunk{ChangedLines: map[int]bool{}, Lines: append([]gitdiff.Line(nil), lines...)}
	for _, line := range lines {
		if hunk.OldStart == 0 && line.OldLine > 0 {
			hunk.OldStart = line.OldLine
		}
		if hunk.NewStart == 0 && line.NewLine > 0 {
			hunk.NewStart = line.NewLine
		}
		if line.Kind != '+' {
			hunk.OldLines++
		}
		if line.Kind != '-' {
			hunk.NewLines++
		}
		if line.Kind == '+' && line.NewLine > 0 {
			hunk.ChangedLines[line.NewLine] = true
		}
	}
	return hunk
}

func countChanges(lines []gitdiff.Line) (int, int) {
	var insertions, deletions int
	for _, line := range lines {
		if line.Kind == '+' {
			insertions++
		} else if line.Kind == '-' {
			deletions++
		}
	}
	return insertions, deletions
}

func estimateTokens(text string) int {
	runes := len([]rune(text))
	return (runes + 3) / 4
}

func renderFileDiff(file gitdiff.FileDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, "file: %s\n", file.Path)
	for _, hunk := range file.Hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", hunk.OldStart, hunk.OldLines, hunk.NewStart, hunk.NewLines)
		for _, line := range hunk.Lines {
			switch line.Kind {
			case '+':
				fmt.Fprintf(&b, "+%d: %s\n", line.NewLine, line.Text)
			case '-':
				fmt.Fprintf(&b, "-%d: %s\n", line.OldLine, line.Text)
			default:
				fmt.Fprintf(&b, " %d: %s\n", line.NewLine, line.Text)
			}
		}
	}
	return b.String()
}

func parseFindings(text string) ([]agent.Finding, error) {
	data := extractJSONValue(text)
	var wrapper struct {
		Findings json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Findings) > 0 {
		var findings []agent.Finding
		if err := json.Unmarshal(wrapper.Findings, &findings); err != nil {
			return nil, fmt.Errorf("model response has invalid findings: %w", err)
		}
		return findings, nil
	}
	var direct []agent.Finding
	if err := json.Unmarshal(data, &direct); err != nil {
		return nil, fmt.Errorf("model response is not valid findings JSON: %w", err)
	}
	return direct, nil
}

func validateFindings(candidates []agent.Finding, file gitdiff.FileDiff, rule rules.ResolvedRule, threshold float64) ([]agent.Finding, error) {
	var out []agent.Finding
	for _, finding := range candidates {
		finding.File = strings.TrimSpace(finding.File)
		finding.Severity = strings.ToLower(strings.TrimSpace(finding.Severity))
		finding.Category = normalizeCategory(finding.Category)
		if finding.File != file.Path {
			continue
		}
		if !validSeverity(finding.Severity) || !validCategory(finding.Category) {
			continue
		}
		if finding.Confidence < threshold {
			continue
		}
		if finding.StartLine <= 0 || finding.EndLine < finding.StartLine {
			continue
		}
		if !overlapsChangedLine(file, finding.StartLine, finding.EndLine) {
			continue
		}
		if finding.RuleID == "" {
			finding.RuleID = rule.Source + ":" + rule.Pattern
		}
		finding.Fingerprint = fingerprint(finding)
		out = append(out, finding)
	}
	return out, nil
}

func normalizeCategory(category string) string {
	normalized := strings.ToLower(strings.TrimSpace(category))
	normalized = strings.NewReplacer("-", " ", "_", " ").Replace(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")
	switch normalized {
	case "bug", "correctness", "error handling", "reliability", "resource management":
		return "correctness"
	case "security":
		return "security"
	case "concurrency":
		return "concurrency"
	case "performance":
		return "performance"
	case "compatibility":
		return "compatibility"
	case "test", "testing", "tests", "test coverage":
		return "tests"
	default:
		return normalized
	}
}

func overlapsChangedLine(file gitdiff.FileDiff, start, end int) bool {
	for _, hunk := range file.Hunks {
		for line := range hunk.ChangedLines {
			if line >= start && line <= end {
				return true
			}
		}
	}
	return false
}

func validSeverity(severity string) bool {
	switch severity {
	case "critical", "high", "medium", "low":
		return true
	default:
		return false
	}
}

func validCategory(category string) bool {
	switch category {
	case "correctness", "security", "concurrency", "performance", "compatibility", "tests":
		return true
	default:
		return false
	}
}

func aggregate(findings []agent.Finding) []agent.Finding {
	seen := map[string]bool{}
	var out []agent.Finding
	for _, finding := range findings {
		if seen[finding.Fingerprint] {
			continue
		}
		seen[finding.Fingerprint] = true
		out = append(out, finding)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].StartLine < out[j].StartLine
	})
	return out
}

func fingerprint(f agent.Finding) string {
	title := strings.ToLower(strings.Join(strings.Fields(f.Title+" "+f.Problem), " "))
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%s:%s", f.File, f.StartLine, f.EndLine, f.Category, title)))
	return hex.EncodeToString(sum[:12])
}

func DecideExit(report agent.Report, failOn []string) int {
	if report.Incomplete {
		return agent.ExitIncomplete
	}
	block := map[string]bool{}
	for _, severity := range failOn {
		block[severity] = true
	}
	for _, finding := range report.Findings {
		if block[finding.Severity] {
			return agent.ExitBlocked
		}
	}
	return agent.ExitOK
}

func incomplete(r Runner, head string, err error) agent.Report {
	return agent.Report{
		Metadata: agent.Metadata{
			DiffMode:   string(normalizeMode(r.DiffRequest.Mode)),
			From:       r.DiffRequest.From,
			To:         r.DiffRequest.To,
			Commit:     r.DiffRequest.Commit,
			Head:       head,
			Protocol:   r.Config.Model.Protocol,
			Model:      r.Config.Model.Name,
			Zadig:      isZadig(),
			Repository: repositoryPath(r.Root),
			Language:   r.Config.Output.Language,
		},
		Stats:      agent.Stats{BySeverity: map[string]int{}},
		Incomplete: true,
		Errors:     []string{err.Error()},
		ExitCode:   agent.ExitIncomplete,
	}
}

func normalizeMode(mode gitdiff.Mode) gitdiff.Mode {
	if mode == "" {
		return gitdiff.ModeWorkspace
	}
	return mode
}

func isZadig() bool {
	for _, key := range []string{"ZADIG_WORKFLOW_NAME", "ZADIG_TASK_ID", "ZADIG_PROJECT"} {
		if strings.TrimSpace(getenv(key)) != "" {
			return true
		}
	}
	return false
}

var getenv = os.Getenv
