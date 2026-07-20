# Zadig Review Agent

[![CI](https://github.com/koderover/zadig-review-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/koderover/zadig-review-agent/actions/workflows/ci.yml)
[![CodeQL](https://github.com/koderover/zadig-review-agent/actions/workflows/codeql.yml/badge.svg)](https://github.com/koderover/zadig-review-agent/actions/workflows/codeql.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

面向本地开发和 CI 工作流的只读大模型代码审查工具。

[English](README.md) | 简体中文

## 功能简介

Zadig Review Agent 读取 Git 变更，使用用户配置的大语言模型识别具体缺陷，将每条问题与真实 diff 进行校验，并生成控制台、JSON 和 Markdown 报告。审查重点包括正确性、安全、并发、资源管理、性能、兼容性和关键测试缺失。

主要特性：

- 支持审查工作区、单个提交或两个 ref 之间的变更；
- 通过官方 Go SDK 支持 OpenAI、Gemini 和 Anthropic 协议；
- 支持内置规则和仓库自定义规则；
- 只向模型提供只读仓库工具；
- 提供适合 CI 质量门禁的确定性退出码；
- 不向代码托管平台回写评论，也不修改被审查仓库。

本项目是审查辅助工具，不能替代测试、安全扫描或人工审查。模型输出可能不完整或不准确。

## 环境要求

- 从源码构建时需要 Go 1.24 或更高版本
- Git 以及一个待审查的 Git 仓库
- 一个受支持模型服务的凭证（`--preview` 不需要）

## 安装

### 使用 Go 安装

首个标签版本发布后可执行：

```bash
go install github.com/koderover/zadig-review-agent@latest
```

`go install` 支持标准 `GOPROXY` 配置。单次安装可以使用可信的企业或区域代理：

```bash
GOPROXY=https://your-go-proxy.example,direct \
  go install github.com/koderover/zadig-review-agent@latest
```

如果需要可复现安装，请使用 `@v0.1.0` 等明确版本，不要使用 `@latest`。

### 下载发布包

从 [GitHub Releases](https://github.com/koderover/zadig-review-agent/releases) 下载 Linux、macOS 或 Windows 压缩包，并使用 `checksums.txt` 校验。

### 从源码构建

```bash
git clone https://github.com/koderover/zadig-review-agent.git
cd zadig-review-agent
make build
./bin/zadig-review-agent version
```

## 快速开始

先在不访问模型的情况下预览文件过滤与规则解析结果：

```bash
zadig-review-agent review --preview
```

配置模型。建议通过环境变量提供 API Key，避免将其写入磁盘或 Shell 历史：

```bash
zadig-review-agent config set model.protocol openai
zadig-review-agent config set model.name gpt-4o
zadig-review-agent config set model.endpoint https://api.openai.com/v1
export ZADIG_REVIEW_MODEL_API_KEY='your-api-key'
```

审查当前工作区：

```bash
zadig-review-agent review
```

审查单个提交或一个范围：

```bash
zadig-review-agent review --commit <sha>
zadig-review-agent review --from origin/main --to HEAD
```

使用 `zadig-review-agent help` 和 `zadig-review-agent review --help` 查看完整命令参数。

## 配置

默认配置文件为 `~/.zadig-review-agent/config.yaml`。可以参考 [.zadig-review-agent.example.yaml](.zadig-review-agent.example.yaml)，或使用 `config set`：

```bash
zadig-review-agent config path
zadig-review-agent config show
zadig-review-agent config get model.name
zadig-review-agent config set output.language zh-CN
```

配置优先级：

```text
内置默认值 < 配置文件 < ZADIG_REVIEW_MODEL_* < review 命令参数
```

支持以下模型环境变量：

```text
ZADIG_REVIEW_MODEL_PROTOCOL
ZADIG_REVIEW_MODEL_NAME
ZADIG_REVIEW_MODEL_ENDPOINT
ZADIG_REVIEW_MODEL_TIMEOUT
ZADIG_REVIEW_MODEL_API_KEY
```

`config show` 会隐藏 API Key。`config get model.api_key` 会按设计返回真实值，请勿在日志中调用它。

## 审查规则

规则是不能执行代码的声明式 JSON 数据，按以下顺序加载：

1. `--rule <path>`
2. `<仓库>/.zadig-review/rules.json`
3. `~/.zadig-review/rules.json`
4. 内置系统规则

参考 [.zadig-review/rules.example.json](.zadig-review/rules.example.json) 和 [.zadig-review/docs/go-review.md](.zadig-review/docs/go-review.md)。可检查单个路径最终使用的规则：

```bash
zadig-review-agent rules check internal/reviewer/reviewer.go
```

## CI 用法

`--ci` 会启用精简控制台输出。在 CI 中显式指定报告路径便于上传制品：

```bash
zadig-review-agent review \
  --from origin/main \
  --to HEAD \
  --ci \
  --output-json "$PWD/review-report.json" \
  --output-md "$PWD/review-report.md"
```

退出码保持稳定：

| 退出码 | 含义 |
| --- | --- |
| `0` | 审查完整，且没有 finding 命中 `fail_on`。 |
| `1` | 审查完整，且至少一个 finding 命中 `fail_on`。 |
| `2` | 配置、Git、Provider、过滤或审查流程不完整。 |
| `130` | 进程被取消。 |

默认质量门禁在出现 `critical` 或 `high` finding 时失败，可通过 `review.fail_on` 或 `--fail-on` 调整。

## 隐私与安全

- Diff、规则文本以及通过只读工具获取的仓库内容会发送到你配置的模型 Endpoint。处理敏感代码前，请确认模型服务商的数据政策。
- JSON 报告为便于诊断会保存详细工具输出和模型原始响应，其中可能包含源码。报告和配置文件会以受限权限创建，但仍应按你的安全策略进行保护、保留和删除。
- Agent 不向模型提供 Shell、网络或文件写入工具，也不执行仓库提供的命令或配置。
- API Key 不会进入 Prompt 或报告。CI 中建议使用环境变量或密钥管理服务。
- 本项目不包含遥测服务。

发现安全漏洞时请按 [SECURITY.md](SECURITY.md) 私下报告，不要创建公开 Issue。

## 故障排查

### 提示模型未配置

设置 `model.name` 和模型服务配置，或导出相应的 `ZADIG_REVIEW_MODEL_*` 环境变量。`--preview` 无需模型凭证。

### 范围审查无法解析基准

两个 ref 必须已存在于本地。运行审查前先获取目标分支，例如 `git fetch origin main`。

### 文件被意外排除

为相同审查增加 `--preview`，查看排除原因与解析后的规则；使用 `rules check` 检查单个路径。

### 审查以退出码 2 结束

检查控制台或 JSON 报告中的 warning 和 error。模型工具循环、Token、Relocation 或过滤阶段不完整时，工具会有意阻止质量门禁通过。

## 开发

```bash
make help
make check
```

贡献流程参见 [CONTRIBUTING-zh-CN.md](CONTRIBUTING-zh-CN.md)，实现细节参见 [DESIGN-zh-CN.md](DESIGN-zh-CN.md)。

## 致谢

Zadig Review Agent 受到 [OpenCodeReview（OCR）](https://github.com/alibaba/open-code-review) 启发，并结合本地开发与 CI 场景进行了相应的设计与取舍。

## 许可证

本项目使用 [Apache License 2.0](LICENSE) 许可证。
