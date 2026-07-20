# Zadig Review Agent

[![CI](https://github.com/koderover/zadig-review-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/koderover/zadig-review-agent/actions/workflows/ci.yml)
[![CodeQL](https://github.com/koderover/zadig-review-agent/actions/workflows/codeql.yml/badge.svg)](https://github.com/koderover/zadig-review-agent/actions/workflows/codeql.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Read-only, LLM-powered code review for local development and CI workflows.

English | [简体中文](README.md)

## What it does

Zadig Review Agent reviews Git changes, asks a configured language model to identify concrete defects, validates every finding against the actual diff, and produces console, JSON, and Markdown reports. It focuses on correctness, security, concurrency, resource management, performance, compatibility, and missing critical tests.

Key properties:

- reviews a workspace, one commit, or a ref range;
- supports OpenAI, Gemini, and Anthropic protocols through their official Go SDKs;
- applies built-in or repository-specific review rules;
- exposes only read-only repository tools to the model;
- returns deterministic exit codes suitable for CI quality gates;
- never writes comments back to a forge or modifies the reviewed repository.

This project is a review assistant, not a substitute for tests, security analysis, or human review. Model output can be incomplete or incorrect.

## Requirements

- Go 1.24 or later when building from source
- Git and a Git repository to review
- credentials for one supported model provider (not needed for `--preview`)

## Installation

### Go install

After the first tagged release is available:

```bash
go install github.com/koderover/zadig-review-agent@latest
```

`go install` honors the standard `GOPROXY` setting. To use a trusted organization or regional proxy for one installation:

```bash
GOPROXY=https://your-go-proxy.example,direct \
  go install github.com/koderover/zadig-review-agent@latest
```

Use an explicit version such as `@v0.1.0` when reproducible installation is more important than tracking the latest release.

### Release archive

Download the archive for Linux, macOS, or Windows from [GitHub Releases](https://github.com/koderover/zadig-review-agent/releases), then verify it with `checksums.txt`.

### Build from source

```bash
git clone https://github.com/koderover/zadig-review-agent.git
cd zadig-review-agent
make build
./bin/zadig-review-agent version
```

## Quick start

Preview which files and rules would be used without contacting a model:

```bash
zadig-review-agent review --preview
```

Configure a provider. Keeping the API key in an environment variable avoids writing it to disk or shell history:

```bash
zadig-review-agent config set model.protocol openai
zadig-review-agent config set model.name gpt-4o
zadig-review-agent config set model.endpoint https://api.openai.com/v1
export ZADIG_REVIEW_MODEL_API_KEY='your-api-key'
```

Review the current workspace:

```bash
zadig-review-agent review
```

Review a commit or a range:

```bash
zadig-review-agent review --commit <sha>
zadig-review-agent review --from origin/main --to HEAD
```

Use `zadig-review-agent help` and `zadig-review-agent review --help` for the complete command-line reference.

## Configuration

The default configuration file is `~/.zadig-review-agent/config.yaml`. Start from [.zadig-review-agent.example.yaml](.zadig-review-agent.example.yaml), or use `config set`:

```bash
zadig-review-agent config path
zadig-review-agent config show
zadig-review-agent config get model.name
zadig-review-agent config set output.language en-US
```

Configuration precedence is:

```text
built-in defaults < configuration file < ZADIG_REVIEW_MODEL_* < review flags
```

Supported model environment variables are:

```text
ZADIG_REVIEW_MODEL_PROTOCOL
ZADIG_REVIEW_MODEL_NAME
ZADIG_REVIEW_MODEL_ENDPOINT
ZADIG_REVIEW_MODEL_TIMEOUT
ZADIG_REVIEW_MODEL_API_KEY
```

`config show` redacts the API key. `config get model.api_key` intentionally returns the real value, so avoid printing it in logs.

## Review rules

Rules are declarative JSON data and cannot execute code. They are loaded in this order:

1. `--rule <path>`
2. `<repository>/.zadig-review/rules.json`
3. `~/.zadig-review/rules.json`
4. embedded system rules

See [.zadig-review/rules.example.json](.zadig-review/rules.example.json) and [.zadig-review/docs/go-review.md](.zadig-review/docs/go-review.md). Check the resolved rule for a path with:

```bash
zadig-review-agent rules check internal/reviewer/reviewer.go
```

## CI usage

The `--ci` flag selects concise console output. Explicit report paths are convenient for CI artifacts:

```bash
zadig-review-agent review \
  --from origin/main \
  --to HEAD \
  --ci \
  --output-json "$PWD/review-report.json" \
  --output-md "$PWD/review-report.md"
```

Exit codes are stable:

| Code | Meaning |
| --- | --- |
| `0` | Review completed and no finding matched `fail_on`. |
| `1` | Review completed and at least one finding matched `fail_on`. |
| `2` | Configuration, Git, provider, filtering, or review processing was incomplete. |
| `130` | The process was canceled. |

The default quality gate fails on `critical` and `high` findings. Configure it with `review.fail_on` or `--fail-on`.

## Privacy and security

- Diffs, rule text, and repository content requested through read-only tools are sent to the model endpoint you configure. Review the provider's data policy before using sensitive code.
- JSON reports retain detailed tool output and raw model responses for diagnostics and can contain source code. Reports and configuration files are created with restricted permissions, but you must protect, retain, and delete them according to your own policy.
- The agent does not provide the model with shell, network, or write-file tools and does not execute repository-provided commands or configuration.
- API keys are not inserted into prompts or reports. Prefer environment variables or a secret manager in CI.
- The project has no telemetry service.

Please report vulnerabilities according to [SECURITY.md](SECURITY.md), not through a public issue.

## Troubleshooting

### Model is not configured

Set `model.name` and the provider settings, or export the corresponding `ZADIG_REVIEW_MODEL_*` variables. `--preview` works without model credentials.

### A range review cannot resolve its base

Both refs must exist locally. Fetch the target branch before running the review, for example `git fetch origin main`.

### A file is unexpectedly excluded

Run the same review with `--preview`, then inspect the exclusion reason and resolved rule. Use `rules check` for a single path.

### The review exits with code 2

Inspect warnings and errors in the console or JSON report. An incomplete model tool loop, token limit, relocation, or filtering phase intentionally cannot pass the quality gate.

## Development

```bash
make help
make check
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow and [DESIGN.md](DESIGN.md) for implementation details.

## Releasing

Release configuration lives in [.goreleaser.yaml](.goreleaser.yaml) and requires GoReleaser v2. On macOS, install it with Homebrew:

```bash
brew install goreleaser
```

Alternatively, install it with Go:

```bash
go install github.com/goreleaser/goreleaser/v2@latest
```

Before a release, run all checks and a local snapshot. Snapshot artifacts are written to `dist/` and are not uploaded to GitHub:

```bash
make check
goreleaser check
goreleaser release --snapshot --clean
```

### Automated tag release (recommended)

Create and push a semantic `vX.Y.Z` tag:

```bash
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

The [Release workflow](.github/workflows/release.yml) then runs the tests, builds amd64/arm64 archives for Linux, macOS, and Windows, generates SHA-256 checksums, and creates the GitHub Release.

### Manual local release

A manual release requires a GitHub token with repository Contents read/write access. Because pushing a tag triggers the automated release, temporarily disable the Release workflow and re-enable it afterward:

```bash
git tag -a v0.1.0 -m "Release v0.1.0"
gh workflow disable release.yml
git push origin v0.1.0

GITHUB_TOKEN="$(gh auth token)" goreleaser release --clean

gh workflow enable release.yml
```

Ensure that `release.yml` is re-enabled whether the manual release succeeds or fails. Never run automated and manual releases for the same tag, because their artifacts will conflict.

## License

Licensed under the [Apache License 2.0](LICENSE).
