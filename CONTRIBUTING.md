# Contributing

Thanks for your interest in improving meraki-exporter. Contributions are
welcome via pull request.

## Development

Requires Go as pinned in [`go.mod`](go.mod) (the `toolchain` directive lets any
recent Go download the right version automatically).

```sh
go build ./...
go test ./... -count=1
go vet ./...
```

Linting uses [golangci-lint](https://golangci-lint.run/) v2 with the config in
[`.golangci.yml`](.golangci.yml):

```sh
golangci-lint run
```

CI runs `go vet`, tests, `golangci-lint`, and `govulncheck` on every pull
request (see [.github/workflows/ci.yml](.github/workflows/ci.yml)). Please make
sure these pass locally before opening a PR.

## Adding a collector

Collectors live in [`internal/collector/`](internal/collector/). Each one polls
a set of Meraki API endpoints and emits Prometheus metrics. Follow the structure
of an existing collector: register metric descriptors, implement the poll, and
add it to the collector registry and the default enable/slow lists as
appropriate. New opt-in collectors should default to disabled.

## Pull requests

- Keep changes focused and include tests for new behavior.
- Match the style and comment density of the surrounding code.
- Describe what the change does and why in the PR description.

## Reporting bugs

Open a GitHub issue with the exporter version, your Go version, relevant log
output (`LOG_LEVEL=debug`), and steps to reproduce. For security issues, follow
[SECURITY.md](SECURITY.md) instead of opening a public issue.
