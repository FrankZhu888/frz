# frz

A Gemini-CLI-style multi-provider chat client for the terminal, with a carefully
crafted Markdown rendering experience. Available in Go and Python editions:

- **`go/`** — a single static binary (main edition, recommended)
- **`python/frz`** — the English single-file stdlib-only edition
- **`python/frz-cn`** — the Chinese-localized single-file stdlib-only edition

Both the Go and Python editions include local shell commands, file/image
attachments, content-addressed attachment storage, automatic web search, and
opt-in verbose diagnostics.

## Features

- **Four provider protocols**: Anthropic (Claude), OpenAI, Google Gemini, and the
  OpenAI **Responses** API (used by providers such as Volcengine Ark / Kimi) with
  true streaming typewriter output
- **Terminal Markdown rendering**: headings, bold/italic, inline code, lists,
  quotes, horizontal rules, fenced code blocks with syntax highlighting (chroma /
  Pygments), and GitHub-style tables with full borders and CJK-aware column widths
- **ASCII-art box realignment**: when the model hand-draws diagrams
  (nested boxes, side-by-side boxes), frz detects ragged right edges caused by
  CJK width miscounts and pads them straight — insert-only, never deletes content
- **Sessions**: save / resume / list / rename / export to Markdown, stored in
  `~/.frz/sessions/*.json` using atomic writes and owner-only permissions
- **Local commands**: run `!<command>` or bare `ls` / `pwd` / `cd`; output stays
  local and is not added to model context (Go and Python editions)
- **File and image attachments**: use `@<file>` with filenames, relative paths,
  absolute paths, or quoted paths. Supports multiple attachments and Tab completion.
  Limits are 2 MiB per text file, 10 MiB per image, and 15 MiB per message (Go and Python editions)
- **Attachment snapshots**: content-addressed blobs in `~/.frz/attachments/<sha256>`
  keep session JSON small, deduplicate content, and support legacy-session migration
  and safe cleanup (Go and Python editions)
- **Automatic web search**: Responses models decide when to search. New Responses
  sessions default to enabled; control it with `/web on|off` or `--web/--no-web`.
  Search activity and thinking status are shown while waiting (Go and Python editions)
- **Opt-in diagnostics**: enable HTTP/SSE debug logs with `--verbose` or
  `FRZ_VERBOSE=1`, and toggle them interactively with `/verbose on|off`. Logs go
  to stderr and are never enabled by default (Go and Python editions)
- **Comfortable REPL**: slash commands, Tab completion (commands, session names,
  and attachment paths), `/edit` for multi-line input in `$EDITOR`, `/history [N]`
  with automatic paging, and a shimmer thinking spinner
- **Graceful degradation**: plain text when piped, `TERM=dumb`, or `NO_COLOR` is set

## Install

### Pre-built binary (recommended)

