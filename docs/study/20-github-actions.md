# 20 — CI with GitHub Actions

## Goals

On every push / PR:
1. Generated code is up to date (buf, sqlc).
2. Proto contract has no breaking changes (PRs).
3. Lint passes.
4. Tests pass with `-race` on Linux and macOS (keyring/UDS paths differ).
5. No known vulnerabilities (`govulncheck`).
6. Release builds still work (`goreleaser check` / snapshot).

On tags `v*`: run GoReleaser (21).

## `.github/workflows/ci.yml`

> Action major versions below were current as of late 2026 — check each action's README for the latest, and consider pinning to commit SHAs for supply-chain safety.

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  generate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          fetch-depth: 0              # buf breaking needs history
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - uses: bufbuild/buf-action@v1
        with:
          setup_only: true
      - uses: sqlc-dev/setup-sqlc@v4
        with:
          sqlc-version: "1.30.0"      # pin; check latest
      - run: buf lint
      - run: buf format --diff --exit-code
      - if: github.event_name == 'pull_request'
        run: buf breaking --against "https://github.com/${{ github.repository }}.git#branch=main"
      - run: buf generate && sqlc generate
      - name: Generated code is committed
        run: git diff --exit-code

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - uses: golangci/golangci-lint-action@v8
        with:
          version: v2.5             # pin a v2 version
      - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...

  test:
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - run: go build ./...
      - run: go test -race -shuffle=on -count=1 -coverprofile=cover.out ./...
      - if: matrix.os == 'ubuntu-latest'
        uses: actions/upload-artifact@v4
        with:
          name: coverage
          path: cover.out

  release-dry-run:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --snapshot --clean
```

## Best practices

- `permissions: contents: read` at the top; grant more only in the release workflow (`contents: write`).
- `go-version-file: go.mod` — one source of truth for the Go version. `setup-go` caches modules and build cache by default.
- `concurrency` cancels superseded runs.
- Tests must not need secrets or network (keyring mocked, Spotify faked).
- Pin tool versions (golangci-lint, sqlc, buf plugins) for reproducibility; update via Dependabot:

```yaml
# .github/dependabot.yml
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule: { interval: weekly }
  - package-ecosystem: github-actions
    directory: /
    schedule: { interval: weekly }
```

- Add a status badge to the README.

## Pitfalls

- `buf breaking` with a shallow clone → can't find `main`.
- macOS runners and UDS path length (`t.TempDir()` paths are long) → use `os.MkdirTemp("", "s")` for socket tests.
- Using `pull_request_target` with untrusted code checkout → secret exfiltration risk. Use plain `pull_request`.

## Further reading

- https://docs.github.com/actions
- https://github.com/actions/setup-go
- https://github.com/golangci/golangci-lint-action
- https://buf.build/docs/bsr/ci-cd/github-actions/
- https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions
