// frz - a Gemini-CLI-style multi-provider chat client for the terminal.
//
// Features:
//   - Four provider protocols: Anthropic (Claude) / OpenAI / Google Gemini /
//     OpenAI Responses API
//   - Terminal Markdown rendering (headings/bold/lists/tables/code blocks);
//     chroma syntax highlighting
//   - Hand-drawn ASCII box diagrams (┌─┐ │ └─┘) in code blocks are automatically
//     realigned, fixing ragged right edges caused by the model miscounting CJK width
//   - Sessions can be saved / resumed / listed / exported to Markdown,
//     stored in ~/.frz/sessions/*.json
//   - REPL slash commands: /help /save /rename /resume /list /new /export /edit
//     /system /model /baseurl /history /clear /exit
//   - REPL input comforts: Tab completion (commands & session names), /edit for
//     multi-line input in $EDITOR, /history with automatic paging,
//     /resume without a name resumes the most recent session
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/chzyer/readline"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

// Version and author info. version/buildTime are injected at build time via
// -ldflags -X (see build.sh); a plain `go build` shows the default dev value.
var (
	version   = "dev"
	buildTime = ""
	author    = "Frank Zhu <flankeroot@gmail.com>"
)

func shortVersion() string { return "frz " + version }

// versionString is the full version line: version + build time + platform
// (handy for checking the architecture of distributed binaries) + author.
func versionString() string {
	s := shortVersion()
	if buildTime != "" {
		s += " (" + buildTime + ")"
	}
	return fmt.Sprintf("%s %s/%s  by %s", s, runtime.GOOS, runtime.GOARCH, author)
}

var (
	appDir     = filepath.Join(os.Getenv("HOME"), ".frz")
	sessDir    = filepath.Join(appDir, "sessions")
	exportDir  = filepath.Join(appDir, "exports")
	configFile = filepath.Join(appDir, "config.json")
)

var defaultModels = map[string]string{
	"anthropic":        "claude-sonnet-4-6",
	"openai":           "gpt-4o",
	"gemini":           "gemini-2.5-flash",
	"openai_responses": "kimi-k3",
}

var envKeyNames = map[string]string{
	"anthropic":        "ANTHROPIC_API_KEY",
	"openai":           "OPENAI_API_KEY",
	"gemini":           "GEMINI_API_KEY",
	"openai_responses": "ARK_API_KEY",
}

// Default API base URLs per provider. anthropic/openai/gemini URLs are hardcoded
// in their callers; the Responses protocol may front different vendors
// (Volcengine Ark, or the official OpenAI Responses API in the future),
// so it gets an overridable default here.
var defaultBaseURLs = map[string]string{
	"openai_responses": "https://ark.cn-beijing.volces.com/api/v3",
}

// Providers with true typewriter streaming (print as generated)
var streamingProviders = map[string]bool{"openai_responses": true}

// --------------------------------------------------------------------------
// Basic utilities
// --------------------------------------------------------------------------

func ensureDirs() { os.MkdirAll(sessDir, 0o755) }

// loadConfig reads the config into a generic map (preserves all fields,
// including ones added in the future)
func loadConfig() map[string]interface{} {
	cfg := map[string]interface{}{}
	data, err := os.ReadFile(configFile)
	if err != nil {
		return cfg
	}
	if json.Unmarshal(data, &cfg) != nil {
		return map[string]interface{}{}
	}
	return cfg
}

func saveConfig(cfg map[string]interface{}) {
	ensureDirs()
	writeJSONFile(configFile, cfg)
	os.Chmod(configFile, 0o600)
}

// writeJSONFile writes JSON with indent=2 without escaping HTML/Unicode
func writeJSONFile(path string, v interface{}) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return
	}
	os.WriteFile(path, buf.Bytes(), 0o644)
}

func cfgStr(cfg map[string]interface{}, key string) string {
	s, _ := cfg[key].(string)
	return s
}

func cfgMap(cfg map[string]interface{}, key string) map[string]interface{} {
	m, _ := cfg[key].(map[string]interface{})
	return m
}

// sessionPath keeps only letters, digits and -_. in the name
// (Unicode letters such as CJK are also allowed)
func sessionPath(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	safe := b.String()
	if safe == "" {
		safe = "session"
	}
	return filepath.Join(sessDir, safe+".json")
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Session struct {
	Name         string    `json:"name"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	BaseURL      string    `json:"base_url"`
	SystemPrompt string    `json:"system_prompt"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
	Messages     []Message `json:"messages"`
}

type sessionEntry struct {
	name string
	sess *Session
}

func listSessions() []sessionEntry {
	ensureDirs()
	files, _ := filepath.Glob(filepath.Join(sessDir, "*.json"))
	type fw struct {
		path string
		mod  time.Time
	}
	var fws []fw
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			fws = append(fws, fw{f, st.ModTime()})
		}
	}
	sort.Slice(fws, func(i, j int) bool { return fws[i].mod.After(fws[j].mod) })
	var out []sessionEntry
	for _, f := range fws {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		out = append(out, sessionEntry{strings.TrimSuffix(filepath.Base(f.path), ".json"), &s})
	}
	return out
}

// saveSession saves the session; empty sessions (0 messages) are skipped unless
// force is set. Returns (path, saved).
func saveSession(s *Session, force bool) (string, bool) {
	if !force && len(s.Messages) == 0 {
		return "", false
	}
	ensureDirs()
	s.UpdatedAt = time.Now().Format("2006-01-02T15:04:05")
	path := sessionPath(s.Name)
	writeJSONFile(path, s)
	return path, true
}

func nowISO() string { return time.Now().Format("2006-01-02T15:04:05") }

func defaultSessionName() string { return time.Now().Format("session-20060102-150405") }

func renameSessionFile(oldName, newName string) (bool, string) {
	oldPath, newPath := sessionPath(oldName), sessionPath(newName)
	if _, err := os.Stat(oldPath); err != nil {
		return false, fmt.Sprintf("session %q not found", oldName)
	}
	if newPath != oldPath {
		if _, err := os.Stat(newPath); err == nil {
			return false, fmt.Sprintf("session %q already exists, pick another name", newName)
		}
	}
	data, err := os.ReadFile(oldPath)
	if err != nil {
		return false, fmt.Sprintf("session %q not found", oldName)
	}
	var s map[string]interface{}
	if json.Unmarshal(data, &s) != nil {
		return false, fmt.Sprintf("session %q is corrupted", oldName)
	}
	s["name"] = newName
	s["updated_at"] = nowISO()
	writeJSONFile(newPath, s)
	if newPath != oldPath {
		os.Remove(oldPath)
	}
	return true, newPath
}

