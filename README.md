# webrudder

> Fast, lightweight browser automation for LLM agents. Launch a headless browser bound to a local port, then drive it over a plain HTTP API — with an interactive Swagger UI console at the root.

---

## What It Is

`webrudder <url>` starts a local daemon (the `app/` engine): it launches a headless Chromium at that URL and serves it on a localhost port. Agents and scripts drive it through an **HTTP API**; visiting the root serves **Swagger UI** — an interactive list of every endpoint with a try-it-out console. Navigation happens by *interacting* (clicking links and buttons); the daemon is a state machine that tracks the current URL, DOM, and element map. Close the terminal and the browser dies with it.

No bundled browser bloat. No per-step screenshots. No MCP layer. One static binary talking to Chromium over CDP.

---

## Repository Layout

```text
webrudder/
├── app/        # engine — Go daemon + headless Chromium (CDP) → HTTP API + Swagger UI
├── web/        # landing page (static marketing site) — not part of the daemon
└── README.md   # plan / overview (this file)
```

---

## Interfaces

| Surface | URL | For | What it does |
| --- | --- | --- | --- |
| **Swagger UI** | `http://localhost:10000/` | humans | interactive endpoint list + try-it-out console |
| **HTTP API** | `http://localhost:10000/scan`, `/click`, … | agents / scripts | programmatic control, JSON in and out |

Both hit the **same state machine**: a try-it-out call from Swagger UI and an agent's `/click` act on one live browser. Swagger UI is generated from the API's OpenAPI spec — the single source of truth for every endpoint.

---

## Why

Driving a browser through an LLM is slow when every step round-trips a screenshot: the model waits on inference, parses ~1.5k image tokens, then acts — and repeats. The browser engine speed was never the bottleneck; the protocol is. webrudder cuts the loop two ways:

- **Text-first state** — `GET /scan` returns a compact list of actionable elements (`e1 button "Login"`). The model acts by ref-id with no vision and ~50 tokens instead of ~1500.
- **Batchable** — many actions in one request (`POST /batch`) collapse N round-trips into one.

---

## How It Works

```text
Terminal:  ./webrudder https://example.com
                 ↓
   Daemon launches headless Chromium (over CDP)
   and serves http://localhost:10000
                 ↓
   Humans → open localhost:10000 → Swagger UI (endpoint list + try-it-out)
   Agents → GET /scan · POST /click · GET /read ...
                 ↓
   Daemon = state machine: current URL + DOM + element map.
   Clicking navigates; re-scan for the new page's elements.
                 ↓
   Close terminal → daemon + Chromium die cleanly
```

The URL passed at launch is just the **entry point** — the browser has to start somewhere. After that you move around by interacting; you never re-feed URLs to navigate.

---

## Using It

Start it (one terminal):

```console
$ ./webrudder https://example.com
webrudder · http://localhost:10000 · chromium pid 4821 · ctrl-c to quit
```

Drive it via API (curl, an agent, a script):

```console
$ curl localhost:10000/scan
{"elements":[{"ref":"e1","role":"link","name":"More information","href":"..."}]}

$ curl localhost:10000/read
{"url":"https://example.com/","title":"Example Domain","text":"Example Domain. This domain is..."}

$ curl -X POST localhost:10000/click -d '{"ref":"e1"}'
{"ok":true,"navigated":true,"url":"https://www.iana.org/help/example-domains"}
```

Or open `http://localhost:10000/` for Swagger UI — browse every endpoint and fire requests live.

Element refs (`e1`, `e2`, …) are stable for the **current** page. After navigating, call `/scan` again to get the new page's refs.

---

## Launch Flags

```bash
./webrudder [--port N] [--downloads DIR] <url>
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--port N` | `10000` | HTTP port. Auto-increments if busy. `--port 0` = OS-assigned free port. |
| `--downloads DIR` | session temp dir | where downloaded files are saved |

