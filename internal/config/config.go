package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Review ReviewConfig
	Model  ModelConfig
	Output OutputConfig
}

type ReviewConfig struct {
	Concurrency         int
	ContextLines        int
	MaxToolRounds       int
	MaxContextToolCalls int
	MaxChunkTokens      int
	ConfidenceThreshold float64
	FailOn              []string
}

type ModelConfig struct {
	Protocol string
	Name     string
	Endpoint string
	APIKey   string
	Timeout  time.Duration
}

type OutputConfig struct {
	JSON     string
	Markdown string
	Console  string
	Language string
	Progress bool
}

type KeyInfo struct {
	Key         string
	ValueHint   string
	Description string
}

func Keys() []KeyInfo {
	return []KeyInfo{
		{"review.concurrency", "4", "Maximum concurrent file reviews."},
		{"review.context_lines", "3", "Context lines around changed lines."},
		{"review.max_tool_rounds", "30", "Maximum main tool-loop request rounds."},
		{"review.max_context_tool_calls", "10", "Maximum read/search context tool calls per file review."},
		{"review.max_chunk_tokens", "12000", "Maximum approximate tokens per review chunk."},
		{"review.confidence_threshold", "0.75", "Minimum accepted finding confidence, 0..1."},
		{"review.fail_on", "critical,high", "Comma-separated severities that fail the run."},
		{"model.protocol", "openai", "Model protocol: openai, gemini, anthropic."},
		{"model.name", "gpt-4o", "Model name for the selected protocol."},
		{"model.endpoint", "https://api.openai.com/v1", "Base endpoint for the selected protocol."},
		{"model.api_key", "sk-...", "Model API key. Prefer ZADIG_REVIEW_MODEL_API_KEY in CI."},
		{"model.timeout", "120s", "Model request timeout as a Go duration."},
		{"output.json", "review-report.json", "JSON report output path. Relative paths are written under each review report dir."},
		{"output.markdown", "review-report.md", "Markdown report output path. Relative paths are written under each review report dir."},
		{"output.console", "detailed", "Console output mode: detailed, summary, none."},
		{"output.language", "zh-CN", "Language for human-readable review finding content."},
		{"output.progress", "true", "Print review progress to stderr."},
	}
}

func Default() Config {
	return Config{
		Review: ReviewConfig{
			Concurrency:         4,
			ContextLines:        3,
			MaxToolRounds:       30,
			MaxContextToolCalls: 10,
			MaxChunkTokens:      12000,
			ConfidenceThreshold: 0.75,
			FailOn:              []string{"critical", "high"},
		},
		Model: ModelConfig{
			Protocol: "openai",
			Name:     "configured-model",
			Endpoint: "https://api.openai.com/v1",
			Timeout:  120 * time.Second,
		},
		Output: OutputConfig{
			JSON:     "review-report.json",
			Markdown: "review-report.md",
			Console:  "detailed",
			Language: "zh-CN",
			Progress: true,
		},
	}
}

func Load(path string) (Config, error) {
	return load(path, true)
}

func LoadFile(path string) (Config, error) {
	return load(path, false)
}

func load(path string, applyEnv bool) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if applyEnv {
			if err := ApplyModelEnvOverrides(&cfg); err != nil {
				return Config{}, err
			}
		}
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	parser := yamlParser{cfg: cfg}
	if err := parser.parse(f); err != nil {
		return Config{}, fmt.Errorf("load %s: %w", path, err)
	}
	if applyEnv {
		if err := ApplyModelEnvOverrides(&parser.cfg); err != nil {
			return Config{}, fmt.Errorf("load %s: %w", path, err)
		}
	}
	return parser.cfg, nil
}

func Save(path string, cfg Config) error {
	if path == "" {
		path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(Render(cfg)), 0o600)
}

func Render(cfg Config) string {
	return render(cfg, false)
}

func RenderRedacted(cfg Config) string {
	return render(cfg, true)
}