func loadSession(name string) *Session {
	data, err := os.ReadFile(sessionPath(name))
	if err != nil {
		return nil
	}
	var s Session
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

func newSession(name, provider, model, systemPrompt, baseURL string) *Session {
	return &Session{
		Name: name, Provider: provider, Model: model, BaseURL: baseURL,
		SystemPrompt: systemPrompt, CreatedAt: nowISO(), UpdatedAt: nowISO(),
		Messages: []Message{},
	}
}

// exportSession exports the session to a Markdown file; returns (path, overwritten).
// Message contents are already Markdown, so they are dumped verbatim under role
// headings; metadata goes into a leading blockquote.
func exportSession(s *Session, dest string) (string, bool) {
	var b strings.Builder
	b.WriteString("# " + s.Name + "\n\n")
	fmt.Fprintf(&b, "> provider=%s  model=%s  created %s\n", s.Provider, s.Model, s.CreatedAt)
	if s.SystemPrompt != "" {
		fmt.Fprintf(&b, "> system: %s\n", s.SystemPrompt)
	}
	for _, m := range s.Messages {
		role := "Assistant"
		if m.Role == "user" {
			role = "User"
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", role, m.Content)
	}
	path := dest
	if path == "" {
		path = filepath.Join(exportDir, s.Name+".md")
	}
	if strings.HasPrefix(path, "~/") {
		path = filepath.Join(os.Getenv("HOME"), path[2:])
	}
	if filepath.Ext(path) == "" {
		path += ".md"
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	_, err := os.Stat(path)
	overwritten := err == nil
	os.WriteFile(path, []byte(b.String()), 0o644)
	return path, overwritten
}

// --------------------------------------------------------------------------
// API calls (unified as call(messages, system, model, apiKey) -> string)
// --------------------------------------------------------------------------

var errInterrupted = fmt.Errorf("interrupted")

func httpPostJSON(ctx context.Context, url string, headers map[string]string, payload interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return nil, errInterrupted
		}
		return nil, fmt.Errorf("network error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}
	return result, nil
}

// No overall timeout on streaming requests (long generations could exceed any
// fixed total deadline); cancellation is done via context.
var httpClient = &http.Client{}

func asMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func asSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func callAnthropic(ctx context.Context, messages []Message, system, model, apiKey, baseURL string, onDelta, onReasoning func(string)) (string, error) {
	msgs := []map[string]string{}
	for _, m := range messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	payload := map[string]interface{}{"model": model, "max_tokens": 8192, "messages": msgs}
	if system != "" {
		payload["system"] = system
	}
	result, err := httpPostJSON(ctx, "https://api.anthropic.com/v1/messages", map[string]string{
		"content-type": "application/json", "x-api-key": apiKey, "anthropic-version": "2023-06-01",
	}, payload)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range asSlice(result["content"]) {
		if asString(asMap(p)["type"]) == "text" {
			b.WriteString(asString(asMap(p)["text"]))
		}
	}
	return b.String(), nil
}

func callOpenAI(ctx context.Context, messages []Message, system, model, apiKey, baseURL string, onDelta, onReasoning func(string)) (string, error) {
	url := "https://api.openai.com/v1/chat/completions"
	if baseURL != "" {
		url = strings.TrimRight(baseURL, "/") + "/chat/completions"
	}
	msgs := []map[string]string{}
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	for _, m := range messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	// Newer models (o1/o3 etc.) only accept max_completion_tokens; the legacy
	// max_tokens parameter is rejected with 400
	payload := map[string]interface{}{"model": model, "messages": msgs, "max_completion_tokens": 8192}
	result, err := httpPostJSON(ctx, url, map[string]string{
		"content-type": "application/json", "authorization": "Bearer " + apiKey,
	}, payload)
	if err != nil {
		return "", err
	}
	choices := asSlice(result["choices"])
	if len(choices) == 0 {
		return "", fmt.Errorf("failed to parse response: no choices")
	}
	return asString(asMap(asMap(choices[0])["message"])["content"]), nil
}

func callGemini(ctx context.Context, messages []Message, system, model, apiKey, baseURL string, onDelta, onReasoning func(string)) (string, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, apiKey)
	contents := []map[string]interface{}{}
	for _, m := range messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]interface{}{
			"role": role, "parts": []map[string]string{{"text": m.Content}},
		})
	}
	payload := map[string]interface{}{
		"contents":         contents,
		"generationConfig": map[string]interface{}{"maxOutputTokens": 8192},
	}
	if system != "" {
		payload["systemInstruction"] = map[string]interface{}{"parts": []map[string]string{{"text": system}}}
	}
	result, err := httpPostJSON(ctx, url, map[string]string{"content-type": "application/json"}, payload)
	if err != nil {
		return "", err
	}
	candidates := asSlice(result["candidates"])
	if len(candidates) == 0 {
		return "(model returned no content, possibly blocked by safety filters)", nil
	}
	var b strings.Builder
	for _, p := range asSlice(asMap(asMap(candidates[0])["content"])["parts"]) {
		b.WriteString(asString(asMap(p)["text"]))
	}
	return b.String(), nil
}

// callOpenAIResponses speaks the OpenAI Responses API protocol
// (POST {baseURL}/responses) with SSE streaming. Many vendors (e.g. Volcengine
// Ark) expose OpenAI-compatible capability through this protocol.
func callOpenAIResponses(ctx context.Context, messages []Message, system, model, apiKey, baseURL string, onDelta, onReasoning func(string)) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("openai_responses provider requires base_url; pass --base-url.")
	}
	url := strings.TrimRight(baseURL, "/") + "/responses"
	items := []map[string]interface{}{}
	for _, m := range messages {
		items = append(items, map[string]interface{}{
			"role":    m.Role,
			"content": []map[string]string{{"type": "input_text", "text": m.Content}},
		})
	}
	payload := map[string]interface{}{"model": model, "input": items, "stream": true}
	if system != "" {
		payload["instructions"] = system
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return "", errInterrupted
		}
		return "", fmt.Errorf("network error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var full strings.Builder
	var currentEvent string
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(line[len("event:"):])
			} else if strings.HasPrefix(line, "data:") {
				dataStr := strings.TrimSpace(line[len("data:"):])
				if dataStr != "" && dataStr != "[DONE]" {
					var evt map[string]interface{}
					if json.Unmarshal([]byte(dataStr), &evt) == nil {
						etype := currentEvent
						if etype == "" {
							etype = asString(evt["type"])
						}
						switch etype {
						case "response.output_text.delta":
							if d := asString(evt["delta"]); d != "" {
								full.WriteString(d)
								if onDelta != nil {
									onDelta(d)
								}
							}
						case "response.reasoning_summary_text.delta":
							// Reasoning deltas don't count as reply content; used
							// only as a "still thinking" heartbeat for the spinner
							if onReasoning != nil {
								onReasoning(asString(evt["delta"]))
							}
						case "response.failed":
							msg := asString(asMap(asMap(evt["response"])["error"])["message"])
							if msg == "" {
								msg = "unknown error"
							}
							return "", fmt.Errorf("upstream error: %s", msg)
						case "response.completed":
							return full.String(), nil
						}
					}
				}
			}
		}
		if err != nil {
			if ctx.Err() == context.Canceled {
				return "", errInterrupted
			}
			break
		}
	}
	return full.String(), nil
}

type callerFunc func(ctx context.Context, messages []Message, system, model, apiKey, baseURL string, onDelta, onReasoning func(string)) (string, error)

var callers = map[string]callerFunc{
	"anthropic":        callAnthropic,
	"openai":           callOpenAI,
	"gemini":           callGemini,
	"openai_responses": callOpenAIResponses,
}

func callModel(ctx context.Context, provider string, messages []Message, system, model, apiKey, baseURL string, onDelta, onReasoning func(string)) (string, error) {
	fn, ok := callers[provider]
	if !ok {
		return "", fmt.Errorf("unknown provider: %s", provider)
	}
	return fn(ctx, messages, system, model, apiKey, baseURL, onDelta, onReasoning)
}

// --------------------------------------------------------------------------
// Terminal Markdown rendering
//   Headings/bold/italic/inline code/lists/quotes/hr/code blocks; chroma syntax
//   highlighting. Falls back to plain text when piped, TERM=dumb, or NO_COLOR.
// --------------------------------------------------------------------------

