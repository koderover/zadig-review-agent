# Zadig Review Agent Design

English | [简体中文](DESIGN-zh-CN.md)

## 1. Goals and boundaries

`zadig-review-agent` is a read-only code-review CLI for local development and Zadig/CI workflows. It reads changes from a local Git repository, uses a large language model to identify concrete defects, and produces console output, JSON and Markdown reports, and deterministic CI exit codes.

Reviews focus on correctness, security, concurrency, resource management, performance, compatibility, and missing critical tests. Formatting, naming preferences, and style observations without a concrete risk are not reported by default.

The current version does not modify the repository, expose arbitrary shell execution, access external MCP servers, load skills, or write comments to GitHub, GitLab, or Gitee.

## 2. Architecture

```text
CLI / config / environment
  -> Git repository and diff mode
  -> unified diff parsing
  -> rules loading and file filtering
  -> hunk chunking and per-file scheduling
  -> optional plan phase
  -> native LLM tool loop with synchronous context compression
  -> finding relocation and falsification filter
  -> deterministic validation, deduplication and policy
  -> console / JSON / Markdown reports
```

Packages have the following responsibilities:

- `internal/cli`: command parsing, configuration overrides, preview, report directories, and progress output.
- `internal/config`: YAML configuration, environment overrides, and `config get/set/show/path`.
- `internal/gitdiff`: workspace, commit, and range diff acquisition and unified-diff parsing.
- `internal/rules`: four-level rule loading, glob matching, references, and system rules.
- `internal/filter`: deterministic file filtering.
- `internal/protocol`: common message/tool protocol and three official SDK adapters.
- `internal/reviewer`: chunk scheduling, planning, tool loop, compression, relocation, review filtering, and finding validation.
- `internal/reporter`: console, JSON, and Markdown output.
- `internal/agent`: report, finding, usage, and tool-audit data types.
- `internal/version`: build version, commit, and timestamp metadata.

Go owns all flow control. Provider adapters only map protocols. The model proposes findings and selects read-only tools, but cannot decide file filtering, line validity, deduplication, or exit policy.

## 3. CLI and diff modes

Primary commands are:

```bash
zadig-review-agent version
zadig-review-agent review --from origin/main --to HEAD
zadig-review-agent review --commit <sha>
zadig-review-agent review
zadig-review-agent review --commit <sha> --preview
zadig-review-agent rules check src/main.go --rule rules.json
zadig-review-agent config show
zadig-review-agent config set model.name gpt-4o
```

### Range

`--from <ref> --to <ref>` reviews `merge-base(from,to)..to`. Both flags are required together.

### Commit

`--commit <sha>` uses `git show` to review the changes introduced by one commit.

### Workspace

With no range or commit, workspace mode runs one `git diff HEAD` so staged and unstaged changes are combined. An unborn repository falls back to the cached diff. Untracked files are converted into diffs separately, and each path produces only one final `FileDiff`.

Git diff/show disables external diff and textconv, uses stable prefixes and `--end-of-options`, and supports spaces, Unicode, renames, and Git-quoted paths. An untracked symlink is reviewed as link-target text and is never followed outside the repository.

`review.context_lines` defaults to `3` and can be overridden by configuration or `--context-lines`.

### Preview

`--preview` performs Git diff acquisition, filtering, and rule resolution only. It reports kept and excluded files, reasons, and matched rule metadata. It neither initializes a provider nor produces findings or triggers the quality gate.

## 4. Configuration

The default path is `~/.zadig-review-agent/config.yaml`:

```yaml
review:
  concurrency: 4
  context_lines: 3
  max_tool_rounds: 30
  max_context_tool_calls: 10
  context_convergence_ratio: 0.5
  max_chunk_tokens: 20000
  max_tokens_budget: 0
  confidence_threshold: 0.75
  fail_on: [critical, high]

model:
  protocol: openai
  name: configured-model
  endpoint: https://api.openai.com/v1
  api_key: sk-...
  timeout: 8m

output:
  json: review-report.json
  markdown: review-report.md
  console: detailed
  language: en-US
  progress: true
```

Precedence is:

```text
built-in defaults < config file < ZADIG_REVIEW_MODEL_* < review flags
```

Environment variables are `ZADIG_REVIEW_MODEL_PROTOCOL`, `ZADIG_REVIEW_MODEL_NAME`, `ZADIG_REVIEW_MODEL_ENDPOINT`, `ZADIG_REVIEW_MODEL_TIMEOUT`, and `ZADIG_REVIEW_MODEL_API_KEY`.

`config show` redacts API keys, while `config get model.api_key` returns the actual value. A real review rejects the placeholder default model; preview does not require model configuration. Review controls, model settings, and output settings can be overridden by review flags. API keys can only come from configuration or the environment, not review flags.

