package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".zadig-review-agent.yaml")
	err := os.WriteFile(path, []byte(`
review:
  concurrency: 2
  context_convergence_ratio: 0.8
  max_tokens_budget: 50000
  confidence_threshold: 0.9
  fail_on:
    - critical
model:
  protocol: anthropic
  name: claude-test
  endpoint: https://api.anthropic.com
  api_key: test-key
  timeout: 30s
output:
  json: out.json
  markdown: out.md
  debug: trace.jsonl
  language: en-US
  progress: true
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Concurrency != 2 || cfg.Review.ContextConvergenceRatio != 0.8 || cfg.Review.MaxTokensBudget != 50000 || cfg.Output.Debug != "trace.jsonl" || cfg.Output.Language != "en-US" || !cfg.Output.Progress {
		t.Fatalf("unexpected review config: %+v", cfg.Review)
	}
	if cfg.Model.Protocol != "anthropic" || cfg.Model.APIKey != "test-key" {
		t.Fatalf("unexpected model config: %+v", cfg.Model)
	}
}

func TestProgressEnabledByDefault(t *testing.T) {
	if !Default().Output.Progress {
		t.Fatal("review progress must be enabled by default")
	}
	if Default().Review.ContextLines != 3 {
		t.Fatalf("default context lines = %d, want 3", Default().Review.ContextLines)
	}
	if Default().Review.ContextConvergenceRatio != 0.5 {
		t.Fatalf("default convergence ratio = %v, want 0.5", Default().Review.ContextConvergenceRatio)
	}
}

func TestContextConvergenceRatioSetGetAndValidation(t *testing.T) {
	cfg := Default()
	if err := Set(&cfg, "review.context_convergence_ratio", "0.65"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(cfg, "review.context_convergence_ratio")
	if err != nil || got != "0.65" {
		t.Fatalf("unexpected convergence ratio %q: %v", got, err)
	}
	if !strings.Contains(Render(cfg), "context_convergence_ratio: 0.65") {
		t.Fatalf("render omitted convergence ratio:\n%s", Render(cfg))
	}
	for _, value := range []string{"0", "-0.1", "1.1", "invalid"} {
		if err := Set(&cfg, "review.context_convergence_ratio", value); err == nil {
			t.Fatalf("expected invalid convergence ratio %q to fail", value)
		}
	}
}

func TestMaxTokensBudgetSetGetAndValidation(t *testing.T) {
	cfg := Default()
	if err := Set(&cfg, "review.max_tokens_budget", "50000"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(cfg, "review.max_tokens_budget")
	if err != nil || got != "50000" {
		t.Fatalf("unexpected token budget %q: %v", got, err)
	}
	if !strings.Contains(Render(cfg), "max_tokens_budget: 50000") {
		t.Fatalf("render omitted token budget:\n%s", Render(cfg))
	}
	for _, value := range []string{"-1", "invalid"} {
		if err := Set(&cfg, "review.max_tokens_budget", value); err == nil {
			t.Fatalf("expected invalid token budget %q to fail", value)
		}
	}
}

func TestDebugOutputSetGetAndRender(t *testing.T) {
	cfg := Default()
	if err := Set(&cfg, "output.debug", "llm-debug.jsonl"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(cfg, "output.debug")
	if err != nil || got != "llm-debug.jsonl" {
		t.Fatalf("unexpected debug output %q: %v", got, err)
	}
	if !strings.Contains(Render(cfg), "debug: llm-debug.jsonl") {
		t.Fatalf("render omitted debug output:\n%s", Render(cfg))
	}
}

func TestLoadDefaultPathUsesHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".zadig-review-agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(`
model:
  protocol: gemini
  name: gemini-test
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Protocol != "gemini" || cfg.Model.Name != "gemini-test" {
		t.Fatalf("expected home config to load, got %+v", cfg.Model)
	}
}

func TestSetSaveLoadFileAndModelConfigurationHint(t *testing.T) {
	cfg := Default()
	if !NeedsModelConfiguration(cfg) {
		t.Fatal("expected default model to require configuration")
	}
	if err := Set(&cfg, "model.name", "gpt-test"); err != nil {
		t.Fatal(err)
	}
	if err := Set(&cfg, "model.api_key", "secret-key"); err != nil {
		t.Fatal(err)
	}
	if NeedsModelConfiguration(cfg) {
		t.Fatal("expected configured model name")
	}
	if err := Set(&cfg, "review.fail_on", "critical,medium"); err != nil {
		t.Fatal(err)
	}
	if err := Set(&cfg, "output.language", "en-US"); err != nil {
		t.Fatal(err)
	}
	if err := Set(&cfg, "output.progress", "true"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".zadig-review-agent", "config.yaml")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Model.Name != "gpt-test" || loaded.Model.APIKey != "secret-key" || len(loaded.Review.FailOn) != 2 || loaded.Review.FailOn[1] != "medium" || loaded.Output.Language != "en-US" || !loaded.Output.Progress {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}
	got, err := Get(loaded, "model.name")
	if err != nil {
		t.Fatal(err)
	}
	if got != "gpt-test" {
		t.Fatalf("unexpected get value %q", got)
	}
	progress, err := Get(loaded, "output.progress")
	if err != nil || progress != "true" {
		t.Fatalf("unexpected progress value %q: %v", progress, err)
	}
	if !strings.Contains(Render(loaded), "api_key: secret-key") {
		t.Fatalf("expected full render to include api key:\n%s", Render(loaded))
	}
	if !strings.Contains(RenderRedacted(loaded), "api_key: ********") || strings.Contains(RenderRedacted(loaded), "secret-key") {
		t.Fatalf("expected redacted render, got:\n%s", RenderRedacted(loaded))
	}
}

func TestLoadAppliesModelEnvironmentOverrides(t *testing.T) {
	t.Setenv("ZADIG_REVIEW_MODEL_PROTOCOL", "openai")
	t.Setenv("ZADIG_REVIEW_MODEL_NAME", "gpt-test")
	t.Setenv("ZADIG_REVIEW_MODEL_ENDPOINT", "https://example.test/v1/")
	t.Setenv("ZADIG_REVIEW_MODEL_API_KEY", "env-key")
	t.Setenv("ZADIG_REVIEW_MODEL_TIMEOUT", "45s")

	dir := t.TempDir()
	path := filepath.Join(dir, ".zadig-review-agent.yaml")
	err := os.WriteFile(path, []byte(`
model:
  protocol: anthropic
  name: claude-test
  endpoint: https://api.anthropic.com
  api_key: file-key
  timeout: 30s
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Protocol != "openai" || cfg.Model.Name != "gpt-test" || cfg.Model.Endpoint != "https://example.test/v1" || cfg.Model.APIKey != "env-key" || cfg.Model.Timeout != 45*time.Second {
		t.Fatalf("unexpected model environment overrides: %+v", cfg.Model)
	}
}

func TestLoadRejectsInvalidModelEnvironmentTimeout(t *testing.T) {
	t.Setenv("ZADIG_REVIEW_MODEL_TIMEOUT", "not-a-duration")

	dir := t.TempDir()
	path := filepath.Join(dir, ".zadig-review-agent.yaml")
	err := os.WriteFile(path, []byte(`
model:
  timeout: 30s
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected invalid environment timeout to fail")
	}
}
