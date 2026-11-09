package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chzyer/readline"
)

func runREPLForTest(t *testing.T, s *Session, input string) string {
	t.Helper()
	dir := t.TempDir()
	inPath := filepath.Join(dir, "input")
	outPath := filepath.Join(dir, "output")
	os.WriteFile(inPath, []byte(input), 0o600)
	in, _ := os.Open(inPath)
	out, _ := os.Create(outPath)
	oldIn, oldOut := os.Stdin, os.Stdout
	oldReadlineIn, oldReadlineOut := readline.Stdin, readline.Stdout
	os.Stdin, os.Stdout = in, out
	readline.Stdin, readline.Stdout = in, out
	defer func() {
		os.Stdin, os.Stdout = oldIn, oldOut
		readline.Stdin, readline.Stdout = oldReadlineIn, oldReadlineOut
		in.Close()
		out.Close()
	}()
	repl(s, "test-key")
	data, _ := os.ReadFile(outPath)
	return string(data)
}

func TestResumedAPIKeyRules(t *testing.T) {
	useTempState(t)
	for _, env := range envKeyNames {
		t.Setenv(env, "")
	}
	if got := resumedAPIKey("anthropic", "openai_responses", "old"); got != "" {
		t.Fatalf("cross-provider key leaked: %q", got)
	}
	if got := resumedAPIKey("openai_responses", "openai_responses", "cli"); got != "cli" {
		t.Fatalf("same-provider CLI key lost: %q", got)
	}
	saveConfig(map[string]interface{}{"api_keys": map[string]interface{}{"openai_responses": "new"}})
	if got := resumedAPIKey("anthropic", "openai_responses", "old"); got != "new" {
		t.Fatalf("new provider key not resolved: %q", got)
	}
}

func TestMissingKeyBlocksMessage(t *testing.T) {
	useTempState(t)
	s := newSession("no-key", "openai_responses", "test", "", "https://example.invalid")
	sendMessage(s, "must not send", "openai_responses", "")
	if len(s.Messages) != 0 {
		t.Fatal("message entered history without an API key")
	}
}

func TestClearIsPersistent(t *testing.T) {
	useTempState(t)
	s := testSession("clear")
	if _, ok := saveSession(s, false); !ok {
		t.Fatal("save failed")
	}
	runREPLForTest(t, s, "/clear\n/exit\n")
	if got := len(loadSession("clear").Messages); got != 0 {
		t.Fatalf("clear was not persisted: %d messages", got)
	}
}

func TestEmptySessionCanRename(t *testing.T) {
	useTempState(t)
	s := newSession("empty-old", "openai_responses", "test", "", "https://example.invalid")
	runREPLForTest(t, s, "/rename empty-new\n/exit\n")
	if s.Name != "empty-new" {
		t.Fatalf("empty session was not renamed: %q", s.Name)
	}
}

func TestNewRejectsExistingName(t *testing.T) {
	useTempState(t)
	existing := testSession("work")
	saveSession(existing, false)
	current := newSession("current", "openai_responses", "test", "", "https://example.invalid")
	output := runREPLForTest(t, current, "/new work\n/exit\n")
	if !strings.Contains(output, "already exists") {
		t.Fatalf("missing collision warning: %s", output)
	}
	if got := loadSession("work").Messages[0].Content; got != "test" {
		t.Fatalf("existing session changed: %q", got)
	}
}

func TestAtomicFailurePreservesOriginalAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	original := []byte("{\"name\":\"original\"}\n")
	os.WriteFile(path, original, 0o600)
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if err := writeJSONFile(path, map[string]string{"name": "replacement"}); err == nil {
		t.Fatal("expected write failure")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(original) {
		t.Fatal("original session changed after failed atomic write")
	}

	useTempState(t)
	s := testSession("mode")
	saved, ok := saveSession(s, false)
	if !ok {
		t.Fatal("save failed")
	}
	info, _ := os.Stat(saved)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session mode is %o", info.Mode().Perm())
	}
	os.Chmod(saved, 0o644)
	ensureDirs()
	info, _ = os.Stat(saved)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy permissions not repaired: %o", info.Mode().Perm())
	}
}

func TestNewSessionWebDefaults(t *testing.T) {
	if !newSession("web", "openai_responses", "test", "", "").WebSearch {
		t.Fatal("Responses sessions should default to web search")
	}
	if newSession("other", "anthropic", "test", "", "").WebSearch {
		t.Fatal("other providers should default to no web search")
	}
	if newSession("off", "openai_responses", "test", "", "", false).WebSearch {
		t.Fatal("explicit web opt-out was ignored")
	}
}

func TestHelperMain(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER") != "1" {
		return
	}
	os.Args = []string{"frz", "--provider", "openai_responses", "--api-key", "x", "--session-name", "work"}
	main()
}

func TestMainRejectsExistingSessionName(t *testing.T) {
	root := t.TempDir()
	oldSess, oldAttach, oldConfig := sessDir, attachDir, configFile
	sessDir = filepath.Join(root, ".frz", "sessions")
	attachDir = filepath.Join(root, ".frz", "attachments")
	configFile = filepath.Join(root, ".frz", "config.json")
	ensureDirs()
	if path, ok := saveSession(testSession("work"), false); !ok {
		t.Fatal("failed to create startup collision fixture")
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("startup collision fixture missing: %v", err)
	} else {
		t.Logf("startup fixture: %s", path)
	}
	t.Cleanup(func() { sessDir, attachDir, configFile = oldSess, oldAttach, oldConfig })

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperMain")
	cmd.Env = []string{"GO_WANT_HELPER=1", "HOME=" + root, "PATH=" + os.Getenv("PATH")}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("main accepted an existing session name: %s", output)
	}
	if !strings.Contains(string(output), "already exists") {
		t.Fatalf("missing startup collision error: %s", output)
	}
}
