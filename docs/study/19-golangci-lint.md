# 19 — Static analysis with golangci-lint (v2)

## Concepts

- golangci-lint runs many linters in one pass with shared parsing/caching.
- **v2 changed the config format** (`version: "2"`, `linters.default`, separate `formatters` section, `exclusions`). v1 configs fail; use `golangci-lint migrate` to convert old ones.
- In v2, `gosimple` and `stylecheck` were merged into `staticcheck`; formatters (gofmt, gofumpt, goimports) are no longer linters.

## `.golangci.yml` for spotd

```yaml
version: "2"

run:
  timeout: 5m

linters:
  default: standard        # errcheck, govet, ineffassign, staticcheck, unused
  enable:
    - bodyclose            # http response bodies closed
    - noctx                # http requests without context
    - contextcheck         # functions that should propagate ctx
    - errorlint            # errors.Is/As instead of == and type assertions
    - nilerr               # returning nil when err != nil
    - gosec                # security issues (file perms, weak rand, etc.)
    - sqlclosecheck        # sql.Rows / Stmt closed
    - rowserrcheck         # rows.Err() checked
    - exhaustive           # switch on enums covers all cases
    - gocritic
    - revive
    - misspell
    - unconvert
    - unparam
    - usestdlibvars
    - fatcontext           # contexts nested in loops
    - nolintlint           # nolint directives must be specific and justified
  settings:
    govet:
      enable-all: true
      disable:
        - fieldalignment   # noisy for little gain
    gosec:
      excludes:
        - G104             # covered by errcheck
  exclusions:
    generated: lax         # skip files with "Code generated ... DO NOT EDIT"
    paths:
      - gen/
      - internal/store/db/
    rules:
      - path: _test\.go
        linters: [gosec, unparam]

formatters:
  enable:
    - gofumpt
    - goimports
  settings:
    goimports:
      local-prefixes:
        - github.com/<you>/spotd
```

Commands:

```bash
golangci-lint run ./...
golangci-lint fmt              # apply formatters
golangci-lint config verify    # validate the config file
golangci-lint linters          # list enabled/disabled
```

## Linters worth understanding (interview material)

| Linter | Catches | spotd example |
|--------|---------|---------------|
| `errcheck` | ignored errors | `resp.Body.Close()` result ignored deliberately → `_ =` or `//nolint:errcheck // reason` |
| `govet` (`copylocks`, `lostcancel`, `printf`, `slog`) | copied mutex, forgotten `cancel()`, bad slog args | Broadcaster passed by value |
| `staticcheck` | bugs, deprecated APIs (`grpc.Dial`), simplifications | |
| `bodyclose` | leaked HTTP bodies | Spotify client |
| `gosec` | `0644` on secret files, `math/rand` for security, SQL string building | backup files, state |
| `errorlint` | `err == ErrX` with wrapped errors | `ErrNoContent` checks |
| `contextcheck` / `noctx` | lost cancellation | token refresh, http requests |

## Best practices

- Start strict on a new project — far cheaper than retrofitting.
- `//nolint:linter // reason` must name the linter and give a reason (`nolintlint` enforces this).
- Pin the golangci-lint version in CI; new versions add checks and can break builds unexpectedly.
- Also run `go vet ./...` and `govulncheck ./...` (vulnerability scanner for dependencies and stdlib) in CI.

## Further reading

- https://golangci-lint.run/docs/configuration/file/
- https://golangci-lint.run/docs/linters/
- https://golangci-lint.run/docs/product/migration-guide/
- https://go.dev/doc/security/vuln/
