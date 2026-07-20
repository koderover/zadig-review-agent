# Zadig Review Agent 设计文档

[English](DESIGN.md) | 简体中文

## 1. 目标与边界

`zadig-review-agent` 是一个面向本地开发和 Zadig/CI 工作流的只读代码审查 CLI。它读取本地 Git 仓库中的变更，通过大语言模型识别具体缺陷，并生成：

- 控制台结果；
- JSON 报告；
- Markdown 报告；
- 可用于 CI 门禁的确定性退出码。

当前版本不会修改仓库、执行任意 Shell、访问外部 MCP、加载 Skill，也不会向 GitHub、GitLab 或 Gitee 回写评论。

审查关注正确性、安全、并发、资源管理、性能、兼容性和关键测试缺失。纯格式、命名偏好和没有实际风险的风格问题默认不报告。

## 2. 总体架构

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

当前代码模块：

- `internal/cli`：命令解析、配置覆盖、preview、报告目录和进度输出。
- `internal/config`：用户 YAML 配置、环境变量覆盖、`config get/set/show/path`。
- `internal/gitdiff`：workspace、commit、range diff 获取和 unified diff 解析。
- `internal/rules`：四层规则加载、glob 匹配、规则引用和系统规则。
- `internal/filter`：确定性文件过滤。
- `internal/protocol`：统一消息/工具协议和三个官方 SDK Adapter。
- `internal/reviewer`：分片调度、Plan、Tool Loop、上下文压缩、Relocation、Review Filter 和 finding 校验。
- `internal/reporter`：控制台、JSON、Markdown 输出。
- `internal/agent`：报告、finding、usage 和工具审计数据结构。

流程控制全部由 Go 实现，Provider Adapter 只负责协议映射。LLM 负责提出候选问题和选择只读工具，不能决定文件过滤、行号有效性、去重结果或 CI 退出码。

## 3. CLI 与 Diff 模式

主要命令：

```bash
zadig-review-agent review --from origin/main --to HEAD
zadig-review-agent review --commit <sha>
zadig-review-agent review
zadig-review-agent review --commit <sha> --preview
zadig-review-agent rules check src/main.go --rule rules.json
zadig-review-agent config show
zadig-review-agent config set model.name gpt-4o
```

三种 diff 模式：

### 3.1 Range

`--from <ref> --to <ref>` 审查 `merge-base(from,to)..to`。必须同时提供两个参数。

### 3.2 Commit

`--commit <sha>` 使用 `git show` 审查单个 commit 引入的变更。

### 3.3 Workspace

不提供 range 或 commit 参数时进入 workspace 模式：

- 有 `HEAD` 时执行一次 `git diff HEAD`，得到从 HEAD 到工作树的 staged 和 unstaged 合并结果；
- unborn repository 回退到 cached diff；
- untracked 文件单独构造 diff；
- 同一路径最终只产生一个 `FileDiff`。

Git diff/show 禁用 external diff 和 textconv，并使用稳定前缀及 `--end-of-options`。路径解析支持空格、Unicode、rename 和 Git quoted path。未跟踪符号链接只审查链接目标文本，不跟随链接读取仓库外文件。

`review.context_lines` 默认是 `3`，配置和 `--context-lines` 可以显式覆盖。

### 3.4 Preview

`--preview` 只执行 Git diff、文件过滤和规则解析：

- 输出 kept/excluded 文件、排除原因和命中的规则元数据；
- 不初始化 Provider；
- 不调用 LLM；
- 不产生 findings，也不触发质量门禁。

## 4. 用户配置

默认配置路径：

```text
~/.zadig-review-agent/config.yaml
```

完整配置示例：

```yaml
review:
  concurrency: 4
  context_lines: 3
  max_tool_rounds: 30
  max_context_tool_calls: 10
  max_chunk_tokens: 12000
  confidence_threshold: 0.75
  fail_on:
    - critical
    - high

model:
  protocol: openai
  name: configured-model
  endpoint: https://api.openai.com/v1
  api_key: sk-...
  timeout: 120s

output:
  json: review-report.json
  markdown: review-report.md
  console: detailed
  language: zh-CN
  progress: true
```

