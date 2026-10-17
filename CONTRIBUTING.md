# Contributing

Thanks for your interest in improving the Go URL Shortener!

## Local development

Prerequisites: Go 1.22+ (the version pinned in `go.mod`), a shell.

```sh
# run directly
go run ./cmd/server                  # http://localhost:8080

# or build a binary (CGO can stay off — the SQLite driver is pure Go)
CGO_ENABLED=0 go build -o bin/server ./cmd/server
./bin/server -config config.yaml

# or everything in containers
docker compose up -d
```

Configuration comes from `config.yaml` and can be overridden with
`SHORTENER_*` environment variables (see README / docs/DEPLOY.md).

## Running tests

```sh
go vet ./...
go test -race ./...

# a single package
go test -race ./internal/httpsrv/
```

Integration tests use SQLite in temp directories, so no external services
are needed. `go test -race` is the baseline: the race detector is what the
CI runs, and a PR without it passing will not be merged.

Also worth checking before pushing:

```sh
gofmt -l -w .          # formatting (CI runs gofmt via golangci-lint)
go mod tidy            # no stray dependencies
```

## Code style

- Standard library first — ask before adding a new runtime dependency.
- Errors follow `errors.Is`/`errors.As` semantics: wrap with
  `fmt.Errorf("context: %w", err)`, export sentinels from packages.
- Logging via `log/slog`; request-scoped fields already flow through the
  middleware, don't build ad-hoc loggers in handlers.
- Table-driven tests with `t.Parallel()` where possible; comments explain
  *why*, not *what*.
- No panics outside startup path — handlers return errors, middleware
  recovers anything that slips through.

## Submitting changes

1. Fork and branch (`feat/<topic>` or `fix/<topic>`).
2. Make the change with tests covering the new behavior.
3. Ensure `go build ./... && go vet ./... && go test -race ./...` is green.
4. Open a pull request describing the *problem* you solved.

Small fixes (typos, docs) don't need an issue first; behavioral changes do.

## Reporting bugs

Open an issue with: Go version, OS, how you run the service, the exact
request you made, and the relevant log lines. Do not paste full database
files or click records — they may contain private data.
