# webrudder — app

The webrudder engine: a Go daemon that drives a headless Chromium over the Chrome DevTools Protocol (CDP) and exposes the live browser as an **HTTP API** plus a **Swagger UI** console on a local port (default `10000`).

See the [root README](../README.md) for the full design, API reference, and open decisions.

## Build & Run

```bash
go build -o webrudder .            # build the binary
./webrudder https://example.com   # launch on :10000 (entry URL optional)
```

First launch auto-downloads a Chromium build (~150 MB, cached in `~/.cache/rod`). Then open <http://localhost:10000/> for Swagger UI, or drive the API directly:

```bash
curl localhost:10000/scan
curl -X POST localhost:10000/click -d '{"ref":"e1"}'
```

## Regenerating the OpenAPI spec

The `docs/` package is generated from handler annotations. After changing endpoints or DTOs:

```bash
go install github.com/swaggo/swag/cmd/swag@latest   # once
swag init -g main.go --parseInternal -o docs
```

A `Makefile` wraps these: `make build`, `make docs`.

## Tests

```bash
go test ./...          # unit + functional (functional drives a real browser)
go test -short ./...   # unit only (no browser)
```

**Status:** v1 implemented, tested, and hardened — all endpoints live.