func detectColorSupport() bool {
	if os.Getenv("FRZ_FORCE_COLOR") != "" {
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

var useColor = detectColorSupport()

var ansiCodes = map[string]string{
	"reset": "\033[0m", "bold": "\033[1m", "dim": "\033[2m", "italic": "\033[3m",
	"red": "\033[31m", "green": "\033[32m", "gray": "\033[90m",
	// Inline code: blue-violet rgb(177,185,249), Claude Code's suggestion color
	"inline_code": "\033[38;2;177;185;249m",
}

func stylize(text string, styles ...string) string {
	if !useColor || text == "" {
		return text
	}
	var b strings.Builder
	for _, s := range styles {
		b.WriteString(ansiCodes[s])
	}
	b.WriteString(text)
	b.WriteString(ansiCodes["reset"])
	return b.String()
}

// Inline elements: code > bold > italic. Go's RE2 has no lookaround, so the
// word-boundary guards for the underscore forms (protecting snake_case) are
// written into the pattern as (?:^|[^\w]) boundary character groups — otherwise
// an underscore inside a word like TIME_WAIT would match a "giant italic" span
// across half the line and swallow any **bold** in between. Boundary characters
// are consumed by the match but written back verbatim, matching the semantics
// of Python's (?<!\w)…(?!\w).
var inlineRe = regexp.MustCompile(
	"(\x60[^\x60\n]+\x60)" +
		"|(\\*\\*[^\\s*](?:[^\n*]*[^\\s*])?\\*\\*)" +
		"|((?:^|[^\\w])(__[^_\n]+?__)(?:[^\\w]|$))" +
		"|(\\*[^\\s*](?:[^\n*]*[^\\s*])?\\*)" +
		"|((?:^|[^\\w])(_[^_\n]+?_)(?:[^\\w]|$))")

func renderInline(line string) string {
	if !useColor {
		return line
	}
	var b strings.Builder
	last := 0
	for _, m := range inlineRe.FindAllStringSubmatchIndex(line, -1) {
		b.WriteString(line[last:m[0]])
		switch {
		case m[2] >= 0: // `code`
			b.WriteString(stylize(line[m[2]+1:m[3]-1], "inline_code"))
		case m[4] >= 0: // **bold**
			b.WriteString(stylize(line[m[4]+2:m[5]-2], "bold"))
		case m[6] >= 0: // boundary+__bold__+boundary (m[6:8] whole, m[8:10] the __..__ body)
			b.WriteString(line[m[6]:m[8]])
			b.WriteString(stylize(line[m[8]+2:m[9]-2], "bold"))
			b.WriteString(line[m[9]:m[7]])
		case m[10] >= 0: // *it*
			b.WriteString(stylize(line[m[10]+1:m[11]-1], "italic"))
		case m[12] >= 0: // boundary+_it_+boundary
			b.WriteString(line[m[12]:m[14]])
			b.WriteString(stylize(line[m[14]+1:m[15]-1], "italic"))
			b.WriteString(line[m[15]:m[13]])
		}
		last = m[1]
	}
	b.WriteString(line[last:])
	return b.String()
}

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*)$`)
	hrRe       = regexp.MustCompile(`^\s{0,3}(-{3,}|\*{3,}|_{3,})\s*$`)
	ulRe       = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	olRe       = regexp.MustCompile(`^(\s*)(\d{1,3})[.)]\s+(.*)$`)
	fenceRe    = regexp.MustCompile("^\\s{0,3}\x60{3}([^\x60]*)\\s*$")
	tableSepRe = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(?:\|\s*:?-{2,}:?\s*)+\|?\s*$`)
	ansiStrip  = regexp.MustCompile("\\x1b\\[[0-9;]*m")
)

func renderLine(line string) string {
	if m := headingRe.FindStringSubmatch(line); m != nil {
		return stylize(m[1], "bold") // heading keeps default foreground, bold only
	}
	if hrRe.MatchString(line) {
		return stylize(strings.Repeat("─", 40), "gray")
	}
	stripped := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(stripped, ">") {
		indent := line[:len(line)-len(stripped)]
		return indent + stylize("▎", "gray") + " " + renderInline(strings.TrimLeft(stripped[1:], " \t"))
	}
	if m := ulRe.FindStringSubmatch(line); m != nil {
		return m[1] + "• " + renderInline(m[2])
	}
	if m := olRe.FindStringSubmatch(line); m != nil {
		return m[1] + m[2] + ". " + renderInline(m[3])
	}
	return renderInline(line)
}

func isTableRow(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "|") && strings.Count(s, "|") >= 2
}

func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	parts := strings.Split(s, "|")
	for i, c := range parts {
		// Expand tabs to a fixed 4 spaces: tabs jump to multiples of 8 in the
		// terminal, making width unpredictable
		parts[i] = strings.ReplaceAll(strings.TrimSpace(c), "\t", "    ")
	}
	return parts
}

func colAlign(cell string) string {
	cell = strings.TrimSpace(cell)
	left, right := strings.HasPrefix(cell, ":"), strings.HasSuffix(cell, ":")
	if left && right {
		return "center"
	}
	if right {
		return "right"
	}
	return "left"
}

// dispWidth returns the display width: ANSI escapes stripped, then terminal
// columns counted by grapheme cluster (uniseg handles ZWJ/VS16/combining marks)
func dispWidth(s string) int {
	return uniseg.StringWidth(ansiStrip.ReplaceAllString(s, ""))
}

func padCell(s string, width int, align string) string {
	gap := width - dispWidth(s)
	if gap <= 0 {
		return s
	}
	switch align {
	case "right":
		return strings.Repeat(" ", gap) + s
	case "center":
		left := gap / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
	}
	return s + strings.Repeat(" ", gap)
}

// pickStyle guesses light/dark terminal background from COLORFGBG and picks a
// chroma style (github for light backgrounds, monokai for dark)
func pickStyle() string {
	cfb := os.Getenv("COLORFGBG")
	if i := strings.LastIndex(cfb, ";"); i >= 0 {
		switch cfb[i+1:] {
		case "7", "15":
			return "github"
		}
	}
	return "monokai"
}

var (
	bgStrip1 = regexp.MustCompile(`;48;5;\d+`)
	bgStrip2 = regexp.MustCompile(`;48;2;\d+;\d+;\d+`)
	bgStrip3 = regexp.MustCompile("\\x1b\\[48;[25];[\\d;]*m")
)

// stripChromaBG strips the style's own background colors, keeping only
// foreground colors (so the terminal background shows through)
func stripChromaBG(text string) string {
	text = bgStrip1.ReplaceAllString(text, "")
	text = bgStrip2.ReplaceAllString(text, "")
	return bgStrip3.ReplaceAllString(text, "")
}

func isNumberToken(t chroma.TokenType) bool {
	return t >= chroma.LiteralNumber && t < chroma.LiteralNumber+100
}

var (
	numPlainRe   = regexp.MustCompile(`^\d+(\.\d+)?$`)
	numAccRe     = regexp.MustCompile(`^[0-9a-fA-FtT./:-]+$`)
	numCompundRe = regexp.MustCompile(`^[0-9a-fA-FtT]+([./:-][0-9a-fA-FtT]+){2,}$`)
	numBareRe    = regexp.MustCompile(`^\d+$`)
)

// demoteLine demotes pseudo-number tokens inside "number+separator+number"
// compound fragments (IP/date/time/MAC/version) within a single line
func demoteLine(tokens []chroma.Token) ([]chroma.Token, bool) {
	out := []chroma.Token{}
	demoted := false
	i, n := 0, len(tokens)
	for i < n {
		t := tokens[i]
		if isNumberToken(t.Type) && numPlainRe.MatchString(t.Value) {
			j := i + 1
			acc := t.Value
			for j < n && numAccRe.MatchString(tokens[j].Value) {
				acc += tokens[j].Value
				j++
			}
			if numCompundRe.MatchString(acc) {
				for k := i; k < j; k++ {
					out = append(out, chroma.Token{Type: chroma.Text, Value: tokens[k].Value})
				}
				demoted = true
				i = j
				continue
			}
		}
		out = append(out, t)
		i++
	}
	return out, demoted
}