Flags must come **before** the URL (Go's `flag` package stops parsing at the first positional argument). `<url>` is optional — omit it to start on a blank page, then `POST /goto`.

---

## Ports & Multiple Instances

Each `webrudder` is one browser on one port. Run as many as you like, side by side:

```bash
./webrudder --port 10000 https://example.com
./webrudder --port 10001 https://example2.com
```

- Default `10000`, auto-incrementing to the next free port if busy. The chosen port is printed on start.
- `--port N` forces a specific port; `--port 0` lets the OS pick any free one.
- **The port is the instance selector** — every request targets exactly one daemon by its port.

---

## API

Base URL: `http://localhost:<port>`

| Method & Path | Body | Returns |
| --- | --- | --- |
| `GET /scan` | — | actionable elements: `[{ref, role, name, kind?, accept?, href?}]` |
| `GET /read` | — | `{url, title, text}` |
| `GET /snap` | — | PNG bytes (`curl -o shot.png`); `POST /snap {path}` saves and returns `{path}` |
| `GET /status` | — | `{url, title, port}` |
| `POST /click` | `{ref}` | `{ok, navigated?, url?, downloaded?, needs_file?}` |
| `POST /fill` | `{ref, text}` | `{ok}` |
| `POST /goto` | `{url}` | `{ok, url}` |
| `POST /upload` | `{ref, file}` | `{ok}` — clicks `ref`, intercepts the file chooser, injects `file` |
| `POST /download` | `{ref, dir?}` | `{ok, saved}` — clicks `ref`, waits for completion, returns the saved path |
| `POST /batch` | `{actions:[…]}` | `{ok, results:[…]}` — many actions, one request |
| `POST /shutdown` | — | stops the daemon and browser |

Responses are JSON. Errors return `{ok:false, error}` with a 4xx/5xx status. The full schema lives in the OpenAPI spec that drives Swagger UI at `/`. `/click` reports `navigated`/`url`, plus `downloaded` (saved path) or `needs_file` when a click triggers a download or opens a file chooser.

### scan output

`scan` tags elements with a `kind` hint when the DOM makes their purpose knowable:

```json
{
  "elements": [
    {"ref":"e1","role":"button","name":"Login"},
    {"ref":"e5","role":"button","name":"Upload avatar","kind":"upload","accept":"image/*"},
    {"ref":"e7","role":"link","name":"Download report","kind":"download","href":".../report.pdf"}
  ]
}
```

`kind` (`upload` / `download`) appears only when statically detectable (a `<input type=file>` is present, an `<a download>` / file-extension href, etc.). Its **absence does not rule it out** — JavaScript-driven uploads and downloads are caught at click time instead (see below).

### batch

Fill a login form and submit in one request:

```console
$ curl -X POST localhost:10000/batch -d '{
  "actions":[
    {"do":"fill","ref":"e2","text":"user@example.com"},
    {"do":"fill","ref":"e3","text":"hunter2"},
    {"do":"click","ref":"e1"}
  ]}'
{"ok":true,"results":[{"ok":true},{"ok":true},{"ok":true,"navigated":true,"url":"/dashboard"}]}
```

---

## Uploads & Downloads

Both are **click-driven**, exactly like a real browser: you click a button, and a file goes in or comes out. The OS file dialog never appears — CDP intercepts it at the protocol level (no drag-and-drop, no native "Save As"). webrudder figures out what a click does in two layers.

**Layer 1 — `scan` hints.** When the DOM is explicit (file input present, `download` attribute, file href), `scan` tags the element `kind:"upload"` or `kind:"download"` so you pick the right call on the first try.

**Layer 2 — click-time detection.** The daemon always arms file-chooser interception and a download directory, so even JS-driven buttons with no DOM hint are handled. A `POST /click` reports what actually happened:

```json
{"ok":true}                                          // plain click
{"ok":true,"navigated":true,"url":"/dashboard"}      // navigation
{"ok":true,"downloaded":"/abs/downloads/report.pdf"} // a download fired
{"ok":false,"needs_file":true,"error":"click opened a file chooser — retry with /upload"}
```

So you never need to know a button's purpose in advance: use the hint when present, otherwise just click and let the response tell you.

**Upload** — `ref` is the button you click to start the upload (from `scan`), not a hidden input:

```console
$ curl -X POST localhost:10000/upload -d '{"ref":"e5","file":"./avatar.png"}'
{"ok":true}
```

**Download** — clicks, waits for the file to finish, returns the saved path:

```console
$ curl -X POST localhost:10000/download -d '{"ref":"e7"}'
{"ok":true,"saved":"/abs/downloads/report.pdf"}
```

---

## Design Principles

- **Browses, doesn't judge.** webrudder navigates, interacts, and extracts. It does *not* assert. The caller reads back text / url / elements and decides pass/fail. Assertions belong to the test framework, not here.
- **Text-first.** `scan` + `read` return cheap text; screenshots only on demand. No vision tokens per step.
- **Lightweight.** No bundled browser, no driver layer — a thin CDP client and a single static binary. Chromium (headless-shell) is fetched once, not embedded.
- **Stateful.** A long-running daemon holds the live browser, so requests operate on evolving page state. Close the terminal and everything dies cleanly.

---

## Security

webrudder is a local tool that drives a real browser and reads/writes local files as you — so access to its port is sensitive. Defenses:

- **Local only.** Binds `127.0.0.1`, never `0.0.0.0`.
- **DNS-rebinding guard.** Requests whose `Host` isn't `localhost` / `127.0.0.1` / `::1` are rejected.
- **CSRF guard.** Cross-site browser requests (`Sec-Fetch-Site: cross-site`) are rejected. curl, scripts, agents, and the Swagger UI send no cross-site header, so they're unaffected.
- **Method pinning.** Each route accepts exactly one HTTP method.
- **Navigation allowlist.** `/goto` accepts only `http`/`https` (no `file://`, `chrome://`, …). Loopback/private hosts stay allowed — testing local apps is the point.
- **Download path safety.** Page-supplied filenames are reduced with `filepath.Base`, so a malicious page can't traverse out of the downloads dir.

By design, `/upload` can attach any local file (the tool acts as you, like `curl -F`); the guards above keep a remote or web page from reaching the API. For multi-user or network-exposed setups, front it with an authenticating reverse proxy.

---

## Stack

- **Go** — single static binary, trivial cross-compile, goroutines for the concurrent CDP event loop and HTTP handlers.
- **Chrome DevTools Protocol (CDP)** — drives headless Chromium directly; no Playwright / Puppeteer bloat.
- **OpenAPI (generated from Go via swaggo) + embedded Swagger UI** — handler annotations generate the spec (`swag init`); `http-swagger` serves the console at `/` from embedded assets (no CDN). The landing page can reuse the same spec later.
- **Local HTTP daemon** — one browser per port; address instances by port.

---

## `app/` Layout

```text
app/
  go.mod / go.sum
  main.go                 # flags (url, --port, --downloads) → start daemon
  internal/
    browser/session.go    # rod/CDP session: state machine, refs, scan/click/fill/upload/download
    server/server.go      # HTTP API, routes, graceful shutdown, Swagger UI
    server/dto.go         # request/response types (also the OpenAPI schema)
  docs/                   # generated OpenAPI spec (swag init) — served at /swagger
  Makefile                # build / docs / tidy
  webrudder               # built binary
```

rod is the CDP layer (no hand-rolled `cdp/` package needed); Swagger UI assets are embedded by `http-swagger` (no `ui/` folder, no CDN).

---

## Open Decisions

Core decisions resolved — see **Status**. New questions get tracked here as they surface.

---

## Status

**v1 implemented, tested, and hardened.** The daemon launches headless Chromium (rod, auto-downloaded), serves the API + embedded Swagger UI, tracks page state, and detects what each click causes. Covered by unit + functional tests (`go test ./...` — the functional suite drives a real browser) and live-smoke-tested end to end.

**Implemented:** `/status /scan /read /snap /goto /click /fill /upload /download /batch /shutdown`; port default `10000` (auto-increment, `--port`, `--port 0`); `--downloads` dir; embedded Swagger UI. `/click` reports `navigated` / `downloaded` / `needs_file`.

**Security:** binds `127.0.0.1` only; Host-header allowlist + `Sec-Fetch-Site` CSRF guard; per-route HTTP method pinning; `/goto` restricted to http/https; download filenames sanitized against traversal. See [Security](#security).

**Locked (design):** Go + rod/CDP engine in `app/`; static Bootstrap landing in `web/`; HTTP API + Swagger UI on one port; text-first `scan`/`read`; browses-doesn't-judge (no assertions). Headed / mirror / screencast modes dropped.

---

## Releases

The binary is **not** committed — it's built and published to [GitHub Releases](https://github.com/0xRuangsak/webrudder/releases) per platform.

Cut a release by pushing a tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` then cross-compiles (linux/darwin/windows × amd64/arm64), packages `.tar.gz`/`.zip` archives plus `checksums.txt`, and publishes a GitHub Release with auto-generated notes.

Install from a release (example — macOS arm64):

```bash
curl -L -o webrudder.tar.gz \
  https://github.com/0xRuangsak/webrudder/releases/download/v0.1.0/webrudder_v0.1.0_darwin_arm64.tar.gz
tar xzf webrudder.tar.gz
./webrudder https://example.com
```

Manual alternative (build locally, publish with the `gh` CLI):

```bash
cd app && go build -trimpath -ldflags "-s -w" -o webrudder .
gh release create v0.1.0 webrudder --generate-notes
```

---

## License

MIT
