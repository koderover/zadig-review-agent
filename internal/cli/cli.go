package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/config"
	"github.com/koderover/zadig-code-review-agent/internal/filter"
	"github.com/koderover/zadig-code-review-agent/internal/gitdiff"
	"github.com/koderover/zadig-code-review-agent/internal/protocol"
	"github.com/koderover/zadig-code-review-agent/internal/reporter"
	"github.com/koderover/zadig-code-review-agent/internal/reviewer"
	"github.com/koderover/zadig-code-review-agent/internal/rules"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		usage(stderr)
		return agent.ExitIncomplete, fmt.Errorf("missing command")
	}
	switch args[0] {
	case "review":
		return runReview(ctx, args[1:], stdout, stderr)
	case "rules":
		return runRules(args[1:], stdout, stderr)
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return agent.ExitOK, nil
	default:
		usage(stderr)
		return agent.ExitIncomplete, fmt.Errorf("unknown command %q", args[0])
	}
}

func runReview(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath(), "path to zadig-review-agent config")
	from := fs.String("from", "", "start ref for range review")
	to := fs.String("to", "", "target ref for range review")
	commit := fs.String("commit", "", "commit sha to review")
	rulePath := fs.String("rule", "", "path to Zadig review rules JSON")
	preview := fs.Bool("preview", false, "print file filtering and rule resolution without invoking the model")
	ciMode := fs.Bool("ci", false, "use CI-friendly console output")
	concurrency := fs.Int("concurrency", 0, "max concurrent file reviews")
	contextLines := fs.Int("context-lines", 0, "context lines around changes")
	maxToolRounds := fs.Int("max-tool-rounds", 0, "max main tool-loop request rounds")
	maxContextToolCalls := fs.Int("max-context-tool-calls", 0, "max read/search context tool calls per file review")
	maxChunkTokens := fs.Int("max-chunk-tokens", 0, "max tokens per review chunk")
	confidenceThreshold := fs.Float64("confidence-threshold", 0, "minimum accepted finding confidence")
	failOn := fs.String("fail-on", "", "comma-separated severities that fail the run")
	language := fs.String("language", "", "language for review finding content, e.g. zh-CN or en-US")
	modelProtocol := fs.String("model-protocol", "", "model protocol: openai, gemini, anthropic")
	modelName := fs.String("model-name", "", "model name")
	modelEndpoint := fs.String("model-endpoint", "", "model endpoint")
	modelTimeout := fs.String("model-timeout", "", "model timeout, e.g. 120s")
	jsonOut := fs.String("output-json", "", "json report path")
	mdOut := fs.String("output-md", "", "markdown report path")
	console := fs.String("console", "", "console output mode: detailed, summary, none")
	progress := fs.Bool("progress", false, "print review progress to stderr (use --progress=false to disable)")
	if err := fs.Parse(args); err != nil {
		return agent.ExitIncomplete, err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	visited := visitedFlags(fs)
	if err := applyCLIOverrides(&cfg, visited, cliOverrides{
		ciMode:              *ciMode,
		concurrency:         *concurrency,
		contextLines:        *contextLines,
		maxToolRounds:       *maxToolRounds,
		maxContextToolCalls: *maxContextToolCalls,
		maxChunkTokens:      *maxChunkTokens,
		confidenceThreshold: *confidenceThreshold,
		failOn:              *failOn,
		language:            *language,
		modelProtocol:       *modelProtocol,
		modelName:           *modelName,
		modelEndpoint:       *modelEndpoint,
		modelTimeout:        *modelTimeout,
		jsonOut:             *jsonOut,
		mdOut:               *mdOut,
		console:             *console,
		progress:            *progress,
	}); err != nil {
		return agent.ExitIncomplete, err
	}
	gitClient := gitdiff.Client{Dir: "."}
	repoRoot, err := gitClient.Root(ctx)
	if err != nil {
		return agent.ExitIncomplete, fmt.Errorf("git root: %w", err)
	}
	gitClient.Dir = repoRoot
	resolver, err := rules.NewResolver(repoRoot, *rulePath)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	diffReq, err := buildDiffRequest(*from, *to, *commit)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	diffReq.ContextLines = cfg.Review.ContextLines
	diffReq.ContextLinesConfigured = true
	progressLog := newProgressLogger(stderr, cfg.Output.Progress)
	if *preview {
		started := time.Now()
		report, err := previewReport(ctx, cfg, resolver, diffReq, repoRoot)
		if err != nil {
			return agent.ExitIncomplete, err
		}
		if err := prepareReportOutput(&report, &cfg); err != nil {
			return agent.ExitIncomplete, err
		}
		if cfg.Output.Progress {
			_, _ = io.WriteString(stderr, reporter.ConsoleStart(report))
		}
		report.DurationMS = durationMilliseconds(time.Since(started))
		writeReport := reporter.Write
		if cfg.Output.Progress {
			writeReport = reporter.WriteResult
		}
		progressLog("preview completed kept=%d excluded=%d duration=%s", report.Stats.ChangedFiles, len(report.ExcludedFiles), time.Duration(report.DurationMS)*time.Millisecond)
		if err := writeReport(report, cfg.Output.JSON, cfg.Output.Markdown, stdout, cfg.Output.Console); err != nil {
			return agent.ExitIncomplete, err
		}
		return report.ExitCode, nil
	}
	if config.NeedsModelConfiguration(cfg) {
		return agent.ExitIncomplete, fmt.Errorf("%s", config.ModelConfigurationHint(*configPath))
	}
	llm, err := protocol.NewRegistry().Build(cfg.Model)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	run := reviewer.Runner{
		Root:         repoRoot,
		Config:       cfg,
		Git:          gitClient,
		LLM:          llm,
		RuleResolver: resolver,
		DiffRequest:  diffReq,
		Progress:     progressLog,
		Started: func(report agent.Report) {
			if cfg.Output.Progress {
				_, _ = io.WriteString(stderr, reporter.ConsoleStart(report))
			}
		},
	}
	report, err := run.Run(ctx)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	if err := prepareReportOutput(&report, &cfg); err != nil {
		return agent.ExitIncomplete, err
	}
	writeReport := reporter.Write
	if cfg.Output.Progress {
		writeReport = reporter.WriteResult
	}
	if err := writeReport(report, cfg.Output.JSON, cfg.Output.Markdown, stdout, cfg.Output.Console); err != nil {
		return agent.ExitIncomplete, err
	}
	return report.ExitCode, nil
}