// demoteCompoundNumberTokens runs compound-number demotion per line; once a
// fragment is demoted on a line, stray bare numbers on that line are demoted
// too (avoiding a half-colored timestamp line)
func demoteCompoundNumberTokens(tokens []chroma.Token) []chroma.Token {
	var lines [][]chroma.Token
	var cur []chroma.Token
	for _, t := range tokens {
		cur = append(cur, t)
		if strings.Contains(t.Value, "\n") {
			lines = append(lines, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	out := []chroma.Token{}
	for _, line := range lines {
		processed, demoted := demoteLine(line)
		if demoted {
			for i, t := range processed {
				if isNumberToken(t.Type) && numBareRe.MatchString(t.Value) {
					processed[i] = chroma.Token{Type: chroma.Text, Value: t.Value}
				}
			}
		}
		out = append(out, processed...)
	}
	return out
}

// Prompt prefix of shell session blocks ([user@host ~]# / user@host:~$);
// the bash lexer doesn't understand prompts, so they are handled separately
var shellSessionLangs = map[string]bool{
	"bash": true, "sh": true, "shell": true, "zsh": true,
	"console": true, "shell-session": true, "": true,
}
var promptRe = regexp.MustCompile(`^(\s*(?:\[[\w.\-]+@[\w.\-]+[^\]]*\]|[\w.\-]+@[\w.\-]+:[^\n]*?)[#$]\s*)(.*)$`)

func lexFragment(lexer chroma.Lexer, s string) []chroma.Token {
	var toks []chroma.Token
	it, err := lexer.Tokenise(nil, s)
	if err != nil {
		return []chroma.Token{{Type: chroma.Text, Value: s}}
	}
	for t := it(); t != chroma.EOF; t = it() {
		toks = append(toks, t)
	}
	return toks
}

// lexShellSession handles shell session blocks per line: the prompt prefix
// stays plain text, the command after #/$ still gets bash highlighting
func lexShellSession(code string, lexer chroma.Lexer) []chroma.Token {
	out := []chroma.Token{}
	for _, line := range strings.Split(code, "\n") {
		if m := promptRe.FindStringSubmatch(line); m != nil {
			out = append(out, chroma.Token{Type: chroma.Text, Value: m[1]})
			if m[2] != "" {
				out = append(out, lexFragment(lexer, m[2])...)
			}
		} else if line != "" {
			out = append(out, lexFragment(lexer, line)...)
		}
		out = append(out, chroma.Token{Type: chroma.Text, Value: "\n"})
	}
	return out
}

func highlightCode(code, lang string) string {
	var lexer chroma.Lexer
	if lang != "" {
		lexer = lexers.Get(lang)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	var toks []chroma.Token
	hasPrompt := false
	for _, l := range strings.Split(code, "\n") {
		if promptRe.MatchString(l) {
			hasPrompt = true
			break
		}
	}
	if shellSessionLangs[strings.ToLower(lang)] && hasPrompt {
		toks = lexShellSession(code, lexer)
	} else {
		toks = lexFragment(lexer, code)
	}
	toks = demoteCompoundNumberTokens(toks)
	// Truecolor formatter: emits the style's exact RGB values, more faithful to
	// the theme than 256-color approximation (whose mapping differs between
	// chroma and pygments anyway)
	formatter := formatters.Get("terminal16m")
	var buf bytes.Buffer
	if err := formatter.Format(&buf, styles.Get(pickStyle()), chroma.Literator(toks...)); err != nil {
		return code
	}
	return stripChromaBG(buf.String())
}

// --------------------------------------------------------------------------
// ASCII box-art realignment in code blocks
//   Models draw ┌─┐│└─┘ diagrams by "mentally" counting CJK widths and are
//   often off by 1-2 columns, leaving ragged right edges. This realigns box
//   lines in untagged/text code blocks by padding every vertical edge rightward
//   to the group's widest column. Insert-only, never deletes content.
// --------------------------------------------------------------------------

// Box-drawing chars with "vertical edge" semantics; ─ ┬ ┴ ┼ are horizontal/crossing
var boxVBars = map[rune]bool{'│': true, '┌': true, '┐': true, '└': true, '┘': true, '├': true, '┤': true}

// Only realign blocks with these language tags, avoiding box chars inside
// e.g. python string literals
var boxArtLangs = map[string]bool{"": true, "text": true, "txt": true, "plain": true}

// Max column spread allowed for edges at the same level: beyond that the boxes
// are intentionally different widths, or an anomalous line sneaked in
const boxEdgeTolerance = 4

type boxEdge struct {
	byteIdx int
	col     int
}

// boxEdges returns (byte_index, display_col) of all vertical-edge chars in the
// line; lines containing tabs return nil (column math unpredictable)
func boxEdges(line string) []boxEdge {
	if strings.Contains(line, "\t") {
		return nil
	}
	var edges []boxEdge
	col := 0
	g := uniseg.NewGraphemes(line)
	for g.Next() {
		cl := g.Str()
		from, _ := g.Positions()
		if r, ok := singleRune(cl); ok && boxVBars[r] {
			edges = append(edges, boxEdge{from, col})
		}
		col += uniseg.StringWidth(cl)
	}
	return edges
}

func singleRune(s string) (rune, bool) {
	rs := []rune(s)
	if len(rs) == 1 {
		return rs[0], true
	}
	return 0, false
}

// alignBoxEdge aligns each line's (-level)-th vertical edge from the right to
// one display column (level=-1 is the outermost right edge). Only pushes edges
// rightward (insert spaces before an edge; insert ─ for horizontal borders).
func alignBoxEdge(group []string, level int) []string {
	type entry struct {
		idx int
		e   boxEdge
	}
	var particip []entry
	for idx, line := range group {
		edges := boxEdges(line)
		// level=-1 needs at least two edges; level=-2 needs at least three
		// (with two, the 2nd-from-right is the left border and must not move)
		if edges != nil && len(edges) >= 1-level {
			particip = append(particip, entry{idx, edges[len(edges)+level]})
		}
	}
	if len(particip) < 2 {
		return group
	}
	minC, maxC := particip[0].e.col, particip[0].e.col
	for _, p := range particip[1:] {
		if p.e.col < minC {
			minC = p.e.col
		}
		if p.e.col > maxC {
			maxC = p.e.col
		}
	}
	if maxC-minC > boxEdgeTolerance {
		return group
	}
	out := append([]string(nil), group...)
	for _, p := range particip {
		gap := maxC - p.e.col
		if gap <= 0 {
			continue
		}
		line := out[p.idx]
		// Horizontal borders (┌──┐/└──┘) get ─ to stay continuous; content lines get spaces
		fill := " "
		if p.e.byteIdx > 0 && lastRuneIs(line[:p.e.byteIdx], '─') {
			fill = "─"
		}
		out[p.idx] = line[:p.e.byteIdx] + strings.Repeat(fill, gap) + line[p.e.byteIdx:]
	}
	return out
}

func lastRuneIs(s string, r rune) bool {
	rs := []rune(s)
	return len(rs) > 0 && rs[len(rs)-1] == r
}

// realignGroup: fully uniform structure (same edge count on every line, e.g.
// several identical side-by-side boxes) aligns every edge level from innermost
// to outermost; mixed structure (nested boxes / annotations) aligns only the
// two outermost right-edge levels — deeper levels risk mismapping and are left
// to the spread guard.
func realignGroup(group []string) []string {
	counts := map[int]bool{}
	for _, l := range group {
		counts[len(boxEdges(l))] = true
	}
	if len(counts) == 1 {
		var nEdges int
		for n := range counts {
			nEdges = n
		}
		for level := -(nEdges - 1); level < 0; level++ {
			group = alignBoxEdge(group, level)
		}
	} else {
		group = alignBoxEdge(group, -2)
		group = alignBoxEdge(group, -1)
	}
	return group
}

// realignBoxArt realigns hand-drawn box diagrams in a code block; a run of
// consecutive box lines (>= 3 lines) forms one box group
func realignBoxArt(code string) string {
	raw := strings.Split(code, "\n")
	lines := make([]string, len(raw))
	for i, l := range raw {
		lines[i] = strings.TrimRight(l, " \t")
	}
	out := []string{}
	i, n := 0, len(lines)
	for i < n {
		if boxEdges(lines[i]) != nil {
			j := i
			for j < n && boxEdges(lines[j]) != nil {
				j++
			}
			group := lines[i:j]
			if len(group) >= 3 {
				group = realignGroup(group)
			}
			out = append(out, group...)
			i = j
		} else {
			out = append(out, lines[i])
			i++
		}
	}
	return strings.Join(out, "\n")
}

// --------------------------------------------------------------------------
// Markdown renderer (line-fed; code blocks and tables accumulate before
// one-shot formatting)
// --------------------------------------------------------------------------

type markdownRenderer struct {
	inCode       bool
	codeLang     string
	codeLines    []string
	tablePending *string
	inTable      bool
	tableRows    []string
	tableAligns  []string
}

func (r *markdownRenderer) feedLine(line string) (string, bool) {
	out := []string{}

	// Table state machine (| inside code blocks doesn't trigger tables)
	if !r.inCode {
		if r.tablePending != nil {
			if tableSepRe.MatchString(line) {
				r.inTable = true
				r.tableRows = []string{*r.tablePending}
				for _, c := range splitTableRow(line) {
					r.tableAligns = append(r.tableAligns, colAlign(c))
				}
				r.tablePending = nil
				return "", false
			}
			out = append(out, renderLine(*r.tablePending))
			r.tablePending = nil
		}
		if r.inTable {
			if isTableRow(line) {
				r.tableRows = append(r.tableRows, line)
				return "", false
			}
			out = append(out, r.renderTable())
			r.inTable = false
			r.tableRows = nil
		}
	}

	// Code block fence
	if m := fenceRe.FindStringSubmatch(line); m != nil {
		if r.inCode {
			r.inCode = false
			out = append(out, r.renderCode())
			r.codeLines = nil
		} else {
			r.inCode = true
			r.codeLang = strings.TrimSpace(m[1])
			r.codeLines = nil
		}
		return strings.Join(out, "\n"), len(out) > 0
	}
	if r.inCode {
		r.codeLines = append(r.codeLines, line)
		return strings.Join(out, "\n"), len(out) > 0
	}

	// Candidate table header: hold pending, check if next line is a separator
	if isTableRow(line) {
		r.tablePending = &line
		return strings.Join(out, "\n"), len(out) > 0
	}

	out = append(out, renderLine(line))
	return strings.Join(out, "\n"), true
}

// flush is called at end of input: renders any pending table or unclosed code block
func (r *markdownRenderer) flush() (string, bool) {
	out := []string{}
	if r.tablePending != nil {
		out = append(out, renderLine(*r.tablePending))
		r.tablePending = nil
	}
	if r.inTable {
		out = append(out, r.renderTable())
		r.inTable = false
		r.tableRows = nil
	}
	if r.inCode {
		r.inCode = false
		if len(r.codeLines) > 0 {
			out = append(out, r.renderCode())
		}
	}
	return strings.Join(out, "\n"), len(out) > 0
}

func (r *markdownRenderer) renderTable() string {
	rows := [][]string{}
	ncols := 0
	for _, row := range r.tableRows {
		cells := splitTableRow(row)
		if len(cells) > ncols {
			ncols = len(cells)
		}
		rows = append(rows, cells)
	}
	for i, row := range rows {
		for len(row) < ncols {
			row = append(row, "")
		}
		rows[i] = row
	}
	aligns := append([]string(nil), r.tableAligns...)
	for len(aligns) < ncols {
		aligns = append(aligns, "left")
	}
	aligns = aligns[:ncols]
	rendered := make([][]string, len(rows))
	for i, row := range rows {
		rc := make([]string, ncols)
		for j, c := range row {
			rc[j] = renderInline(c)
		}
		rendered[i] = rc
	}
	widths := make([]int, ncols)
	for j := 0; j < ncols; j++ {
		for _, row := range rendered {
			if w := dispWidth(row[j]); w > widths[j] {
				widths[j] = w
			}
		}
	}
	segs := make([]string, ncols)
	for j, w := range widths {
		segs[j] = strings.Repeat("─", w+2) // +2 for the space padding on both sides
	}
	border := func(left, mid, right string) string {
		return stylize(left + strings.Join(segs, mid) + right, "gray")
	}
	makeRow := func(cells []string, cellStyle string) string {
		parts := make([]string, ncols)
		for i := 0; i < ncols; i++ {
			c := cells[i]
			if cellStyle != "" {
				c = stylize(c, cellStyle)
			}
			parts[i] = padCell(c, widths[i], aligns[i])
		}
		bar := stylize("│", "gray")
		return bar + " " + strings.Join(parts, " "+bar+" ") + " " + bar
	}
	// Fully enclosed box: top/bottom borders + separators between header and rows
	lines := []string{border("┌", "┬", "┐")}
	lines = append(lines, makeRow(rendered[0], "bold"))
	lines = append(lines, border("├", "┼", "┤"))
	for i, row := range rendered[1:] {
		if i > 0 {
			lines = append(lines, border("├", "┼", "┤"))
		}
		lines = append(lines, makeRow(row, ""))
	}
	lines = append(lines, border("└", "┴", "┘"))
	return strings.Join(lines, "\n")
}

func (r *markdownRenderer) renderCode() string {
	code := strings.Join(r.codeLines, "\n")
	if boxArtLangs[strings.ToLower(r.codeLang)] {
		// Hand-drawn diagrams are often ragged from CJK width miscounts; realign first
		code = realignBoxArt(code)
	}
	bar := stylize("▎", "gray")
	if useColor {
		rendered := strings.TrimRight(highlightCode(code, r.codeLang), "\n")
		lines := strings.Split(rendered, "\n")
		for i, l := range lines {
			lines[i] = bar + " " + l
		}
		return strings.Join(lines, "\n")
	}
	if code == "" {
		return ""
	}
	lines := strings.Split(code, "\n")
	for i, l := range lines {
		lines[i] = bar + " " + l
	}
	return strings.Join(lines, "\n")
}

func renderMarkdown(text string) string {
	r := &markdownRenderer{}
	out := []string{}
	for _, line := range strings.Split(text, "\n") {
		if rendered, ok := r.feedLine(line); ok {
			out = append(out, rendered)
		}
	}
	if tail, ok := r.flush(); ok {
		out = append(out, tail)
	}
	return strings.Join(out, "\n")
}

// streamRenderer is a line-buffered renderer for streaming output: renders only
// complete lines (Markdown syntax closes per line)
type streamRenderer struct {
	r   *markdownRenderer
	buf string
}

func newStreamRenderer() *streamRenderer {
	return &streamRenderer{r: &markdownRenderer{}}
}

func (s *streamRenderer) feed(text string) {
	s.buf += text
	for strings.Contains(s.buf, "\n") {
		i := strings.Index(s.buf, "\n")
		line := s.buf[:i]
		s.buf = s.buf[i+1:]
		s.emit(line)
	}
}

func (s *streamRenderer) finish() {
	if s.buf != "" {
		s.emit(s.buf)
		s.buf = ""
	}
	if tail, ok := s.r.flush(); ok {
		fmt.Println(tail)
	}
}

func (s *streamRenderer) emit(line string) {
	if rendered, ok := s.r.feedLine(line); ok {
		fmt.Println(rendered)
	}
}

// --------------------------------------------------------------------------
// ThinkingIndicator: animated indicator while waiting for the model (refreshed
// from a goroutine). Style replicates Claude Code's spinner: ✻ + random verb +
// shimmer sweep.
// --------------------------------------------------------------------------

var thinkingVerbs = []string{"Thinking", "Pondering", "Mulling", "Brewing", "Wondering",
	"Deliberating", "Computing", "Searching", "Weaving", "Wandering"}

const (
	shimmerBase = "\033[38;2;147;165;255m" // claudeBlue_FOR_SYSTEM_SPINNER
	shimmerHot  = "\033[38;2;177;195;255m" // claudeBlueShimmer
	shimmerW    = 4
)

type thinkingIndicator struct {
	stopCh  chan struct{}
	doneCh  chan struct{}
	mu      sync.Mutex
	state   string
	t0      time.Time
	started bool
	rng     *rand2
}

type rand2 struct{ mu sync.Mutex }

func (r *rand2) pick() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return thinkingVerbs[int(time.Now().UnixNano()%int64(len(thinkingVerbs)))]
}

func newThinkingIndicator() *thinkingIndicator {
	return &thinkingIndicator{state: "Waiting", rng: &rand2{}}
}

func (ti *thinkingIndicator) start() {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	ti.t0 = time.Now()
	ti.stopCh = make(chan struct{})
	ti.doneCh = make(chan struct{})
	ti.started = true
	go ti.run()
}

func (ti *thinkingIndicator) setState(state string) {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	if ti.state == "Waiting" {
		ti.state = state
	}
}

func (ti *thinkingIndicator) shimmerText(text string, frame int) string {
	if !useColor {
		return text
	}
	runes := []rune(text)
	n := len(runes)
	cycle := n + shimmerW
	pos := frame % cycle
	var b strings.Builder
	for i, ch := range runes {
		if pos <= i && i < pos+shimmerW {
			b.WriteString(shimmerHot)
		} else {
			b.WriteString(shimmerBase)
		}
		b.WriteRune(ch)
	}
	b.WriteString("\033[0m")
	return b.String()
}

func (ti *thinkingIndicator) run() {
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	frame := 0
	for {
		select {
		case <-ti.stopCh:
			close(ti.doneCh)
			return
		case <-ticker.C:
			ti.mu.Lock()
			state := ti.state
			ti.mu.Unlock()
			elapsed := int(time.Since(ti.t0).Seconds())
			ts := fmt.Sprintf("%ds", elapsed)
			if elapsed >= 60 {
				ts = fmt.Sprintf("%dm %ds", elapsed/60, elapsed%60)
			}
			text := fmt.Sprintf("✻ %s %s", state, ts)
			fmt.Fprintf(os.Stdout, "\r%s\033[K", ti.shimmerText(text, frame))
			frame++
		}
	}
}

func (ti *thinkingIndicator) stop() {
	if !ti.started {
		return
	}
	close(ti.stopCh)
	<-ti.doneCh
	ti.started = false
	fmt.Fprint(os.Stdout, "\r\033[K") // erase the indicator line
}

func (ti *thinkingIndicator) randomVerb() { ti.setState(ti.rng.pick()) }

// --------------------------------------------------------------------------
// REPL
// --------------------------------------------------------------------------

func formatHistory(messages []Message) string {
	out := []string{}
	for _, m := range messages {
		out = append(out, "")
		if m.Role == "user" {
			for _, line := range strings.Split(m.Content, "\n") {
				out = append(out, stylize("> ", "green")+line)
			}
		} else {
			out = append(out, renderMarkdown(m.Content))
		}
	}
	return strings.Join(out, "\n")
}

// pageText views long text through a pager ($PAGER, default less -R -F -X);
// falls back to plain print when not a tty or no pager is available
func pageText(text string) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Println(text)
		return
	}
	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less"
	}
	parts := strings.Fields(pager)
	if filepath.Base(parts[0]) == "less" {
		parts = append(parts, "-R", "-F", "-X")
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println(text)
	}
}