配置优先级：

```text
内置默认值 < ~/.zadig-review-agent/config.yaml < ZADIG_REVIEW_MODEL_* < review CLI flags
```

模型环境变量：

```bash
export ZADIG_REVIEW_MODEL_PROTOCOL=openai
export ZADIG_REVIEW_MODEL_NAME=gpt-4o
export ZADIG_REVIEW_MODEL_ENDPOINT=https://api.openai.com/v1
export ZADIG_REVIEW_MODEL_TIMEOUT=120s
export ZADIG_REVIEW_MODEL_API_KEY=...
```

`config show` 会隐藏 API Key；`config get model.api_key` 返回真实值。正式 review 检测到默认模型占位值时返回配置错误，preview 不要求模型配置。

审查控制、模型协议/名称/Endpoint/Timeout 和输出设置可通过 review flags 覆盖；API Key 只通过配置文件或环境变量传入。

## 5. Rules

Rules 是普通声明式数据，不具备执行能力。加载层级按优先级排列：

1. `--rule <path>`；
2. `<repo>/.zadig-review/rules.json`；
3. `~/.zadig-review/rules.json`；
4. 嵌入式 `internal/rules/system_rules.json`。

指定 `--rule` 不会阻止 project、global 和 system 层加载。每层按声明顺序匹配，第一条命中的规则生效；当前层没有命中时继续下一层。

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

Glob 匹配大小写不敏感，支持 `**`、`?`、字符类和 `{a,b}` 展开。`**` 匹配任意层级路径。系统规则最后的 `path: "**"` 加载 `default.md`，只为没有语言或文件类型专用规则的文件提供兜底。

`rule` 可以是内联文本，也可以引用 `.md`、`.txt` 或 `.markdown`：

- custom 引用相对 `--rule` 文件目录解析；
- project 引用相对仓库根目录解析；
- global 引用相对 `~/.zadig-review/` 解析；
- 引用上限 512 KiB；
- 拒绝路径逃逸和解析后扩展名不受支持的符号链接；
- 引用失败记录非阻断 warning，并继续向下一规则层 fallback。

`merge_system_rule: true` 将匹配的系统规则和用户规则合并。过滤配置取最高优先级且实际包含 include/exclude 的规则层，不跨层合并。

## 6. 文件过滤

文件按以下顺序处理：

1. 非法或逃逸路径：`invalid_path`；
2. 二进制：`binary`；
3. 已删除文件：`deleted`；
4. 用户 exclude：`user_exclude`；
5. 用户 include：直接保留；
6. 不支持的扩展名或 basename：`unsupported_ext`；
7. 默认测试路径：`default_path`。

`include` 是 bypass，不是白名单：它允许文件绕过扩展名 allowlist 和默认测试路径排除，但不能绕过非法路径、binary、deleted 和 user exclude。

内置 basename 支持 `Dockerfile`、`Makefile`、`pom.xml`、`build.gradle`、`package.json` 和 `Cargo.toml`，其余类型按扩展名 allowlist 判断。

## 7. 模型协议

支持三个调用协议：

| protocol | 官方 SDK | API |
|---|---|---|
| `openai` | `github.com/openai/openai-go/v3` | Chat Completions |
| `gemini` | `google.golang.org/genai` | generateContent |
| `anthropic` | `github.com/anthropics/anthropic-sdk-go` | Messages |

统一请求结构包含：

- 标准化 `system`、`user`、`assistant`、`tool` 消息；
- `ToolDefinition`；
- assistant `ToolCall` 和对应 `ToolResult`；
- 无工具调用后的 `RequireTool` 标记。

Main Loop 只接受官方 SDK 返回的原生 function/tool call，不解析文本 action JSON，也没有 response repair 或 findings-to-action 兼容路径。

首次请求允许模型自主选择工具。模型只返回文本时，Reviewer 追加提醒，并在后续请求强制工具调用：

