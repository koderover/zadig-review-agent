# Contributing to Zadig Review Agent

Thank you for helping improve Zadig Review Agent. This guide applies to code, documentation, rules, tests, and issue reports.

English | [简体中文](CONTRIBUTING-zh-CN.md)

## Before you start

- Search existing issues and pull requests before opening a duplicate.
- Use a GitHub issue to discuss large features, behavior changes, new dependencies, or public interface changes before implementation.
- Report security vulnerabilities privately according to [SECURITY.md](SECURITY.md).
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Development setup

You need Go 1.24 or later and Git.

```bash
git clone https://github.com/koderover/zadig-review-agent.git
cd zadig-review-agent
go mod download
make check
```

Useful targets are listed by `make help`. `make fmt` rewrites Go files; `make fmt-check` only checks them.

## Making a change

1. Fork the repository and create a focused branch from `main`.
2. Keep the change small enough to review and avoid unrelated formatting or refactors.
3. Add or update tests for observable behavior.
4. Update both English and Chinese user documentation when user-facing behavior changes.
5. Run `make check` before opening a pull request.

The project uses normal Go conventions. Prefer deterministic behavior in filtering, validation, reporting, and exit policy. Treat repository content, diffs, rules, and model output as untrusted input. Changes must preserve the read-only boundary unless a separately discussed proposal explicitly changes the product scope.

## Tests

Tests must not require real model credentials or make live provider requests. Use local fakes such as those already present in the protocol and reviewer packages.

At minimum, run:

```bash
make fmt-check
go vet ./...
go test ./...
go test -race ./...
make build
```

Add regression tests for bug fixes. Security-sensitive path handling should cover traversal, symlink, quoting, and boundary cases where relevant.

## Pull requests

- Explain the problem and why the chosen approach solves it.
- Link related issues.
- Describe tests performed and any compatibility impact.
- Update documentation for flags, configuration, report fields, rules, or workflows.
- Do not include API keys, proprietary source, generated reports, or local build artifacts.

Maintainers may ask for a smaller change, additional tests, or design discussion. A pull request can be closed when it is inactive, duplicates another effort, or conflicts with the project's scope or security model.

## Commit messages

Use a short imperative subject that explains the change, for example:

```text
Validate finding paths before relocation
```

There is no required commit-message framework. Keep fixups local and present a reviewable history when practical.

## Licensing

By submitting a contribution, you agree that it is licensed under the [Apache License 2.0](LICENSE), consistent with the rest of the project. Only submit work you have the right to contribute.
