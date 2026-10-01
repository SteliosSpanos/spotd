# 21 — Releases with GoReleaser

## Concepts

- GoReleaser builds cross-platform binaries, packages archives, checksums, changelog, and publishes a GitHub Release from a git tag.
- Because spotd uses `modernc.org/sqlite` (no CGO), `CGO_ENABLED=0` cross-compiles to every target from one Linux runner. This is a concrete payoff of the driver choice — mention it.
- Config format v2: `version: 2` at the top. Validate with `goreleaser check`.

## `.goreleaser.yaml`

```yaml
version: 2

before:
  hooks:
    - go mod tidy
    # generated code is committed; CI already checks it's fresh

builds:
  - id: spotd
    main: ./cmd/spotd
    binary: spotd
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
    flags:
      - -trimpath
    ldflags:
      - -s -w
      - -X main.version={{.Version}}
      - -X main.commit={{.Commit}}
      - -X main.date={{.Date}}
    mod_timestamp: "{{ .CommitTimestamp }}"   # reproducible builds

archives:
  - formats: [tar.gz]
    format_overrides:
      - goos: windows
        formats: [zip]
    files:
      - README.md
      - LICENSE
      - contrib/*        # e.g. systemd user unit, launchd plist

checksum:
  name_template: checksums.txt

changelog:
  use: github
  sort: asc
  filters:
    exclude:
      - "^docs:"
      - "^test:"
      - "^chore:"
```

Optional extras (check current docs for exact keys):
- `sboms:` (Syft) and `signs:` (cosign keyless via GitHub OIDC) — supply-chain points on a CV.
- Homebrew tap / `nfpms` for `.deb`/`.rpm`.

## Release workflow

```yaml
# .github/workflows/release.yml
name: release
on:
  push:
    tags: ["v*"]

permissions:
  contents: write

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with:
          fetch-depth: 0          # changelog needs full history
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Release flow:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

Local dry run: `goreleaser release --snapshot --clean` → artifacts in `dist/`.

## Platform notes for spotd

- **Windows**: keyring works (Credential Manager); Go supports `unix` sockets on Windows 10+, but there's no `XDG_RUNTIME_DIR` — use `%LOCALAPPDATA%\spotd`. Or explicitly scope v1 to Linux/macOS and say so.
- **macOS**: unsigned binaries trigger Gatekeeper warnings when downloaded via browser; fine for a CV project, note it in the README.
- Use semantic versioning and Conventional Commits so the changelog is readable.

## Further reading

- https://goreleaser.com/quick-start/
- https://goreleaser.com/customization/builds/go/
- https://goreleaser.com/ci/actions/
- https://semver.org, https://www.conventionalcommits.org