- OpenAI：`tool_choice=required`；
- Gemini：`functionCallingConfig.mode=ANY`；
- Anthropic：`tool_choice=any`。

如果模型从未调用过工具，连续三轮仍没有工具调用时记录 `tool_loop_empty_limit_reached`，审查标记为不完整。如果模型已经完成过有效工具活动，后续无工具响应会强制重试一次；重试仍返回空响应，或返回非空自然语言但没有工具调用时，将其视为模型 end turn，避免兼容端点忽略 `tool_choice` 或返回空 completion 时误判失败。达到 `max_tool_rounds` 时记录 `tool_loop_limit_reached`。

SDK 自带重试关闭。Reviewer 对超时、HTTP 408、429 和 5xx 统一重试一次；认证和参数错误不重试。每个实际请求都计入 `LLM Requests`。

## 8. Prompt 与 Tool Loop

Plan、Main、Relocation 和 Review Filter 分别使用独立 system/user Prompt，并通过 `go:embed` 从 `internal/reviewer/prompts/` 加载：

- 安全边界、工具规则和输出约束只进入 system message；
- diff、rules、其他变更文件和候选 finding 只进入 user message；
- 工具输出只进入 tool message；
- 仓库数据始终声明为不可信输入。

单文件 changed lines 达到 50 时执行无工具 Plan；小于 50 行时跳过。大 hunk 根据 `max_chunk_tokens` 分片。每次模型请求前执行约 80% 的本地 token guard；该 guard 是粗略容量保护，正式 Usage 以 Provider 响应为准。

每个文件默认最多执行 10 次 `file_read`、`code_search` 或 `file_find`，由 `review.max_context_tool_calls` 或 `--max-context-tool-calls` 调整。该值是上限：不超过 10 行的变更最多使用 6 次，11 至 50 行最多使用 8 次，更大变更使用配置上限。预算耗尽后，下一轮只向模型暴露 `code_comment` 和 `task_done` 并强制进入收敛阶段，避免小 diff 因重复搜索产生大量 Tool Calls 和 Token 消耗。

原生工具定义位于 `internal/reviewer/tools.json`：

- `file_read`：从 post-change snapshot 读取最多 500 行；
- `code_search`：通过 `git grep` 搜索 tracked files，支持 Git pathspec、大小写和 PCRE；
- `file_find`：按 basename 关键字查找文件，不是 glob；
- `code_comment`：提交一个当前文件的候选 finding；
- `task_done`：明确结束当前文件审查。

每轮 assistant tool calls 和所有对应 tool results 都保留在下一次请求中。工具输出最大 32 KiB，截断时保持有效 UTF-8。

单个文件/chunk 会话内对参数完全相同的 `file_read`、`code_search` 和 `file_find` 调用做确定性去重；`file_read` 请求的行区间已被此前读取范围完整覆盖时也会命中缓存。缓存命中不重复执行、不消耗 Context Tool Budget，并重新注入真实缓存内容，确保上下文压缩后仍可使用。`code_search` 定位符号后再用 `file_read` 获取上下文属于不同操作，不会被去重。不同文件 subtask 的上下文彼此独立，不共享工具结果缓存。

### 8.1 上下文压缩

Main Loop 在每次模型请求前估算当前消息和工具定义的 Token。达到 `max_chunk_tokens` 的 60% 且存在可压缩的历史轮次时，执行一次同步 LLM 摘要请求：

- system message 和初始 user message 固定保留；
- 较旧的完整 assistant/tool 轮次进入摘要；
- 最近两个完整轮次保持原始消息和 Tool Call ID，不拆散 assistant tool call 与 tool result；
- 如果少量超大工具结果直接达到 80% 硬阈值，允许摘要全部已完成轮次，避免尚未积累三个轮次就直接失败；
- 摘要以独立的 `<previous_review_summary>` user message 放回上下文，然后继续原生 Tool Loop。