func runRules(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(stderr, "usage: zadig-review-agent rules check <path> [--rule rules.json]")
		return agent.ExitIncomplete, fmt.Errorf("unknown rules command")
	}
	fs := flag.NewFlagSet("rules check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rulePath := fs.String("rule", "", "path to Zadig review rules JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return agent.ExitIncomplete, err
	}
	if fs.NArg() != 1 {
		return agent.ExitIncomplete, fmt.Errorf("rules check requires exactly one path")
	}
	resolver, err := rules.NewResolver(".", *rulePath)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	for _, warning := range resolver.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	file := filepath.ToSlash(fs.Arg(0))
	rule := resolver.Resolve(file)
	fmt.Fprintf(stdout, "File: %s\n", file)
	fmt.Fprintf(stdout, "Source: %s\n", rule.Source)
	if rule.SourcePath != "" {
		fmt.Fprintf(stdout, "SourcePath: %s\n", rule.SourcePath)
	}
	fmt.Fprintf(stdout, "Pattern: %s\n", rule.Pattern)
	fmt.Fprintf(stdout, "Digest: %s\n", rule.Digest)
	fmt.Fprintln(stdout, "Rule:")
	fmt.Fprintln(stdout, "────────────────────────────────────────")
	fmt.Fprintln(stdout, rule.Rule)
	fmt.Fprintln(stdout, "────────────────────────────────────────")
	return agent.ExitOK, nil
}

func runConfig(args []string, stdout, stderr io.Writer) (int, error) {
	configPath, rest, err := parseConfigArgs(args)
	if err != nil {
		return agent.ExitIncomplete, err
	}
	if len(rest) == 0 {
		configUsage(stdout, configPath)
		return agent.ExitOK, nil
	}
	if rest[0] == "help" && len(rest) == 2 {
		configSubcommandUsage(stdout, rest[1], configPath)
		return agent.ExitOK, nil
	}
	if isHelpArg(rest[0]) {
		configUsage(stdout, configPath)
		return agent.ExitOK, nil
	}
	if len(rest) > 1 && isHelpArg(rest[1]) {
		configSubcommandUsage(stdout, rest[0], configPath)
		return agent.ExitOK, nil
	}
	if len(rest) > 2 && isHelpArg(rest[2]) {
		configSubcommandUsage(stdout, rest[0], configPath)
		return agent.ExitOK, nil
	}
	switch rest[0] {
	case "path":
		if len(rest) != 1 {
			return agent.ExitIncomplete, fmt.Errorf("config path does not accept arguments")
		}
		fmt.Fprintln(stdout, configPath)
		return agent.ExitOK, nil
	case "show", "list":
		if len(rest) != 1 {
			return agent.ExitIncomplete, fmt.Errorf("config %s does not accept arguments", rest[0])
		}
		cfg, err := config.LoadFile(configPath)
		if err != nil {
			return agent.ExitIncomplete, err
		}
		fmt.Fprint(stdout, config.RenderRedacted(cfg))
		return agent.ExitOK, nil
	case "get":
		if len(rest) != 2 {
			return agent.ExitIncomplete, fmt.Errorf("usage: zadig-review-agent config get <key> [--config path]")
		}
		cfg, err := config.LoadFile(configPath)
		if err != nil {
			return agent.ExitIncomplete, err
		}
		value, err := config.Get(cfg, rest[1])
		if err != nil {
			return agent.ExitIncomplete, err
		}
		fmt.Fprintln(stdout, value)
		return agent.ExitOK, nil
	case "set":
		if len(rest) < 3 {
			return agent.ExitIncomplete, fmt.Errorf("usage: zadig-review-agent config set <key> <value> [--config path]")
		}
		cfg, err := config.LoadFile(configPath)
		if err != nil {
			return agent.ExitIncomplete, err
		}
		value := strings.Join(rest[2:], " ")
		if err := config.Set(&cfg, rest[1], value); err != nil {
			return agent.ExitIncomplete, err
		}
		if err := config.Save(configPath, cfg); err != nil {
			return agent.ExitIncomplete, err
		}
		fmt.Fprintf(stdout, "Wrote %s\n", configPath)
		return agent.ExitOK, nil
	default:
		configUsage(stderr, configPath)
		return agent.ExitIncomplete, fmt.Errorf("unknown config command %q", rest[0])
	}
}

func isHelpArg(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func configUsage(w io.Writer, configPath string) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  zadig-review-agent config path [--config path]")
	fmt.Fprintln(w, "  zadig-review-agent config show [--config path]")
	fmt.Fprintln(w, "  zadig-review-agent config get <key> [--config path]")
	fmt.Fprintln(w, "  zadig-review-agent config set <key> <value> [--config path]")
	fmt.Fprintf(w, "\ndefault config: %s\n", configPath)
	fmt.Fprintln(w, "\nsettable keys:")
	printConfigKeys(w)
}