## 5. Rules

Rules are non-executable declarative data. Layers are loaded in priority order:

1. `--rule <path>`
2. `<repo>/.zadig-review/rules.json`
3. `~/.zadig-review/rules.json`
4. embedded `internal/rules/system_rules.json`

Supplying `--rule` does not disable lower layers. The first matching rule in each layer wins, and resolution continues to the next layer only when the current layer has no match.

```json
{
  "include": ["src/**/*.custom"],
  "exclude": ["**/vendor/**", "**/generated/**"],
  "rules": [
    {
      "path": "**/*.go",
      "rule": ".zadig-review/docs/go-review.md",
      "merge_system_rule": true
    }
  ]
}
```

Matching is case-insensitive and supports `**`, `?`, character classes, and `{a,b}` expansion. The final system `**` rule supplies a fallback only when there is no language- or file-specific system rule.

A rule can be inline text or a `.md`, `.txt`, or `.markdown` reference. Custom references resolve relative to the custom rule file, project references relative to the repository root, and global references relative to `~/.zadig-review/`. References are limited to 512 KiB and reject path escape or symlinks resolving to unsupported extensions. A failed reference adds a non-blocking warning and falls back to a lower layer.

`merge_system_rule: true` combines the matching user and system rule. Filtering settings come from the highest-priority layer that actually declares include/exclude and are not merged across layers.

## 6. File filtering

Files are processed in this order:

1. invalid or escaping path (`invalid_path`)
2. binary (`binary`)
3. deleted (`deleted`)
4. user exclude (`user_exclude`)
5. user include bypass
6. unsupported extension or basename (`unsupported_ext`)
7. default test path (`default_path`)

Include is a bypass, not an allowlist: it bypasses the extension allowlist and default test exclusion, but never invalid-path, binary, deleted, or user-exclude checks. Built-in basenames include `Dockerfile`, `Makefile`, `pom.xml`, `build.gradle`, `package.json`, and `Cargo.toml`; other files use the extension allowlist.

## 7. Provider protocol

| Protocol | Official SDK | API |
| --- | --- | --- |
| `openai` | `github.com/openai/openai-go/v3` | Chat Completions |
| `gemini` | `google.golang.org/genai` | generateContent |
| `anthropic` | `github.com/anthropics/anthropic-sdk-go` | Messages |

The common request supports normalized system, user, assistant, and tool messages; tool definitions; assistant tool calls and matching results; and a `RequireTool` marker. The main loop accepts only native function/tool calls returned by official SDKs. It does not parse textual action JSON or repair legacy response formats.

The first request allows autonomous tool choice. Runtime tool definitions put `task_done` and `code_comment` before context tools, matching the completion-first decision in the system prompt. Once no concrete hypothesis remains, the model should batch all confirmed findings into one `code_comment` call and call `task_done` in the same response, or call `task_done` alone for a clean review. Outside finalization, `code_comment` does not implicitly complete the review; its result explicitly directs the next turn to `task_done` when nothing remains. During finalization, a successful batched comment is accepted as terminal for providers that cannot emit both calls together.

A text-only response adds a reminder and forces tools on later requests (`required`, `ANY`, or `any`, depending on provider). Three consecutive no-tool responses before any valid tool activity produce `tool_loop_empty_limit_reached` and an incomplete review. After valid activity, a no-tool response on an autonomous round gets one forced retry, and any no-tool response on that retry ends the model turn. Finalization is already a forced-tool round, so an empty or natural-language response there ends immediately without another full request. If a finalization tool call is present but rejected because its arguments or payload are invalid, the model gets exactly one terminal-only repair request; this repair may extend the configured round limit by one. `finalization_failed` is emitted only if that repair also fails. The last configured tool round is reserved for finalization instead of allowing a context call that cannot be followed by a result-submission round.

SDK retries are disabled. The reviewer performs one consistent retry for timeouts, HTTP 408, 429, and 5xx; authentication and parameter errors are not retried. Every actual request contributes to `LLM Requests`.

## 8. Prompts, tools, and compression

Plan, Main, Relocation, Review Filter, Memory Compression, and Localization use separate embedded system/user prompts. Security boundaries and output constraints occur only in system messages. Diffs, rules, changed-file context, and candidate findings occur only in user messages. Tool output occurs only in tool messages. Repository data is always labeled untrusted.

