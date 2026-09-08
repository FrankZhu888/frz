# frz

A Gemini-CLI-style multi-provider chat client for the terminal, with a carefully
crafted Markdown rendering experience. Available in two fully equivalent editions:

- **`go/`** — a single static binary (main edition, recommended)
- **`python/`** — a single-file stdlib-only script (reference / hacking edition)

## Features

- **Four provider protocols**: Anthropic (Claude), OpenAI, Google Gemini, and the
  OpenAI **Responses** API (used by providers such as Volcengine Ark / Kimi) with
  true streaming typewriter output
- **Terminal Markdown rendering**: headings, bold/italic, inline code, lists,
  quotes, horizontal rules, fenced code blocks with syntax highlighting (chroma /
  Pygments), and GitHub-style tables with full borders and CJK-aware column widths
- **ASCII-art box realignment**: when the model hand-draws `┌─┐ │ └─┘` diagrams
  (nested boxes, side-by-side boxes), frz detects ragged right edges caused by
  CJK width miscounts and pads them straight — insert-only, never deletes content
- **Sessions**: save / resume / list / rename / export to Markdown, stored in
  `~/.frz/sessions/*.json`
- **Comfortable REPL**: slash commands, Tab completion (commands & session
  names), `/edit` for multi-line input in `$EDITOR` (great for pasting long
  logs), `/history [N]` with automatic paging, and a shimmer thinking spinner
- **Graceful degradation**: plain text when piped, `TERM=dumb`, or `NO_COLOR` is set

## Install

### Pre-built binary (recommended)

Download the archive for your platform from
[Releases](https://github.com/FrankZhu888/frz/releases), then:

```bash
chmod +x frz-* && sudo mv frz-* /usr/local/bin/frz
```

### go install

```bash
go install github.com/FrankZhu888/frz/go@latest
```

### Python edition (single file, stdlib only)

```bash
curl -L -o ~/bin/frz https://raw.githubusercontent.com/FrankZhu888/frz/main/python/frz
chmod +x ~/bin/frz
# Optional enhancements (syntax highlighting, GNU readline on macOS):
pip install pygments gnureadline
# ...or simply run it with uv, which auto-installs the declared dependencies:
uv run python/frz
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
| `/edit` | Compose a multi-line message in `$EDITOR` |
| `/history [N]` | Show history (last N rounds only; long output is paged) |
| `/clear` | Clear current session history |
| `/exit` | Save and quit |

## License

MIT © Frank Zhu