// Slash commands offered for Tab completion
var replCommands = []string{
	"/baseurl", "/clear", "/edit", "/exit", "/export", "/help", "/history",
	"/list", "/model", "/new", "/quit", "/rename", "/resume", "/save", "/system",
}

type frzCompleter struct{}

// Do implements readline.AutoCompleter: session names after /resume,
// command names at line start
func (frzCompleter) Do(line []rune, pos int) ([][]rune, int) {
	s := string(line[:pos])
	if strings.HasPrefix(s, "/resume ") || strings.HasPrefix(s, "/resume\t") {
		idx := strings.LastIndexAny(s, " \t")
		prefix := s[idx+1:]
		var out [][]rune
		for _, e := range listSessions() {
			if strings.HasPrefix(e.name, prefix) {
				out = append(out, []rune(e.name[len(prefix):]))
			}
		}
		return out, len([]rune(prefix))
	}
	if strings.HasPrefix(s, "/") && !strings.ContainsAny(s, " \t") {
		var out [][]rune
		for _, c := range replCommands {
			if strings.HasPrefix(c, s) {
				out = append(out, []rune(c[len(s):]))
			}
		}
		return out, len([]rune(s))
	}
	return nil, 0
}

const helpText = `Available commands:
  Sessions
    /save [name]         Save current session (uses current name if omitted)
    /rename <name>       Rename current session
    /resume [name]       Switch to another session (latest session if omitted)
    /list                List all saved sessions
    /new [name]          Start a new session
    /export [file]       Export current session to Markdown
                         (default ~/.frz/exports/<session>.md)

  Session settings
    /system [prompt]     View or set the system prompt
    /model [name]        View or switch model
    /baseurl [url]       View or set custom API base url (required by
                         openai_responses and similar providers)

  Other
    /edit                Compose a multi-line message in your editor (great for
                         pasting long logs/questions); sent on save & exit
    /history [N]         Show conversation history (N = last N rounds only;
                         long output is paged automatically)
    /clear               Clear current session history (keeps system prompt)
    /help                Show this help
    /exit or /quit       Save and exit
`

