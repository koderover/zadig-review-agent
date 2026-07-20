# Security Policy / 安全策略

## Supported versions

Only the latest release line and `main` are supported unless a release note states otherwise.

除非发布说明另有声明，仅支持最新版本线和 `main`。

## Reporting a vulnerability

Please do not open a public issue or pull request for a suspected vulnerability.

请勿为疑似安全漏洞创建公开 Issue 或 Pull Request。

1. Use the repository's **Security** tab and select **Report a vulnerability** to open a private GitHub Security Advisory.
2. If private vulnerability reporting is unavailable, email `security@koderover.com` with the subject `[zadig-review-agent security]`.
3. Include affected versions or commits, impact, reproduction steps, and a minimal proof of concept when safe to do so. Do not include credentials or unrelated proprietary data.

1. 通过仓库的 **Security** 页面选择 **Report a vulnerability**，创建私有 GitHub Security Advisory。
2. 如果私有漏洞报告不可用，请发送邮件至 `security@koderover.com`，标题以 `[zadig-review-agent security]` 开头。
3. 请提供受影响版本或 Commit、影响、复现步骤，以及在安全情况下可提供的最小验证示例。不要附带凭证或无关专有数据。

We will acknowledge a report as soon as maintainers are available, investigate it privately, and coordinate disclosure and remediation with the reporter. Response times are not guaranteed. Please allow a reasonable remediation period before public disclosure.

维护者会在条件允许时尽快确认报告、进行私下调查，并与报告者协调披露和修复。项目不承诺固定响应时间，请在公开披露前预留合理的修复周期。

## Security scope

Examples include repository path escape, unsafe symlink handling, credential disclosure, unintended command execution, prompt-data boundary violations, and vulnerabilities in generated reports or provider adapters. Model quality disagreements without a security impact should use a normal bug report.

安全问题包括但不限于：仓库路径逃逸、不安全的符号链接处理、凭证泄露、非预期命令执行、Prompt 数据边界突破、生成报告或 Provider Adapter 中的漏洞。单纯的模型质量分歧应使用普通 Bug 报告。
