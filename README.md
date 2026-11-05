# frz

A Gemini-CLI-style multi-provider chat client for the terminal, with a carefully
crafted Markdown rendering experience. Available in Go and Python editions:

- **`go/`** — a single static binary (main edition, recommended)
- **`python/frz`** — the English single-file stdlib-only edition
- **`python/frz-cn`** — the Chinese-localized single-file stdlib-only edition

The Python editions also include local shell commands, file/image attachments,
content-addressed attachment storage, and automatic web search. Go support for
these newer features is being synchronized.

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
  local and is not added to model context (Python editions)
- **File and image attachments**: use `@<file>` with filenames, relative paths,
  absolute paths, or quoted paths. Supports multiple attachments and Tab completion.
  Limits are 2 MiB per text file, 10 MiB per image, and 15 MiB per message (Python editions)
- **Attachment snapshots**: content-addressed blobs in `~/.frz/attachments/<sha256>`
  keep session JSON small, deduplicate content, and support legacy-session migration
  and safe cleanup (Python editions)
- **Automatic web search**: Responses models decide when to search. New Responses
  sessions default to enabled; control it with `/web on|off` or `--web/--no-web`.
  Search activity and thinking status are shown while waiting (Python editions)
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
| `/web [on\|off]` | View or control automatic web search |
| `/edit` | Compose a multi-line message in `$EDITOR` |
| `/history [N]` | Show history (last N rounds only; long output is paged) |
| `/clear` | Clear current session history |
| `/exit` | Save and quit |

## Local commands and attachments (Python editions)

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

The Python edition includes feature and regression tests for local commands,
attachments, storage, web search, sessions, permissions, and SSE error handling:

```bash
python3 -I -m unittest discover -s python/tests -v
```

## License

MIT © Frank Zhu