const editHint = `# Type your message below (multi-line OK). It is sent when you
# save and exit the editor. These two lines are removed automatically;
# empty content cancels.
`

// editInEditor opens $VISUAL/$EDITOR (default vi) to compose a multi-line
// message; returns "" when cancelled/empty. Goes through a temp file rather
// than terminal line buffering, so there is no 1024-byte canonical-mode limit.
func editInEditor() string {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "frz-edit-*.md")
	if err != nil {
		fmt.Println(stylize("[error] cannot create temp file: "+err.Error(), "red"))
		return ""
	}
	path := f.Name()
	f.WriteString(editHint)
	f.Close()
	defer os.Remove(path)

	parts := append(strings.Fields(editor), path)
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.Error); ok {
			fmt.Println(stylize(fmt.Sprintf("[error] editor not found: %s (set it via export EDITOR=vim)", editor), "red"))
		} else {
			fmt.Println("(editor exited abnormally, cancelled)")
		}
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(data)
	// Only strip the hint lines if they are exactly as written: a blanket
	// "ignore lines starting with #" rule would eat log content such as
	// root prompts (# iptables -L)
	if strings.HasPrefix(text, editHint) {
		text = text[len(editHint):]
	}
	return strings.TrimSpace(text)
}

// sendMessage sends one user message to the model and renders the reply;
// on failure/interruption the message is not recorded
func sendMessage(session *Session, userInput, provider, apiKey string) {
	session.Messages = append(session.Messages, Message{Role: "user", Content: userInput})
	isStreaming := streamingProviders[provider]

	// Ctrl-C cancels this round: context cancels the HTTP call, message dropped
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	var interrupted atomic.Bool
	go func() {
		select {
		case <-sigCh:
			interrupted.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()

	indicator := newThinkingIndicator()
	indicator.start()
	var reply string
	var err error
	if isStreaming {
		fmt.Println()
		stream := newStreamRenderer()
		onDelta := func(text string) {
			indicator.stop()
			stream.feed(text)
		}
		onReasoning := func(string) { indicator.randomVerb() }
		reply, err = callModel(ctx, provider, session.Messages, session.SystemPrompt,
			session.Model, apiKey, session.BaseURL, onDelta, onReasoning)
		indicator.stop()
		stream.finish()
		fmt.Println()
	} else {
		reply, err = callModel(ctx, provider, session.Messages, session.SystemPrompt,
			session.Model, apiKey, session.BaseURL, nil, nil)
		indicator.stop()
	}

	if interrupted.Load() || err == errInterrupted {
		fmt.Println(stylize("\n[cancelled] request interrupted", "gray"))
		session.Messages = session.Messages[:len(session.Messages)-1]
		return
	}
	if err != nil {
		fmt.Println(stylize(fmt.Sprintf("\n[error] %v", err), "red"))
		session.Messages = session.Messages[:len(session.Messages)-1] // failed message is not recorded
		return
	}

	session.Messages = append(session.Messages, Message{Role: "assistant", Content: reply})
	if !isStreaming {
		fmt.Println()
		fmt.Println(renderMarkdown(reply))
		fmt.Println()
	}
	// Auto-save every round so an unexpected exit doesn't lose history
	saveSession(session, false)
}

func repl(session *Session, apiKey string) {
	provider := session.Provider
	fmt.Printf("%s  session %q  provider=%s  model=%s\n", shortVersion(), session.Name, provider, session.Model)
	fmt.Print("Type /help for commands, /exit to save and quit.\n\n")

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		AutoComplete:    frzCompleter{},
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Println(stylize("[error] cannot initialize line editing: "+err.Error(), "red"))
		return
	}
	defer rl.Close()

	for {
		userInput, err := rl.Readline()
		if err != nil { // Ctrl-C (ErrInterrupt) or Ctrl-D (io.EOF): save and quit
			fmt.Print("\n(interrupted, saving session...)\n")
			saveSession(session, false)
			break
		}
		userInput = strings.TrimSpace(userInput)
		if userInput == "" {
			continue
		}

		if strings.HasPrefix(userInput, "/") {
			cmd := userInput
			arg := ""
			if i := strings.IndexAny(userInput, " \t"); i >= 0 {
				cmd, arg = userInput[:i], strings.TrimSpace(userInput[i+1:])
			}
			cmd = strings.ToLower(cmd)

			switch cmd {
			case "/exit", "/quit":
				if path, ok := saveSession(session, false); ok {
					fmt.Printf("session saved to %s\n", path)
				} else {
					fmt.Println("(empty session, not saved)")
				}
				return

			case "/help":
				fmt.Print(helpText)

			case "/save":
				if arg != "" && arg != session.Name {
					if _, err := os.Stat(sessionPath(arg)); err == nil {
						fmt.Printf("session %q already exists, pick another name (to avoid overwriting it)\n", arg)
						continue
					}
				}
				if arg != "" {
					session.Name = arg
				}
				path, _ := saveSession(session, true)
				fmt.Printf("saved to %s\n", path)

			case "/rename":
				if arg == "" {
					fmt.Println("usage: /rename <new-name>")
					continue
				}
				oldName := session.Name
				saveSession(session, false) // persist latest content first, then rename as a whole
				if ok, result := renameSessionFile(oldName, arg); ok {
					session.Name = arg
					fmt.Printf("renamed session %q to %q\n", oldName, arg)
				} else {
					fmt.Println(result)
				}

			case "/resume":
				target := arg
				if target == "" {
					// No name given: resume the most recently updated session
					// (excluding the current one)
					for _, e := range listSessions() {
						if e.name != session.Name {
							target = e.name
							break
						}
					}
					if target == "" {
						fmt.Println("no session to resume.")
						continue
					}
				}
				saveSession(session, false)
				loaded := loadSession(target)
				if loaded == nil {
					fmt.Printf("session %q not found; type /list to see saved sessions.\n", target)
					continue
				}
				session = loaded
				provider = session.Provider
				// The resumed session may belong to another provider; re-resolve
				// the key against the session's own provider to avoid a 401
				if resumedKey := resolveAPIKey(provider, "", loadConfig()); resumedKey != "" {
					apiKey = resumedKey
				} else {
					envName := envKeyNames[provider]
					if envName == "" {
						envName = "corresponding env var"
					}
					fmt.Println(stylize(fmt.Sprintf("[note] no API key found for %s (%s); calls in this session will fail", provider, envName), "red"))
				}
				fmt.Printf("switched to session %q provider=%s model=%s\n", session.Name, provider, session.Model)

			case "/list":
				sessions := listSessions()
				if len(sessions) == 0 {
					fmt.Println("(no saved sessions yet)")
				}
				for _, e := range sessions {
					fmt.Printf("  - %s  [%s/%s]  %d messages  updated %s\n",
						e.name, e.sess.Provider, e.sess.Model, len(e.sess.Messages), e.sess.UpdatedAt)
				}

			case "/new":
				saveSession(session, false)
				newName := arg
				if newName == "" {
					newName = defaultSessionName()
				}
				// Carry base_url over: openai_responses and similar providers
				// cannot make calls without it
				session = newSession(newName, provider, session.Model, session.SystemPrompt, session.BaseURL)
				fmt.Printf("started new session %q\n", newName)

			case "/export":
				if len(session.Messages) == 0 {
					fmt.Println("(no conversation yet, nothing to export)")
					continue
				}
				path, overwritten := exportSession(session, arg)
				msg := fmt.Sprintf("exported to %s", path)
				if overwritten {
					msg += " (overwrote existing file)"
				}
				fmt.Println(msg)

			case "/edit":
				text := editInEditor()
				if text != "" {
					if kb := len(text) / 1024; kb > 100 {
						fmt.Printf("(editor content is %d KB, sending as a whole)\n", kb)
					}
					sendMessage(session, text, provider, apiKey)
				} else {
					fmt.Println("(empty content, cancelled)")
				}

			case "/system":
				if arg != "" {
					session.SystemPrompt = arg
					fmt.Println("system prompt updated.")
				} else if session.SystemPrompt != "" {
					fmt.Printf("current system prompt: %s\n", session.SystemPrompt)
				} else {
					fmt.Println("current system prompt: (not set)")
				}

			case "/model":
				if arg != "" {
					session.Model = arg
					fmt.Printf("model switched to: %s\n", arg)
				} else {
					fmt.Printf("current model: %s\n", session.Model)
				}

			case "/baseurl":
				if arg != "" {
					session.BaseURL = arg
					fmt.Printf("base url set to: %s\n", arg)
				} else if session.BaseURL != "" {
					fmt.Printf("current base url: %s\n", session.BaseURL)
				} else {
					fmt.Println("current base url: (not set, provider default)")
				}

			case "/history":
				msgs := session.Messages
				if len(msgs) == 0 {
					fmt.Println("(no conversation yet in this session)")
					continue
				}
				if arg != "" {
					var n int
					if _, err := fmt.Sscanf(arg, "%d", &n); err != nil || n <= 0 {
						fmt.Println("usage: /history [last N rounds]")
						continue
					}
					if 2*n < len(msgs) {
						msgs = msgs[len(msgs)-2*n:]
					}
					// Align to a user-message boundary so display doesn't start mid-round
					for len(msgs) > 0 && msgs[0].Role != "user" {
						msgs = msgs[1:]
					}
					if len(msgs) < len(session.Messages) {
						fmt.Printf("(showing last %d rounds only; full history has %d messages, /history shows all)\n",
							n, len(session.Messages))
					}
				}
				pageText(formatHistory(msgs))

			case "/clear":
				session.Messages = nil
				fmt.Println("session history cleared.")

			default:
				fmt.Printf("unknown command: %s; type /help for available commands.\n", cmd)
			}
			continue
		}

		// Normal user message -> call the model
		sendMessage(session, userInput, provider, apiKey)
	}
}

// --------------------------------------------------------------------------
// CLI entry
// --------------------------------------------------------------------------

func resolveModel(provider, cliModel string, cfg map[string]interface{}) string {
	if cliModel != "" {
		return cliModel
	}
	if m := cfgMap(cfg, "models"); m != nil {
		if s, ok := m[provider].(string); ok && s != "" {
			return s
		}
	}
	// The legacy global "model" config field only applies to the default
	// provider in config — otherwise switching providers would call the API
	// with the previous provider's model name (404)
	if provider == cfgStr(cfg, "provider") {
		if m := cfgStr(cfg, "model"); m != "" {
			return m
		}
	}
	return defaultModels[provider]
}

func resolveBaseURL(provider, cliURL string, cfg map[string]interface{}) string {
	if cliURL != "" {
		return cliURL
	}
	if urls := cfgMap(cfg, "base_urls"); urls != nil {
		if s, ok := urls[provider].(string); ok && s != "" {
			return s
		}
	}
	return defaultBaseURLs[provider]
}

func resolveAPIKey(provider, cliKey string, cfg map[string]interface{}) string {
	if cliKey != "" {
		return cliKey
	}
	if envName := envKeyNames[provider]; envName != "" {
		if k := os.Getenv(envName); k != "" {
			return k
		}
	}
	if keys := cfgMap(cfg, "api_keys"); keys != nil {
		if s, ok := keys[provider].(string); ok {
			return s
		}
	}
	return ""
}

const helpUsage = `usage: frz [-h] [--provider PROVIDER] [--model MODEL] [--api-key KEY]
           [--base-url URL] [--system PROMPT] [--resume NAME]
           [--session-name NAME] [--list-sessions] [--version]
           {config,rename,clean} ...

options:
  -h, --help           show this help message and exit
  --provider PROVIDER  model backend: anthropic / openai / gemini / openai_responses (default anthropic)
  --model MODEL        model name, e.g. claude-sonnet-4-6 / gpt-4o / gemini-2.5-flash / kimi-k3
  --api-key KEY        API key (can also come from env var or config, see below)
  --base-url URL       custom API base url (required by openai_responses and similar providers)
  --system PROMPT      system prompt
  --resume NAME        resume the named session
  --session-name NAME  name for the new session (auto-generated if omitted)
  --list-sessions      list saved sessions and exit
  --version            show version info and exit

subcommands:
  config    view or set default config
  rename    rename a saved session
  clean     delete all empty sessions

Examples:
  frz                                       start chatting with the default config
  frz --provider openai --model gpt-4o      pick a provider / model
  frz --resume my-session                   resume a saved session
  frz --list-sessions                       list all saved sessions and exit
  frz config set --provider anthropic --api-key sk-ant-xxxx
                                            persist defaults to ~/.frz/config.json
  frz rename old new                        rename a saved session
  frz clean                                 delete all empty sessions

API key precedence: --api-key > environment variable > config file
  anthropic=ANTHROPIC_API_KEY  openai=OPENAI_API_KEY
  gemini=GEMINI_API_KEY        openai_responses=ARK_API_KEY

Models and base urls are stored per provider and never leak across providers.

Once inside a session, type /help for slash commands (/save /resume /export
/edit /history etc.); Tab completes commands and /resume session names.
`

// parseFlags parses "--flag value", "--flag=value" and "--boolflag" forms
func parseFlags(args []string, names map[string]bool, boolNames map[string]bool) (map[string]string, []string, error) {
	flags := map[string]string{}
	var rest []string
	i := 0
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			kv := strings.SplitN(a[2:], "=", 2)
			name := kv[0]
			if boolNames[name] {
				flags[name] = "true"
				i++
				continue
			}
			if !names[name] {
				return nil, nil, fmt.Errorf("unknown option: --%s", name)
			}
			if len(kv) == 2 {
				flags[name] = kv[1]
			} else if i+1 < len(args) {
				flags[name] = args[i+1]
				i++
			} else {
				return nil, nil, fmt.Errorf("option --%s requires a value", name)
			}
		} else {
			rest = append(rest, a)
		}
		i++
	}
	return flags, rest, nil
}

var mainFlagNames = map[string]bool{
	"provider": true, "model": true, "api-key": true, "base-url": true,
	"system": true, "resume": true, "session-name": true,
}

var mainBoolFlags = map[string]bool{"list-sessions": true, "version": true}

func main() {
	ensureDirs()
	args := os.Args[1:]

	// Subcommands
	if len(args) > 0 {
		switch args[0] {
		case "config":
			cmdConfig(args[1:])
			return
		case "rename":
			cmdRename(args[1:])
			return
		case "clean":
			cmdClean()
			return
		case "render": // hidden subcommand: render Markdown from stdin (for golden tests)
			data, _ := io.ReadAll(os.Stdin)
			fmt.Println(renderMarkdown(string(data)))
			return
		}
	}

	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Printf("%s — a Gemini-CLI-style multi-provider chat client"+
				" (Anthropic / OpenAI / Gemini / OpenAI Responses)\n", shortVersion())
			fmt.Printf("author: %s\n\n", author)
			fmt.Print(helpUsage)
			return
		}
	}

	flags, _, err := parseFlags(args, mainFlagNames, mainBoolFlags)
	if err != nil {
		fmt.Println(err)
		fmt.Print("usage: frz [-h] [--provider PROVIDER] [--model MODEL] [--api-key KEY]\n" +
			"           [--base-url URL] [--system PROMPT] [--resume NAME]\n" +
			"           [--session-name NAME] [--list-sessions] [--version]\n" +
			"           {config,rename,clean} ...\n")
		os.Exit(1)
	}
	if flags["version"] == "true" {
		fmt.Println(versionString())
		return
	}
	listOnly := flags["list-sessions"] == "true"
	cfg := loadConfig()

	if listOnly {
		sessions := listSessions()
		if len(sessions) == 0 {
			fmt.Println("(no saved sessions yet)")
		}
		for _, e := range sessions {
			fmt.Printf("  - %s  [%s/%s]  %d messages  updated %s\n",
				e.name, e.sess.Provider, e.sess.Model, len(e.sess.Messages), e.sess.UpdatedAt)
		}
		return
	}

	provider := flags["provider"]
	if provider == "" {
		provider = cfgStr(cfg, "provider")
	}
	if provider == "" {
		provider = "anthropic"
	}
	if _, ok := callers[provider]; !ok {
		fmt.Printf("[error] unknown provider: %s (choose from: anthropic / openai / gemini / openai_responses)\n", provider)
		os.Exit(1)
	}

	var session *Session
	if name := flags["resume"]; name != "" {
		session = loadSession(name)
		if session == nil {
			fmt.Printf("[error] session %q not found; see --list-sessions.\n", name)
			os.Exit(1)
		}
		// The REPL always calls the model with the session's own provider, so the
		// provider/key are re-resolved against it
		provider = session.Provider
	}

	model := resolveModel(provider, flags["model"], cfg)
	baseURL := resolveBaseURL(provider, flags["base-url"], cfg)
	apiKey := resolveAPIKey(provider, flags["api-key"], cfg)

	if apiKey == "" {
		envName := envKeyNames[provider]
		if envName == "" {
			envName = "corresponding env var"
		}
		fmt.Printf("[error] no API key found for %s. Provide it via:\n", provider)
		fmt.Println("  1) --api-key YOUR_KEY")
		fmt.Printf("  2) export %s=YOUR_KEY\n", envName)
		fmt.Printf("  3) frz config set --provider %s --api-key YOUR_KEY\n", provider)
		os.Exit(1)
	}

	if _, need := defaultBaseURLs[provider]; need && baseURL == "" {
		fmt.Printf("[error] provider=%s requires a base url; pass --base-url, "+
			"or frz config set --provider %s --base-url URL\n", provider, provider)
		os.Exit(1)
	}

	if session != nil {
		// --resume: apply command-line overrides
		if v := flags["system"]; v != "" {
			session.SystemPrompt = v
		}
		if v := flags["model"]; v != "" {
			session.Model = v
		}
		if v := flags["base-url"]; v != "" {
			session.BaseURL = v
		} else if session.BaseURL == "" {
			session.BaseURL = baseURL
		}
	} else {
		name := flags["session-name"]
		if name == "" {
			name = defaultSessionName()
		}
		session = newSession(name, provider, model, flags["system"], baseURL)
	}

	repl(session, apiKey)
}