func render(cfg Config, redact bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "review:\n")
	fmt.Fprintf(&b, "  concurrency: %d\n", cfg.Review.Concurrency)
	fmt.Fprintf(&b, "  context_lines: %d\n", cfg.Review.ContextLines)
	fmt.Fprintf(&b, "  max_tool_rounds: %d\n", cfg.Review.MaxToolRounds)
	fmt.Fprintf(&b, "  max_context_tool_calls: %d\n", cfg.Review.MaxContextToolCalls)
	fmt.Fprintf(&b, "  max_chunk_tokens: %d\n", cfg.Review.MaxChunkTokens)
	fmt.Fprintf(&b, "  confidence_threshold: %s\n", strconv.FormatFloat(cfg.Review.ConfidenceThreshold, 'f', -1, 64))
	fmt.Fprintf(&b, "  fail_on:\n")
	for _, severity := range cfg.Review.FailOn {
		fmt.Fprintf(&b, "    - %s\n", severity)
	}
	fmt.Fprintf(&b, "\nmodel:\n")
	fmt.Fprintf(&b, "  protocol: %s\n", cfg.Model.Protocol)
	fmt.Fprintf(&b, "  name: %s\n", cfg.Model.Name)
	fmt.Fprintf(&b, "  endpoint: %s\n", cfg.Model.Endpoint)
	if cfg.Model.APIKey != "" {
		apiKey := cfg.Model.APIKey
		if redact {
			apiKey = "********"
		}
		fmt.Fprintf(&b, "  api_key: %s\n", apiKey)
	}
	fmt.Fprintf(&b, "  timeout: %s\n", cfg.Model.Timeout)
	fmt.Fprintf(&b, "\noutput:\n")
	fmt.Fprintf(&b, "  json: %s\n", cfg.Output.JSON)
	fmt.Fprintf(&b, "  markdown: %s\n", cfg.Output.Markdown)
	fmt.Fprintf(&b, "  console: %s\n", cfg.Output.Console)
	fmt.Fprintf(&b, "  language: %s\n", cfg.Output.Language)
	fmt.Fprintf(&b, "  progress: %t\n", cfg.Output.Progress)
	return b.String()
}

func Set(cfg *Config, key, value string) error {
	switch key {
	case "review.concurrency":
		v, err := strconv.Atoi(value)
		if err != nil || v < 1 {
			return fmt.Errorf("review.concurrency must be a positive integer")
		}
		cfg.Review.Concurrency = v
	case "review.context_lines", "review.context":
		v, err := strconv.Atoi(value)
		if err != nil || v < 0 {
			return fmt.Errorf("%s must be a non-negative integer", key)
		}
		cfg.Review.ContextLines = v
	case "review.max_tool_rounds":
		v, err := strconv.Atoi(value)
		if err != nil || v < 1 {
			return fmt.Errorf("review.max_tool_rounds must be a positive integer")
		}
		cfg.Review.MaxToolRounds = v
	case "review.max_context_tool_calls":
		v, err := strconv.Atoi(value)
		if err != nil || v < 1 {
			return fmt.Errorf("review.max_context_tool_calls must be a positive integer")
		}
		cfg.Review.MaxContextToolCalls = v
	case "review.max_chunk_tokens":
		v, err := strconv.Atoi(value)
		if err != nil || v < 1000 {
			return fmt.Errorf("review.max_chunk_tokens must be at least 1000")
		}
		cfg.Review.MaxChunkTokens = v
	case "review.confidence_threshold":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || v < 0 || v > 1 {
			return fmt.Errorf("review.confidence_threshold must be between 0 and 1")
		}
		cfg.Review.ConfidenceThreshold = v
	case "review.fail_on":
		values := splitCSV(value)
		if len(values) == 0 {
			return fmt.Errorf("review.fail_on must contain at least one severity")
		}
		cfg.Review.FailOn = values
	case "model.protocol":
		switch value {
		case "openai", "gemini", "anthropic":
			cfg.Model.Protocol = value
		default:
			return fmt.Errorf("model.protocol must be one of openai, gemini, anthropic")
		}
	case "model.name":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("model.name must not be empty")
		}
		cfg.Model.Name = value
	case "model.endpoint":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("model.endpoint must not be empty")
		}
		cfg.Model.Endpoint = strings.TrimRight(value, "/")
	case "model.api_key":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("model.api_key must not be empty")
		}
		cfg.Model.APIKey = value
	case "model.timeout":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("model.timeout must be a Go duration")
		}
		cfg.Model.Timeout = d
	case "output.json":
		cfg.Output.JSON = value
	case "output.markdown":
		cfg.Output.Markdown = value
	case "output.console":
		switch value {
		case "detailed", "summary", "none":
			cfg.Output.Console = value
		default:
			return fmt.Errorf("output.console must be one of detailed, summary, none")
		}
	case "output.language":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("output.language must not be empty")
		}
		cfg.Output.Language = strings.TrimSpace(value)
	case "output.progress":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("output.progress must be true or false")
		}
		cfg.Output.Progress = v
	default:
		return fmt.Errorf("unsupported config key %q", key)
	}
	return nil
}