A file with at least 50 changed lines receives a tool-free plan phase. Large hunks are chunked using `max_chunk_tokens`. Local token estimates use the embedded cl100k_base tokenizer (as in OCR's default CountTokens); counts for other model families remain approximate. A local token guard stops requests near 80% of the configured chunk capacity; provider usage remains authoritative.

Each file has at most `review.max_context_tool_calls` context calls, default 10. Changes of at most 10 lines are capped at 6, changes of 11–50 lines at 8, and larger changes at the configured maximum. Convergence begins at `ceil(effective_limit × review.context_convergence_ratio)`; the ratio defaults to `0.5` and can also be set with `--context-convergence-ratio`. The model may then use at most one final batched context round and must submit findings or finish. This soft limit starts convergence halfway through the effective budget, reducing low-value exploratory calls while preserving one final evidence-gathering round. `changed_diff_read` counts as one context call even when it batches several paths. During finalization, only `task_done` and `code_comment` remain available.

`review.max_tokens_budget` (or `--max-tokens-budget`) provides an aggregate input-plus-output token budget; `0` is unlimited. Before dispatching each file/chunk, the scheduler reserves an OCR-style estimate for it and stops when committed usage plus the next estimate would exceed the budget. Already dispatched work is allowed to finish. The resulting partial review is marked incomplete and identifies the first skipped chunk with `token_budget_reached`.

Native tools in `internal/reviewer/tools.json` are:

- `file_read`: read at most 500 lines from the post-change snapshot.
- `code_search`: search tracked files through `git grep`, with pathspec, case, and PCRE options. In the default fixed-string mode, an unescaped `|` separates OR alternatives, `\|` searches for a literal pipe, and redundant regex escapes such as `\(` or `\.` are normalized to their literal characters.
- `changed_diff_read`: batch-read up to 10 other diffs from the reviewed change set. Valid paths are returned even when the same batch contains the current file, malformed paths, or paths outside the reviewed change; rejected paths are reported alongside the valid output. Each valid path is injected at most once per file/chunk session, with a cumulative 32 KiB changed-diff limit.
- `file_find`: find files by basename keyword, not glob.
- `code_comment`: submit up to 10 candidate findings for the current file in one batched call; the legacy single `finding` argument remains accepted. An omitted `file` is scoped to the current file; a non-empty path naming another file is rejected. Mixed batches retain valid current-file findings.
- `task_done`: explicitly finish the current file review.

Assistant tool calls and all matching results are retained. Output is capped at 32 KiB and remains valid UTF-8. Identical context calls are cached within one file/chunk session; fully covered `file_read` ranges also hit the cache. Cache hits neither re-execute nor consume tool budget and re-inject the real cached content. `changed_diff_read` uses stricter path-level deduplication: overlapping calls return only previously unseen paths, and a fully repeated request returns a short cached notice without injecting the diff again. Once all paths have been read or its cumulative limit is reached, the tool is removed from later requests. Subtasks do not share caches.

### Context compression

Before each autonomous main request, the reviewer estimates message and tool-definition tokens. At 60% of `max_chunk_tokens`, completed older rounds may be synchronously summarized. Convergence and Finalization requests skip this opportunistic compression unless the next request would exceed the 80% guard; in that case all completed rounds are summarized while the pending instruction remains intact:

- system and initial user messages remain fixed;
- older complete assistant/tool rounds are summarized;
- the newest two complete rounds remain intact with tool-call IDs;
- a request above the 80% guard summarizes all completed rounds, even if fewer than three rounds exist;
- the result returns as a `<previous_review_summary>` user message.

Compression input is structured JSON. Compression has no tools and cannot access the repository. Its usage is added to total review usage. Failure, empty output, or a non-shrinking summary preserves the original messages and disables further compression for that file, leaving the 80% guard and tool budget as backstops.

Commit/range `file_read` uses the reviewed ref; workspace mode uses the working tree. All paths are constrained to the repository. No shell, network, write-file, or arbitrary Git tool is exposed to the model.

## 9. Finding validation

`code_comment` submits one `finding` or a `findings` batch of up to 10 entries. Each entry contains severity, category, optional rule ID, path, line range, existing code, human-readable explanation, suggestion, and confidence. Severity and category are normalized to lowercase. Categories are restricted to correctness, security, concurrency, performance, compatibility, and tests, with deterministic aliases for a few common model values. Unknown categories are rejected.

After Review Filter, non-English output is localized in one tool-free batch unless every human-readable field already uses the requested Chinese script. Localization can only rewrite title, problem, evidence, and suggestion by candidate ID. It cannot change paths, lines, severity, category, confidence, or finding count. Review Filter and Localization accept bare arrays and a small set of common wrappers, retry malformed responses once with strict formatting, and retain original findings while marking the review incomplete if retries fail. A response ending with `finish_reason=length` is reported explicitly as truncated and is not replayed into the retry context.

Validation order is:

1. locate exact contiguous `existing_code` in a new-side hunk;
2. if non-empty `existing_code` does not match the current diff, never trust a coincidentally overlapping supplied line range; when the snippet appears in another changed file, drop the candidate without Relocation;
3. only when `existing_code` is empty, accept a supplied line range overlapping a changed line;
4. otherwise ask Relocation for exact existing code; malformed JSON receives one strict repair request, while a valid but unresolvable snippet drops only that candidate;
5. assign stable temporary IDs such as `c-0`;
6. let Review Filter return IDs to delete only;
7. allow deletion only when the diff directly disproves a candidate;
8. retain candidates and mark incomplete if filtering fails;
9. revalidate lines, severity, confidence, and path, then fingerprint, deduplicate, and aggregate.

Allowed severities are critical, high, medium, and low. Findings below `confidence_threshold` are removed. Only local `fail_on` policy determines the quality gate.

## 10. Reports and progress

The default report directory is:

```text
~/.zadig-review-agent/reports/<flattened-repository-path>/<review-id>/
```

For example:

```text
/home/developer/projects/zadig
-> home-developer-projects-zadig
```

Review IDs use a UTC second timestamp plus diff identity, with `-2`, `-3`, and so on for collisions. Existing reports are not migrated. Relative JSON, Markdown, and optional LLM debug paths are placed in the per-review directory; absolute paths are used directly. Reports and debug logs have mode `0600`.

JSON contains metadata, stats, findings, excluded files, resolved rules, warnings, errors, usage, duration, and process details. Markdown contains a readable summary, findings, rules, and a lightweight tool index. Compression and model-response records include stage, attempts, status, timing, raw text/error, finish reason, and independent usage. Detailed raw process data is excluded from console and Markdown output.

Provider responses are the sole source of token usage. Total falls back to prompt plus completion if absent. Every request, including failure and retry, is counted. Supported cache-read and cache-write counters are preserved.

Progress is enabled by default and written to stderr; final output goes to stdout. Full tool arguments and bounded output are stored only in JSON process records.

LLM debug logging is disabled by default. `--debug` writes the human-readable `llm-debug.md` in the per-review directory; `--debug-file` or `output.debug` selects another path. Markdown groups each actual provider attempt into a call section and pairs its Request and Response, including retries, across Plan, Main Review, Memory Compression, Relocation, Review Filter, and Localization. Each section displays stage, file, sequence, transport-attempt number, timing, estimated tokens, normalized messages/tools, normalized response, usage, finish reason, and errors. A `.jsonl` suffix selects the compact machine-readable request/response event stream. Both formats deliberately exclude the API key but include full prompts, diffs, repository tool results, and model output, so they must be handled as sensitive source data.

## 11. Completeness and exit codes

| Code | Meaning |
| --- | --- |
| `0` | Complete, with no finding matching `fail_on`. |
| `1` | Complete, with at least one finding matching `fail_on`. |
| `2` | Configuration, Git, provider, filtering, or any review chunk was incomplete. |
| `130` | Context canceled. |

Warnings that make a report incomplete include token-threshold exhaustion, empty or exhausted tool loops, failed/invalid Review Filter, and failed/invalid Relocation. A failed rule reference warns and falls back but does not alone make the review incomplete. Partial findings may be saved, but an incomplete review cannot pass.

## 12. Security

- Repository content, diffs, rules, tool output, and candidate findings are untrusted.
- Repository data never enters system messages.
- Repository-provided commands and configuration are not executed.
- Absolute paths, parent traversal, and outside-repository symlinks are rejected.
- Untracked symlinks are not followed.
- Rule references constrain path, extension, and size.
- API keys do not enter prompts, console output, or reports.
- Reports and configuration use restricted permissions.
- Findings must validate against real changed lines.
- Reviews remain read-only.

## 13. Tests, builds, and releases

```bash
make fmt
make check
make build
```

Tests cover diff modes and special paths; rule priority, references, and filtering; provider messages, native tools, usage, and forced-tool mappings; multi-round conversations and concurrency; relocation, filtering, localization, validation, and deduplication; preview, overrides, report directories and formats; and traversal, symlink, and truncation boundaries.

CI runs formatting checks, module verification, vet, race tests, builds, a GoReleaser snapshot, and CodeQL. A `vX.Y.Z` tag triggers GoReleaser to build Linux, macOS, and Windows archives for amd64 and arm64, inject version metadata, generate SHA-256 checksums, and create a GitHub Release.

## 14. Out of scope

- modifying source code or generating patches;
- line comments on GitHub, GitLab, or Gitee;
- whole-repository scan mode;
- asynchronous memory compression or a global token budget;
- JSONL session viewers;
- MCP, skills, or external network tools;
- telemetry services;
- a web UI or persistent service.