func cmdConfig(args []string) {
	cfg := loadConfig()
	if len(args) == 0 || args[0] == "show" {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		enc.Encode(cfg)
		fmt.Print(buf.String())
		return
	}
	if args[0] != "set" {
		fmt.Println("usage: frz config {set,show} ...")
		os.Exit(1)
	}
	for _, a := range args[1:] {
		if a == "-h" || a == "--help" {
			fmt.Print("usage: frz config set [-h] [--provider PROVIDER] [--model MODEL]\n" +
				"                     [--api-key KEY] [--base-url URL]\n\n" +
				"set default provider/model/api-key (API keys, base urls and models are stored per provider)\n")
			return
		}
	}
	flags, _, err := parseFlags(args[1:], map[string]bool{
		"provider": true, "model": true, "api-key": true, "base-url": true,
	}, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defaultProvider := flags["provider"]
	if defaultProvider == "" {
		defaultProvider = cfgStr(cfg, "provider")
	}
	if defaultProvider == "" {
		defaultProvider = "anthropic"
	}
	if v := flags["provider"]; v != "" {
		cfg["provider"] = v
	}
	if v := flags["model"]; v != "" {
		m := cfgMap(cfg, "models")
		if m == nil {
			m = map[string]interface{}{}
			cfg["models"] = m
		}
		m[defaultProvider] = v
	}
	if v := flags["api-key"]; v != "" {
		m := cfgMap(cfg, "api_keys")
		if m == nil {
			m = map[string]interface{}{}
			cfg["api_keys"] = m
		}
		m[defaultProvider] = v
	}
	if v := flags["base-url"]; v != "" {
		m := cfgMap(cfg, "base_urls")
		if m == nil {
			m = map[string]interface{}{}
			cfg["base_urls"] = m
		}
		m[defaultProvider] = v
	}
	saveConfig(cfg)
	fmt.Printf("config saved to %s\n", configFile)
}

func cmdRename(args []string) {
	if len(args) != 2 {
		fmt.Println("usage: frz rename <old-name> <new-name>")
		os.Exit(1)
	}
	if ok, result := renameSessionFile(args[0], args[1]); ok {
		fmt.Printf("renamed session %q to %q (%s)\n", args[0], args[1], result)
	} else {
		fmt.Printf("[error] %s\n", result)
		os.Exit(1)
	}
}

func cmdClean() {
	removed := []string{}
	for _, e := range listSessions() {
		if len(e.sess.Messages) == 0 {
			os.Remove(sessionPath(e.name))
			removed = append(removed, e.name)
			fmt.Printf("deleted empty session: %s\n", e.name)
		}
	}
	if len(removed) == 0 {
		fmt.Println("(no empty sessions)")
	}
}