func Get(cfg Config, key string) (string, error) {
	switch key {
	case "review.concurrency":
		return strconv.Itoa(cfg.Review.Concurrency), nil
	case "review.context_lines", "review.context":
		return strconv.Itoa(cfg.Review.ContextLines), nil
	case "review.max_tool_rounds":
		return strconv.Itoa(cfg.Review.MaxToolRounds), nil
	case "review.max_context_tool_calls":
		return strconv.Itoa(cfg.Review.MaxContextToolCalls), nil
	case "review.max_chunk_tokens":
		return strconv.Itoa(cfg.Review.MaxChunkTokens), nil
	case "review.confidence_threshold":
		return strconv.FormatFloat(cfg.Review.ConfidenceThreshold, 'f', -1, 64), nil
	case "review.fail_on":
		return strings.Join(cfg.Review.FailOn, ","), nil
	case "model.protocol":
		return cfg.Model.Protocol, nil
	case "model.name":
		return cfg.Model.Name, nil
	case "model.endpoint":
		return cfg.Model.Endpoint, nil
	case "model.api_key":
		return cfg.Model.APIKey, nil
	case "model.timeout":
		return cfg.Model.Timeout.String(), nil
	case "output.json":
		return cfg.Output.JSON, nil
	case "output.markdown":
		return cfg.Output.Markdown, nil
	case "output.console":
		return cfg.Output.Console, nil
	case "output.language":
		return cfg.Output.Language, nil
	case "output.progress":
		return strconv.FormatBool(cfg.Output.Progress), nil
	default:
		return "", fmt.Errorf("unsupported config key %q", key)
	}
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

func NeedsModelConfiguration(cfg Config) bool {
	return strings.TrimSpace(cfg.Model.Name) == "" || cfg.Model.Name == Default().Model.Name
}

func ModelConfigurationHint(path string) string {
	if path == "" {
		path = DefaultPath()
	}
	return fmt.Sprintf("model is not configured; set it with `zadig-review-agent config set model.name <model> --config %s` or export ZADIG_REVIEW_MODEL_NAME", path)
}

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".zadig-review-agent", "config.yaml")
	}
	return filepath.Join(home, ".zadig-review-agent", "config.yaml")
}

func DefaultReportRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".zadig-review-agent", "reports")
	}
	return filepath.Join(home, ".zadig-review-agent", "reports")
}

func ApplyModelEnvOverrides(cfg *Config) error {
	if v := os.Getenv("ZADIG_REVIEW_MODEL_PROTOCOL"); v != "" {
		cfg.Model.Protocol = v
	}
	if v := os.Getenv("ZADIG_REVIEW_MODEL_NAME"); v != "" {
		cfg.Model.Name = v
	}
	if v := os.Getenv("ZADIG_REVIEW_MODEL_ENDPOINT"); v != "" {
		cfg.Model.Endpoint = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("ZADIG_REVIEW_MODEL_API_KEY"); v != "" {
		cfg.Model.APIKey = v
	}
	if v := os.Getenv("ZADIG_REVIEW_MODEL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("ZADIG_REVIEW_MODEL_TIMEOUT must be a Go duration")
		}
		cfg.Model.Timeout = d
	}
	return nil
}

type yamlParser struct {
	cfg     Config
	section string
	subkey  string
}