func configSubcommandUsage(w io.Writer, subcommand, configPath string) {
	switch subcommand {
	case "set":
		fmt.Fprintln(w, "usage: zadig-review-agent config set <key> <value> [--config path]")
		fmt.Fprintf(w, "\ndefault config: %s\n", configPath)
		fmt.Fprintln(w, "\nexamples:")
		fmt.Fprintln(w, "  zadig-review-agent config set model.protocol openai")
		fmt.Fprintln(w, "  zadig-review-agent config set model.name gpt-4o")
		fmt.Fprintln(w, "  zadig-review-agent config set model.endpoint https://api.openai.com/v1")
		fmt.Fprintln(w, "  zadig-review-agent config set model.api_key sk-...")
		fmt.Fprintln(w, "  zadig-review-agent config set review.fail_on critical,high")
		fmt.Fprintln(w, "  zadig-review-agent config set output.language zh-CN")
		fmt.Fprintln(w, "\nsettable keys:")
		printConfigKeys(w)
	case "get":
		fmt.Fprintln(w, "usage: zadig-review-agent config get <key> [--config path]")
		fmt.Fprintf(w, "\ndefault config: %s\n", configPath)
		fmt.Fprintln(w, "\nreadable keys:")
		printConfigKeys(w)
	case "show", "list":
		fmt.Fprintln(w, "usage: zadig-review-agent config show [--config path]")
		fmt.Fprintf(w, "\ndefault config: %s\n", configPath)
	case "path":
		fmt.Fprintln(w, "usage: zadig-review-agent config path [--config path]")
		fmt.Fprintf(w, "\ndefault config: %s\n", configPath)
	default:
		configUsage(w, configPath)
	}
}

func printConfigKeys(w io.Writer) {
	for _, key := range config.Keys() {
		fmt.Fprintf(w, "  %-28s %-28s %s\n", key.Key, key.ValueHint, key.Description)
	}
}

func parseConfigArgs(args []string) (string, []string, error) {
	path := config.DefaultPath()
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--config":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--config requires a path")
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(arg, "--config="):
			path = strings.TrimPrefix(arg, "--config=")
			if path == "" {
				return "", nil, fmt.Errorf("--config requires a path")
			}
		default:
			rest = append(rest, arg)
		}
	}
	return path, rest, nil
}

func buildDiffRequest(from, to, commit string) (gitdiff.Request, error) {
	if commit != "" && (from != "" || to != "") {
		return gitdiff.Request{}, fmt.Errorf("--commit cannot be used with --from/--to")
	}
	if commit != "" {
		return gitdiff.Request{Mode: gitdiff.ModeCommit, Commit: commit}, nil
	}
	if from != "" || to != "" {
		if from == "" || to == "" {
			return gitdiff.Request{}, fmt.Errorf("--from and --to must be used together")
		}
		return gitdiff.Request{Mode: gitdiff.ModeRange, From: from, To: to}, nil
	}
	return gitdiff.Request{Mode: gitdiff.ModeWorkspace}, nil
}

func previewReport(ctx context.Context, cfg config.Config, resolver rules.Resolver, diffReq gitdiff.Request, root string) (agent.Report, error) {
	client := gitdiff.Client{Dir: root}
	head, err := client.Head(ctx)
	if err != nil {
		return agent.Report{}, fmt.Errorf("git head: %w", err)
	}
	files, err := client.Diff(ctx, diffReq)
	if err != nil {
		return agent.Report{}, err
	}
	filtered := filter.Apply(files, filter.Options{RuleFile: resolver.FilterFile})
	report := agent.Report{
		Metadata: agent.Metadata{
			DiffMode:   string(diffReq.Mode),
			From:       diffReq.From,
			To:         diffReq.To,
			Commit:     diffReq.Commit,
			Head:       head,
			Protocol:   cfg.Model.Protocol,
			Model:      cfg.Model.Name,
			Repository: repositoryPath(root),
			Language:   cfg.Output.Language,
		},
		Stats: agent.Stats{
			ChangedFiles: len(filtered.Kept),
			Chunks:       len(filtered.Kept),
			BySeverity:   map[string]int{},
		},
		ExcludedFiles: filtered.Excluded,
		Warnings:      append([]string(nil), resolver.Warnings...),
		Process:       agent.ReviewProcess{ToolCalls: []agent.ToolCall{}},
		ExitCode:      agent.ExitOK,
	}
	for _, file := range filtered.Kept {
		rule := resolver.Resolve(file.Path)
		report.ResolvedRules = append(report.ResolvedRules, agent.ResolvedRule{
			File:       file.Path,
			Source:     rule.Source,
			SourcePath: rule.SourcePath,
			Pattern:    rule.Pattern,
			Digest:     rule.Digest,
		})
	}
	return report, nil
}

