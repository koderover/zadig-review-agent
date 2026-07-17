package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koderover/zadig-code-review-agent/internal/agent"
	"github.com/koderover/zadig-code-review-agent/internal/config"
)

func TestRulesCheck(t *testing.T) {
	dir := t.TempDir()
	rulePath := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(rulePath, []byte(`{"rules":[{"path":"src/**/*.go","rule":"custom go rule"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), []string{"rules", "check", "--rule", rulePath, "src/main.go"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Source: Custom (--rule)") || !strings.Contains(out, "Pattern: src/**/*.go") || !strings.Contains(out, "custom go rule") {
		t.Fatalf("unexpected rules check output:\n%s", out)
	}
}

func TestReviewPreviewDoesNotNeedModel(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldwd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	git(t, "init")
	git(t, "config", "user.email", "test@example.com")
	git(t, "config", "user.name", "Test")
	git(t, "config", "commit.gpgsign", "false")
	if err := os.WriteFile("main.go", []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, "add", "main.go")
	git(t, "commit", "-m", "base")
	base := strings.TrimSpace(gitOut(t, "rev-parse", "HEAD"))
	if err := os.WriteFile("main.go", []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, "add", "main.go")
	git(t, "commit", "-m", "change")
	rulePath := filepath.Join(dir, "rules.json")
	if err := os.WriteFile(rulePath, []byte(`{"rules":[{"path":"**/*.go","rule":"preview go rule"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("nested", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("nested"); err != nil {
		t.Fatal(err)
	}
	repoRoot := strings.TrimSpace(gitOut(t, "rev-parse", "--show-toplevel"))
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), []string{"review", "--from", base, "--to", "HEAD", "--rule", rulePath, "--preview"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Code review:") || !strings.Contains(stderr.String(), "Changed files: 1") || !strings.Contains(stderr.String(), "Review process:") || !strings.Contains(stderr.String(), "[progress] preview completed") {
		t.Fatalf("unexpected preview progress output:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Code review:") || !strings.Contains(stdout.String(), "Review result: findings=0 status=complete") {
		t.Fatalf("unexpected preview result output:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Report dir:") || strings.Contains(stdout.String(), "JSON report:") || strings.Contains(stdout.String(), "Markdown report:") {
		t.Fatalf("report paths must not be printed to the console:\n%s", stdout.String())
	}
	repository := sanitizePathLabel(repoRoot)
	jsonReports, err := filepath.Glob(filepath.Join(home, ".zadig-review-agent", "reports", repository, "*", "review-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	markdownReports, err := filepath.Glob(filepath.Join(home, ".zadig-review-agent", "reports", repository, "*", "review-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jsonReports) != 1 || len(markdownReports) != 1 {
		t.Fatalf("expected one json and one markdown report, got json=%v markdown=%v", jsonReports, markdownReports)
	}
	if filepath.Dir(jsonReports[0]) != filepath.Dir(markdownReports[0]) {
		t.Fatalf("expected reports in the same per-review dir, got json=%s markdown=%s", jsonReports[0], markdownReports[0])
	}
	data, err := os.ReadFile(jsonReports[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"repository": "`+repoRoot+`"`) {
		t.Fatalf("expected report metadata to contain repository root %q:\n%s", repoRoot, data)
	}
	if !strings.Contains(string(data), `"llm_requests": 0`) || !strings.Contains(string(data), `"total_tokens": 0`) {
		t.Fatalf("expected preview report to contain zero usage:\n%s", data)
	}
}

func TestReportRunDirUsesSecondTimestamp(t *testing.T) {
	metadata := agent.Metadata{DiffMode: "commit", Commit: "d3ee93bda", Repository: "/Users/petrus/Project/koderover/zadig"}
	when := time.Date(2026, time.July, 14, 9, 1, 39, 226907000, time.UTC)
	dir := reportRunDirAt(metadata, when)
	want := filepath.Join(config.DefaultReportRoot(), "Users-petrus-Project-koderover-zadig", "20260714T090139Z-commit-d3ee93bda")
	if dir != want {
		t.Fatalf("unexpected report dir: got %q want %q", dir, want)
	}
}

func TestAvailableReportRunDirAvoidsCollision(t *testing.T) {
	preferred := filepath.Join(t.TempDir(), "20260714T090139Z-workspace")
	if err := os.Mkdir(preferred, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, want := availableReportRunDir(preferred), preferred+"-2"; got != want {
		t.Fatalf("unexpected collision path: got %q want %q", got, want)
	}
}

func TestRepositoryPathGroupingUsesFlattenedFullPath(t *testing.T) {
	first := sanitizePathLabel(repositoryPath("/Users/petrus/Project/koderover/zadig"))
	second := sanitizePathLabel(repositoryPath("/Users/petrus/Project/another/zadig"))
	if first != "Users-petrus-Project-koderover-zadig" {
		t.Fatalf("unexpected flattened repository path %q", first)
	}
	if first == second {
		t.Fatalf("same basename repositories must use different groups: %q", first)
	}
	if got := sanitizePathLabel(`/Users/petrus/My Project\\nested///zadig`); got != "Users-petrus-My-Project-nested-zadig" {
		t.Fatalf("unexpected special-character normalization %q", got)
	}
}

func TestReviewBaseFlagRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), []string{"review", "--base", "origin/main", "--preview"}, &stdout, &stderr)
	if err == nil || code == 0 {
		t.Fatalf("expected --base to be rejected, code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestReviewIncludeFlagRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), []string{"review", "--include", "src/**", "--preview"}, &stdout, &stderr)
	if err == nil || code == 0 {
		t.Fatalf("expected --include to be rejected, code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestConfigSetGetShowAndPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), []string{"config", "set", "model.name", "gpt-test", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("set model.name code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	code, err = Run(context.Background(), []string{"config", "set", "--config", configPath, "model.protocol", "gemini"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("set model.protocol code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	code, err = Run(context.Background(), []string{"config", "set", "model.api_key", "secret-key", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("set model.api_key code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	stdout.Reset()
	code, err = Run(context.Background(), []string{"config", "get", "model.name", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("get model.name code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "gpt-test" {
		t.Fatalf("unexpected get output %q", stdout.String())
	}
	stdout.Reset()
	code, err = Run(context.Background(), []string{"config", "get", "model.api_key", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("get model.api_key code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "secret-key" {
		t.Fatalf("unexpected api key get output %q", stdout.String())
	}
	stdout.Reset()
	code, err = Run(context.Background(), []string{"config", "show", "--config=" + configPath}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("show code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "protocol: gemini") || !strings.Contains(stdout.String(), "name: gpt-test") || !strings.Contains(stdout.String(), "api_key: ********") || strings.Contains(stdout.String(), "secret-key") {
		t.Fatalf("unexpected show output:\n%s", stdout.String())
	}
	stdout.Reset()
	code, err = Run(context.Background(), []string{"config", "path", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("path code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != configPath {
		t.Fatalf("unexpected path output %q", stdout.String())
	}
}

func TestConfigSetHelpListsKeys(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), []string{"config", "set", "-h"}, &stdout, &stderr)
	if err != nil || code != 0 {
		t.Fatalf("config set -h code=%d err=%v stderr=%s", code, err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"usage: zadig-review-agent config set", "model.protocol", "model.name", "model.api_key", "review.fail_on", "output.language", "output.console"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected help to contain %q:\n%s", want, out)
		}
	}
}

func TestApplyCLIOverrides(t *testing.T) {
	cfg := config.Default()
	visited := map[string]bool{
		"ci":                     true,
		"concurrency":            true,
		"context-lines":          true,
		"max-tool-rounds":        true,
		"max-context-tool-calls": true,
		"max-chunk-tokens":       true,
		"confidence-threshold":   true,
		"fail-on":                true,
		"language":               true,
		"model-protocol":         true,
		"model-name":             true,
		"model-endpoint":         true,
		"model-timeout":          true,
		"output-json":            true,
		"output-md":              true,
		"console":                true,
		"progress":               true,
	}
	err := applyCLIOverrides(&cfg, visited, cliOverrides{
		ciMode:              true,
		concurrency:         8,
		contextLines:        12,
		maxToolRounds:       3,
		maxContextToolCalls: 7,
		maxChunkTokens:      2000,
		confidenceThreshold: 0.6,
		failOn:              "critical,medium",
		language:            "en-US",
		modelProtocol:       "anthropic",
		modelName:           "claude-test",
		modelEndpoint:       "https://example.test/v1/",
		modelTimeout:        "45s",
		jsonOut:             "out.json",
		mdOut:               "out.md",
		console:             "none",
		progress:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Concurrency != 8 || cfg.Review.ContextLines != 12 || cfg.Review.MaxToolRounds != 3 || cfg.Review.MaxContextToolCalls != 7 || cfg.Review.MaxChunkTokens != 2000 || cfg.Review.ConfidenceThreshold != 0.6 || cfg.Output.Language != "en-US" {
		t.Fatalf("review overrides failed: %+v", cfg.Review)
	}
	if strings.Join(cfg.Review.FailOn, ",") != "critical,medium" {
		t.Fatalf("unexpected fail_on: %+v", cfg.Review.FailOn)
	}
	if cfg.Model.Protocol != "anthropic" || cfg.Model.Name != "claude-test" || cfg.Model.Endpoint != "https://example.test/v1" || cfg.Model.Timeout != 45*time.Second {
		t.Fatalf("unexpected model overrides: %+v", cfg.Model)
	}
	if cfg.Output.JSON != "out.json" || cfg.Output.Markdown != "out.md" || cfg.Output.Console != "none" || !cfg.Output.Progress {
		t.Fatalf("unexpected output overrides: %+v", cfg.Output)
	}
}

func TestApplyCLIProgressFalseOverridesConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Output.Progress = true
	if err := applyCLIOverrides(&cfg, map[string]bool{"progress": true}, cliOverrides{progress: false}); err != nil {
		t.Fatal(err)
	}
	if cfg.Output.Progress {
		t.Fatal("expected --progress=false to disable configured progress")
	}
}

func TestApplyCLIOverridesRejectsInvalidCIValues(t *testing.T) {
	cfg := config.Default()
	err := applyCLIOverrides(&cfg, map[string]bool{"fail-on": true}, cliOverrides{failOn: "urgent"})
	if err == nil {
		t.Fatal("expected invalid severity to fail")
	}
	err = applyCLIOverrides(&cfg, map[string]bool{"console": true}, cliOverrides{console: "verbose"})
	if err == nil {
		t.Fatal("expected invalid console mode to fail")
	}
}

func TestApplyCLIOverridesWinsOverModelEnvironment(t *testing.T) {
	t.Setenv("ZADIG_REVIEW_MODEL_PROTOCOL", "gemini")
	t.Setenv("ZADIG_REVIEW_MODEL_NAME", "gemini-env")
	cfg := config.Default()
	if err := config.ApplyModelEnvOverrides(&cfg); err != nil {
		t.Fatal(err)
	}
	err := applyCLIOverrides(&cfg, map[string]bool{
		"model-protocol": true,
		"model-name":     true,
	}, cliOverrides{
		modelProtocol: "anthropic",
		modelName:     "claude-cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Protocol != "anthropic" || cfg.Model.Name != "claude-cli" {
		t.Fatalf("expected CLI overrides to win over environment: %+v", cfg.Model)
	}
}

func git(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