func (p *yamlParser) parse(f *os.File) error {
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		raw := stripComment(scanner.Text())
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := countIndent(raw)
		line := strings.TrimSpace(raw)
		if indent == 0 {
			key, value, ok := splitKV(line)
			if !ok {
				return fmt.Errorf("line %d: expected section or key/value", lineNo)
			}
			p.section = key
			p.subkey = ""
			if value != "" {
				if err := p.assign(key, "", value); err != nil {
					return fmt.Errorf("line %d: %w", lineNo, err)
				}
			}
			continue
		}
		if p.section == "" {
			return fmt.Errorf("line %d: key outside section", lineNo)
		}
		if strings.HasPrefix(line, "- ") {
			if err := p.appendList(p.section, p.subkey, strings.TrimSpace(strings.TrimPrefix(line, "- "))); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		key, value, ok := splitKV(line)
		if !ok {
			return fmt.Errorf("line %d: expected key/value", lineNo)
		}
		if value == "" {
			p.subkey = key
			continue
		}
		if indent > 2 && p.subkey != "" {
			if err := p.assignNested(p.section, p.subkey, key, value); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		if err := p.assign(p.section, key, value); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	return scanner.Err()
}

func (p *yamlParser) assignNested(section, parent, key, value string) error {
	return nil
}

func (p *yamlParser) assign(section, key, value string) error {
	value = trimScalar(value)
	switch section {
	case "review":
		switch key {
		case "concurrency":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				return fmt.Errorf("review.concurrency must be a positive integer")
			}
			p.cfg.Review.Concurrency = v
		case "context_lines", "context":
			v, err := strconv.Atoi(value)
			if err != nil || v < 0 {
				return fmt.Errorf("review.%s must be a non-negative integer", key)
			}
			p.cfg.Review.ContextLines = v
		case "max_tool_rounds":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				return fmt.Errorf("review.max_tool_rounds must be a positive integer")
			}
			p.cfg.Review.MaxToolRounds = v
		case "max_context_tool_calls":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1 {
				return fmt.Errorf("review.max_context_tool_calls must be a positive integer")
			}
			p.cfg.Review.MaxContextToolCalls = v
		case "max_chunk_tokens":
			v, err := strconv.Atoi(value)
			if err != nil || v < 1000 {
				return fmt.Errorf("review.max_chunk_tokens must be at least 1000")
			}
			p.cfg.Review.MaxChunkTokens = v
		case "confidence_threshold":
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || v < 0 || v > 1 {
				return fmt.Errorf("review.confidence_threshold must be between 0 and 1")
			}
			p.cfg.Review.ConfidenceThreshold = v
		default:
			return nil
		}
	case "model":
		switch key {
		case "protocol", "provider":
			p.cfg.Model.Protocol = value
		case "name":
			p.cfg.Model.Name = value
		case "endpoint":
			p.cfg.Model.Endpoint = strings.TrimRight(value, "/")
		case "api_key":
			p.cfg.Model.APIKey = value
		case "timeout":
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("model.timeout must be a Go duration")
			}
			p.cfg.Model.Timeout = d
		}
	case "output":
		switch key {
		case "json":
			p.cfg.Output.JSON = value
		case "markdown":
			p.cfg.Output.Markdown = value
		case "console":
			p.cfg.Output.Console = value
		case "language":
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("output.language must not be empty")
			}
			p.cfg.Output.Language = strings.TrimSpace(value)
		case "progress":
			v, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("output.progress must be true or false")
			}
			p.cfg.Output.Progress = v
		}
	default:
		return nil
	}
	return nil
}

func (p *yamlParser) appendList(section, key, value string) error {
	value = trimScalar(value)
	switch section {
	case "review":
		if key == "fail_on" {
			p.cfg.Review.FailOn = append(resetIfDefault(p.cfg.Review.FailOn, []string{"critical", "high"}), value)
		}
	case "files":
		return nil
	}
	return nil
}

func splitKV(line string) (string, string, bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

func stripComment(line string) string {
	var b strings.Builder
	inQuote := rune(0)
	for _, r := range line {
		if (r == '"' || r == '\'') && inQuote == 0 {
			inQuote = r
		} else if r == inQuote {
			inQuote = 0
		}
		if r == '#' && inQuote == 0 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func trimScalar(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func countIndent(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' {
			return n
		}
		n++
	}
	return n
}

func resetIfDefault(current, defaults []string) []string {
	if len(current) != len(defaults) {
		return current
	}
	for i := range current {
		if current[i] != defaults[i] {
			return current
		}
	}
	return nil
}