func prepareReportOutput(report *agent.Report, cfg *config.Config) error {
	runDir := reportRunDir(report.Metadata)
	jsonPath, err := resolveReportPath(cfg.Output.JSON, runDir)
	if err != nil {
		return err
	}
	markdownPath, err := resolveReportPath(cfg.Output.Markdown, runDir)
	if err != nil {
		return err
	}
	cfg.Output.JSON = jsonPath
	cfg.Output.Markdown = markdownPath
	report.Metadata.ReportDir = runDir
	report.Metadata.JSONReport = jsonPath
	report.Metadata.MDReport = markdownPath
	if jsonPath != "" || markdownPath != "" {
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			return fmt.Errorf("create report dir: %w", err)
		}
	}
	return nil
}

func resolveReportPath(path, runDir string) (string, error) {
	if path == "" {
		return "", nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("expand %s: home directory is unavailable", path)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Join(runDir, path), nil
}

func reportRunDir(metadata agent.Metadata) string {
	return availableReportRunDir(reportRunDirAt(metadata, time.Now()))
}

func reportRunDirAt(metadata agent.Metadata, now time.Time) string {
	label := "review"
	switch metadata.DiffMode {
	case string(gitdiff.ModeCommit):
		label = "commit-" + shortForPath(metadata.Commit)
	case string(gitdiff.ModeRange):
		label = shortForPath(metadata.From) + "-to-" + shortForPath(metadata.To)
	case string(gitdiff.ModeWorkspace):
		label = "workspace"
	}
	id := now.UTC().Format("20060102T150405Z")
	repository := sanitizePathLabel(metadata.Repository)
	if repository == "review" {
		repository = "repository"
	}
	return filepath.Join(config.DefaultReportRoot(), repository, id+"-"+sanitizePathLabel(label))
}

func availableReportRunDir(preferred string) string {
	if _, err := os.Stat(preferred); err != nil {
		return preferred
	}
	for index := 2; ; index++ {
		candidate := fmt.Sprintf("%s-%d", preferred, index)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
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

func shortForPath(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	if value == "" {
		return "unknown"
	}
	return value
}

func sanitizePathLabel(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		keep := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if keep {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "review"
	}
	return out
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  zadig-review-agent review [--from origin/main --to HEAD | --commit <sha>] [--config ~/.zadig-review-agent/config.yaml] [--rule rules.json] [--preview] [--progress]")
	fmt.Fprintln(w, "  zadig-review-agent config set <key> <value> [--config ~/.zadig-review-agent/config.yaml]")
	fmt.Fprintln(w, "  zadig-review-agent config get <key> [--config ~/.zadig-review-agent/config.yaml]")
	fmt.Fprintln(w, "  zadig-review-agent config show [--config ~/.zadig-review-agent/config.yaml]")
	fmt.Fprintln(w, "  zadig-review-agent rules check <path> [--rule rules.json]")
}

type cliOverrides struct {
	ciMode              bool
	concurrency         int
	contextLines        int
	maxToolRounds       int
	maxContextToolCalls int
	maxChunkTokens      int
	confidenceThreshold float64
	failOn              string
	language            string
	modelProtocol       string
	modelName           string
	modelEndpoint       string
	modelTimeout        string
	jsonOut             string
	mdOut               string
	console             string
	progress            bool
}

func visitedFlags(fs *flag.FlagSet) map[string]bool {
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		visited[f.Name] = true
	})
	return visited
}

func applyCLIOverrides(cfg *config.Config, visited map[string]bool, o cliOverrides) error {
	if visited["ci"] && o.ciMode {
		cfg.Output.Console = "summary"
	}
	if visited["concurrency"] {
		if o.concurrency < 1 {
			return fmt.Errorf("--concurrency must be a positive integer")
		}
		cfg.Review.Concurrency = o.concurrency
	}
	if visited["context-lines"] {
		if o.contextLines < 0 {
			return fmt.Errorf("--context-lines must be a non-negative integer")
		}
		cfg.Review.ContextLines = o.contextLines
	}
	if visited["max-tool-rounds"] {
		if o.maxToolRounds < 1 {
			return fmt.Errorf("--max-tool-rounds must be a positive integer")
		}
		cfg.Review.MaxToolRounds = o.maxToolRounds
	}
	if visited["max-context-tool-calls"] {
		if o.maxContextToolCalls < 1 {
			return fmt.Errorf("--max-context-tool-calls must be a positive integer")
		}
		cfg.Review.MaxContextToolCalls = o.maxContextToolCalls
	}
	if visited["max-chunk-tokens"] {
		if o.maxChunkTokens < 1000 {
			return fmt.Errorf("--max-chunk-tokens must be at least 1000")
		}
		cfg.Review.MaxChunkTokens = o.maxChunkTokens
	}
	if visited["confidence-threshold"] {
		if o.confidenceThreshold < 0 || o.confidenceThreshold > 1 {
			return fmt.Errorf("--confidence-threshold must be between 0 and 1")
		}
		cfg.Review.ConfidenceThreshold = o.confidenceThreshold
	}
	if visited["fail-on"] {
		values := splitCSV(o.failOn)
		if len(values) == 0 {
			return fmt.Errorf("--fail-on must contain at least one severity")
		}
		for _, severity := range values {
			if !validSeverity(severity) {
				return fmt.Errorf("--fail-on contains invalid severity %q", severity)
			}
		}
		cfg.Review.FailOn = values
	}
	if visited["language"] {
		if strings.TrimSpace(o.language) == "" {
			return fmt.Errorf("--language must not be empty")
		}
		cfg.Output.Language = strings.TrimSpace(o.language)
	}
	if visited["model-protocol"] {
		cfg.Model.Protocol = o.modelProtocol
	}
	if visited["model-name"] {
		cfg.Model.Name = o.modelName
	}
	if visited["model-endpoint"] {
		cfg.Model.Endpoint = strings.TrimRight(o.modelEndpoint, "/")
	}
	if visited["model-timeout"] {
		d, err := time.ParseDuration(o.modelTimeout)
		if err != nil {
			return fmt.Errorf("--model-timeout must be a Go duration: %w", err)
		}
		cfg.Model.Timeout = d
	}
	if visited["output-json"] {
		cfg.Output.JSON = o.jsonOut
	}
	if visited["output-md"] {
		cfg.Output.Markdown = o.mdOut
	}
	if visited["console"] {
		if !validConsoleMode(o.console) {
			return fmt.Errorf("--console must be one of detailed, summary, none")
		}
		cfg.Output.Console = o.console
	}
	if visited["progress"] {
		cfg.Output.Progress = o.progress
	}
	return nil
}

func newProgressLogger(w io.Writer, enabled bool) func(string, ...any) {
	if !enabled || w == nil {
		return func(string, ...any) {}
	}
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "[progress] "+format+"\n", args...)
	}
}

func durationMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	if duration < time.Millisecond {
		return 1
	}
	return duration.Milliseconds()
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func validSeverity(severity string) bool {
	switch severity {
	case "critical", "high", "medium", "low":
		return true
	default:
		return false
	}
}

func validConsoleMode(mode string) bool {
	switch mode {
	case "detailed", "summary", "none":
		return true
	default:
		return false
	}
}
