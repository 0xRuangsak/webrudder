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
./webrudder <url> [--port N] [--downloads DIR]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--port N` | `10000` | HTTP port. Auto-increments if busy. `--port 0` = OS-assigned free port. |
| `--downloads DIR` | session temp dir | where downloaded files are saved |

`<url>` is optional — omit it to start on a blank page, then `POST /goto`.

---

## Ports & Multiple Instances

Each `webrudder` is one browser on one port. Run as many as you like, side by side:

```bash
./webrudder https://example.com   --port 10000
./webrudder https://example2.com  --port 10001
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

Responses are JSON. Errors return `{ok:false, error}` with a 4xx/5xx status. The full schema lives in the OpenAPI spec that drives Swagger UI at `/`.

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

## Stack

- **Go** — single static binary, trivial cross-compile, goroutines for the concurrent CDP event loop and HTTP handlers.
- **Chrome DevTools Protocol (CDP)** — drives headless Chromium directly; no Playwright / Puppeteer bloat.
- **OpenAPI (generated from Go via swaggo) + Swagger UI** — annotations in the handlers generate the spec; it drives the interactive console at `/` and, later, the docs on the landing page.
- **Local HTTP daemon** — one browser per port; address instances by port.

---

## Proposed `app/` Layout

```text
app/
  go.mod                # module github.com/0xRuangsak/webrudder
  main.go               # flag parsing → start daemon
  internal/
    cdp/                # thin CDP client (WebSocket + JSON)
    browser/            # session + state machine
    server/             # HTTP API + OpenAPI/Swagger UI handlers
  ui/                   # embedded Swagger UI assets (go:embed)
```

---

## Open Decisions

Core decisions resolved — see **Status**. New questions get tracked here as they surface.

---

## Status

Early — core design locked, implementation not started. The API above is the v1 target.

**Locked:** Go + CDP engine in `app/` (`internal/{cdp,browser,server}` + embedded `ui/`); static Bootstrap landing in `web/`; HTTP API + **Swagger UI at `/`** on one port; default port `10000` (`--port` override, auto-increment, `--port 0`); click-driven uploads/downloads with chooser interception; text-first `scan`/`read`; browses-doesn't-judge (no assertions); OpenAPI generated from Go (swaggo); `web/` landing mirrors this README in Bootstrap. Headed / mirror / screencast modes dropped.

---

## License

MIT