压缩输入使用结构化 JSON，保留 assistant 工具名称、参数、Tool Call ID 和工具结果。压缩请求不携带工具定义，不允许调用仓库工具。压缩产生的 Prompt、Completion、Cache Token 和 LLM Requests 全部计入本次 review Usage。

压缩失败、返回空摘要或摘要未缩短上下文时保留原始消息，并对当前文件禁用后续压缩，避免每轮重复支付失败成本。后续仍由 80% 本地 Token guard 和上下文工具调用预算终止失控循环。第一版压缩是按文件同步执行的，不共享并发状态，也不实现 OCR 的后台异步压缩。

Commit/Range 模式的 `file_read` 从被审查 ref 读取，Workspace 模式读取当前工作树。所有文件访问都经过相对路径清理和仓库根目录约束；不提供 Shell、网络、写文件或任意 Git 命令工具。

## 9. Finding 验证

`code_comment` 产生候选 finding：

```json
{
  "severity": "high",
  "category": "correctness",
  "rule_id": "optional-rule-id",
  "file": "internal/order/service.go",
  "start_line": 82,
  "end_line": 87,
  "existing_code": "if current.Status == pending {",
  "title": "并发更新可能覆盖订单状态",
  "problem": "读取和更新之间没有版本检查。",
  "evidence": "变更后的代码执行无条件更新。",
  "suggestion": "使用乐观锁并检查受影响行数。",
  "confidence": 0.93
}
```

`severity` 和 `category` 在最终验证前统一转为小写。工具 schema 将 category 限制为 `correctness`、`security`、`concurrency`、`performance`、`compatibility` 和 `tests`；对少数常见兼容值做确定性归一化，例如 `Error Handling`、`reliability` 映射为 `correctness`，`test coverage` 映射为 `tests`。未知类别仍会被丢弃。

Review Filter 完成后，如果输出语言不是 English，Reviewer 使用独立的无工具 Localization Prompt 批量本地化 `title`、`problem`、`evidence` 和 `suggestion`。该阶段只能按候选 ID 改写人类可读字段，不能修改文件、行号、severity、category、confidence 或 finding 数量。Filter 和 Localization 接受裸数组及有限的常见包装对象；响应非法时追加严格格式提示重试一次。重试后仍失败则保留原 finding、记录 warning，并将审查标记为不完整。

处理顺序：

1. 优先用 `existing_code` 在新侧 hunk 中进行精确连续片段定位；
2. 已有行号与 changed line 重叠时直接接受位置；
3. 无法定位时调用 Relocation，返回新的精确 `existing_code`；仍无法定位的单个候选会被丢弃，但不会使整个 review 变为不完整；
4. 为候选分配 `c-0`、`c-1` 等稳定临时 ID；
5. Review Filter 只返回需要删除的 ID；
6. Filter 只有在 diff 提供直接反证时才应删除，不能改写 finding；
7. Filter 超时或响应非法时保留原 findings、记录 warning，并将审查标记为不完整；
8. 最终再次校验 changed line、严重程度、置信度和路径，然后生成 fingerprint、去重并聚合。

允许的 severity 为 `critical`、`high`、`medium`、`low`。低于 `confidence_threshold` 的 finding 被丢弃。质量门禁只由本地 `fail_on` 策略决定。

## 10. 报告与过程输出

默认报告目录：

```text
~/.zadig-review-agent/reports/<flattened-repository-path>/<review-id>/
```

仓库分组使用 Git 顶层绝对路径压平，例如：

```text
/home/developer/projects/zadig
-> home-developer-projects-zadig
```

Review ID 使用 UTC 秒级时间戳和 diff 标识；同一秒冲突时追加 `-2`、`-3`。已有报告不迁移。相对 `output.json` 和 `output.markdown` 写入本次 review 目录，绝对路径直接使用。报告权限为 `0600`。