Download the archive for your platform from
[Releases](https://github.com/FrankZhu888/frz/releases), then:

```bash
chmod +x frz-* && sudo mv frz-* /usr/local/bin/frz
```

> **macOS Gatekeeper note**: the release binaries are unsigned (built by CI),
> so macOS may refuse to open them ("Apple cannot check it for malicious
> software"). Run this once after downloading, or right-click → Open:
>
> ```bash
> xattr -d com.apple.quarantine frz-darwin-arm64
> ```
>
> Linux binaries are unaffected, and so is anything you build yourself
> (`go install` below compiles locally and never trips Gatekeeper).

### go install

```bash
go install github.com/FrankZhu888/frz/go@latest
```

### Python editions (single file, stdlib only)

Install the English edition:

```bash
curl -L -o ~/bin/frz https://raw.githubusercontent.com/FrankZhu888/frz/main/python/frz
chmod +x ~/bin/frz
# Optional enhancements (syntax highlighting, GNU readline on macOS):
pip install pygments gnureadline
# ...or run it with uv, which auto-installs the declared dependencies:
uv run python/frz
```

Install the Chinese-localized edition as `frz-cn`:

```bash
curl -L -o ~/bin/frz-cn https://raw.githubusercontent.com/FrankZhu888/frz/main/python/frz-cn
chmod +x ~/bin/frz-cn
# Optional enhancements:
pip install pygments gnureadline
```

### Build from source

```bash
cd go && ./build.sh   # produces frz-go + linux/darwin amd64/arm64 binaries
```

## Quick start

```bash
# Store your credentials once (per-provider keys/models/base URLs)
frz config set --provider anthropic --api-key sk-ant-xxxx
frz config set --provider openai_responses \
    --model kimi-k3 --base-url https://ark.cn-beijing.volces.com/api/v3 \
    --api-key ark-xxxx

frz                      # start chatting with the default provider
frz --provider openai --model gpt-4o
frz --resume my-session  # resume a saved session
frz --list-sessions
frz --web                # explicitly enable automatic web search
frz --no-web             # start a new session with web search disabled
frz --verbose            # enable HTTP/SSE diagnostics from the first request
```

API key resolution order: `--api-key` > environment variable > config file
(`ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GEMINI_API_KEY` / `ARK_API_KEY`).

## REPL commands

| Command | Description |
|---|---|
| `/save [name]` | Save current session (refuses to overwrite another session) |
| `/rename <name>` | Rename current session |
| `/resume [name]` | Switch to another session (latest one if omitted) |
| `/list` | List saved sessions |
| `/new [name]` | Start a fresh session |
| `/export [file]` | Export current session to Markdown |
| `/system [prompt]` | View or set the system prompt |
| `/model [name]` | View or switch model |
| `/baseurl [url]` | View or set the API base URL |
| `/web [on\|off]` | View or control automatic web search (saved with the session) |
| `/verbose [on\|off]` | View or toggle debug logs (current process only) |
| `/edit` | Compose a multi-line message in `$EDITOR` |
| `/history [N]` | Show history (last N rounds only; long output is paged) |
| `/clear` | Clear current session history |
| `/exit` | Save and quit |

## Web search and diagnostics

Startup options establish the initial state; REPL commands change it while
chatting. Their persistence rules are intentionally different:

| Setting | Startup | REPL | Scope |
|---|---|---|---|
| Web search | `--web` / `--no-web` | `/web [on\|off]` | Saved with the session |
| Verbose logging | `--verbose` or `FRZ_VERBOSE=1` | `/verbose [on\|off]` | Current process only; not saved |

Web search is enabled by default for new Responses sessions. Enabling it allows
the model to use the search tool; **it does not force a search on every request**.
Use `/web` without an argument to check the current session's setting.

Verbose logging is off by default. Enable it from the first request, or toggle it
inside the REPL:

```bash
frz --resume my-session --verbose
FRZ_VERBOSE=1 frz --resume my-session
# Accepted environment values: 1, true, yes, on (case-insensitive)
```

```text
/verbose on     # enable HTTP/SSE diagnostics
/verbose        # show the current state
/verbose off    # disable diagnostics
```

Logs go to **stderr** and include HTTP status and timing, request IDs such as
`X-Request-Id` / `X-Tt-Logid`, web-search state, SSE event details, and network
or premature-EOF errors. Upstream failures retain available response IDs and
error codes instead of collapsing into `unknown error`. To save diagnostics:

```bash
frz --resume my-session --verbose 2>frz-debug.log
```

API keys, authorization headers, and conversation request bodies are not
intentionally logged. Text/reasoning deltas are represented by their lengths;
other SSE events are logged as JSON (up to 12,000 characters per event) and may
contain response content. Review debug logs before sharing them.

## Local commands and attachments (Go and Python editions)

```text
> !git status                 Run a command locally; output stays out of context
> ls -la                     Bare ls / pwd / cd also work
> @src/main.py Explain this file
> @./error.log Analyze this error
> @"error screenshot.png" What does this screenshot show?
```

Attachment snapshots are stored independently of session JSON. Identical content
is stored once, and legacy inline attachments are migrated automatically on save.
`frz clean` removes empty sessions and unreferenced blobs after a 24-hour grace
period. Session files use owner-only permissions and atomic replacement.

## Tests

The Go and Python editions include feature and regression tests for local
commands, attachments, storage, web search, sessions, permissions, and SSE error
handling:

```bash
# Go: 37 test functions plus race detection
cd go && go test ./... && go test -race ./...

# Python: 32 feature and regression tests
python3 -I -m unittest discover -s python/tests -v
```

## License

MIT © Frank Zhu
