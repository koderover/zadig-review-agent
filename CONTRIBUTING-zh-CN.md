# 为 Zadig Review Agent 做贡献

感谢你帮助改进 Zadig Review Agent。本指南适用于代码、文档、规则、测试和问题报告。

[English](CONTRIBUTING.md) | 简体中文

## 开始之前

- 创建 Issue 前先搜索已有 Issue 和 Pull Request，避免重复。
- 大型功能、行为变化、新依赖或公共接口变化应先通过 GitHub Issue 讨论。
- 安全漏洞请按 [SECURITY.md](SECURITY.md) 私下报告。
- 遵守[社区行为准则](CODE_OF_CONDUCT-zh-CN.md)。

## 开发环境

需要 Go 1.24 或更高版本以及 Git。

```bash
git clone https://github.com/koderover/zadig-review-agent.git
cd zadig-review-agent
go mod download
make check
```

`make help` 会列出常用目标。`make fmt` 会改写 Go 文件，`make fmt-check` 只进行检查。

## 提交改动

1. Fork 仓库，并基于 `main` 创建目标明确的分支。
2. 保持改动便于审查，不要混入无关格式化或重构。
3. 为可观察行为添加或更新测试。
4. 用户可见行为变化时，同时更新中英文文档。
5. 创建 Pull Request 前运行 `make check`。

项目遵循常规 Go 约定。文件过滤、结果校验、报告和退出策略应保持确定性。仓库内容、diff、规则和模型输出都必须视为不可信输入。除非经过单独的产品范围讨论，否则改动必须保持只读安全边界。

## 测试

测试不得依赖真实模型凭证或请求在线模型服务。请使用 protocol 和 reviewer 包中已有模式的本地 fake。

至少运行：

```bash
make fmt-check
go vet ./...
go test ./...
go test -race ./...
make build
```

修复缺陷时应添加回归测试。涉及安全的路径处理应按需覆盖路径穿越、符号链接、引用和边界情况。

## Pull Request

- 解释问题以及所选方案为何能够解决问题。
- 关联相关 Issue。
- 说明已执行的测试和兼容性影响。
- Flags、配置、报告字段、规则或工作流变化时同步更新文档。
- 不得提交 API Key、专有源码、生成的审查报告或本地构建产物。

维护者可能要求缩小改动、补充测试或先进行设计讨论。长期无活动、与其他工作重复或不符合项目范围及安全模型的 Pull Request 可能会被关闭。

## Commit 信息

使用简短的祈使句说明改动，例如：

```text
Validate finding paths before relocation
```

项目不强制使用特定 Commit 规范。尽量在本地整理 fixup，并提供便于审查的历史。

## 贡献许可

提交贡献即表示你同意按项目的 [Apache License 2.0](LICENSE) 对其授权。请仅提交你有权贡献的内容。