JSON 顶层保存 metadata、stats、findings、excluded_files、resolved_rules、warnings、errors、usage、duration_ms 和 process。Markdown 保存可读摘要、finding、规则和轻量工具索引。`process.compressions` 记录每次压缩的文件、轮次、状态、压缩前后估算 Token、消息数量、耗时、错误和独立 Usage。`process.model_responses` 保存 Plan、上下文压缩、Relocation、Review Filter 和 Localization 的阶段、尝试次数、原始文本、结束原因、耗时、错误及 Usage，用于定位空响应、截断和结构错误；该详细信息不输出到控制台或 Markdown。

Token Usage 完全来自 Provider 响应：

| 字段 | 含义 |
|---|---|
| Prompt Tokens | 输入 token，包含 Provider 报告的缓存输入 |
| Completion Tokens | 模型输出；Gemini 包含 thinking 差额 |
| Total Tokens | Provider total，缺失时使用 prompt + completion |
| LLM Requests | 所有实际请求，包括失败和重试 |
| Cache Read Tokens | Provider 响应中的缓存读取 |
| Cache Write Tokens | Provider 响应中的缓存创建；不支持时为 0 |

Progress 默认开启并写入 stderr，正式报告写入 stdout。输出顺序固定为：

1. `Code review` 和 changed files/chunks/excluded；
2. `Review process`；
3. `Review result`、duration、usage、warnings、findings 和 exit code。

进度输出保持简洁，完整参数和工具输出只进入 JSON 的 `process.tool_calls`。示例：

```text
[progress] code_search "TestResultPassRate" in repository (113ms): 7 matches
[progress] file_read "pkg/types/step/step_junit_report.go" (91ms): lines 1-50
```

`process.tool_calls` 记录调用 ID、文件、轮次、完整参数、耗时、状态、原始输出字节数、截断标记、摘要和受限后的工具输出。

## 11. 完整性与退出码

| 退出码 | 含义 |
|---|---|
| `0` | 审查完整，且没有 finding 命中 `fail_on` |
| `1` | 审查完整，且至少一个 finding 命中 `fail_on` |
| `2` | 配置、Git、Provider、模型协议、过滤或任一分片审查不完整 |
| `130` | context 被取消 |

以下 warning 会使报告不完整，包括但不限于：

- token threshold exceeded；
- tool loop 无工具调用或达到轮次上限；
- Review Filter 失败或响应非法；
- Relocation 请求失败或响应非法。

规则引用失败会记录 warning，但当前设计不把该 warning 单独视为审查不完整；resolver 会继续 fallback。

不完整审查可以保存部分 findings 和报告，但不能以通过状态结束。

## 12. 安全性

- 仓库内容、diff、rules、工具结果和候选 finding 都是非可信数据。
- system message 不包含仓库数据。
- 不执行仓库中的命令或配置。
- 文件读取拒绝绝对路径、父目录逃逸和仓库外符号链接。
- untracked 符号链接不跟随目标。
- rules 引用限制路径、扩展名和大小。
- API Key 不进入 Prompt、控制台或报告。
- 报告和配置文件使用受限权限。
- 模型 finding 必须通过真实 diff 行号校验。
- 审查始终只读。

## 13. 测试与构建

```bash
make fmt
make test
make build
```

测试覆盖：

- 三种 diff 模式、unborn HEAD、staged/unstaged 合并、untracked 和特殊路径；
- rules 四层优先级、glob、引用、merge 和过滤语义；
- 三个 Provider 的消息、原生 tool call/result、usage 和强制工具选项映射；
- 多轮工具对话、连续无工具调用、并发 usage 和工具审计；
- Relocation、ID-only Review Filter、finding 定位和去重；
- preview、CLI override、报告目录、控制台/JSON/Markdown；
- 路径穿越、符号链接和输出截断。

发布前应运行：

```bash
go test ./...
go test -race ./...
make build
```

## 14. 当前范围外

- 修改源代码或生成补丁；
- GitHub/GitLab/Gitee 行级评论；
- 全仓库 scan 模式；
- 异步 memory compression 和全局 token budget；
- JSONL session viewer；
- MCP、Skill 和外部网络工具；
- 遥测服务；
- Web UI 和持久化服务。
