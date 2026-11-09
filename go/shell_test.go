package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShellClassificationAndQuoting(t *testing.T) {
	if handleShellInput("echo 你好") {
		t.Fatal("natural language was treated as a command")
	}
	parts, err := splitShellWords("cd 'path with spaces'")
	if err != nil || len(parts) != 2 || parts[1] != "path with spaces" {
		t.Fatalf("unexpected shell words: %v, %v", parts, err)
	}
}

func TestCdPersists(t *testing.T) {
	old, _ := os.Getwd()
	dir := filepath.Join(t.TempDir(), "with space")
	os.Mkdir(dir, 0o700)
	if !handleShellInput("cd '" + dir + "'") {
		t.Fatal("cd was not handled")
	}
	current, _ := os.Getwd()
	currentReal, _ := filepath.EvalSymlinks(current)
	dirReal, _ := filepath.EvalSymlinks(dir)
	if currentReal != dirReal {
		t.Fatalf("cwd=%q, want %q", currentReal, dirReal)
	}
	os.Chdir(old)
}

func TestPathCompletion(t *testing.T) {
	old, _ := os.Getwd()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(dir, "with space.png"), testPNG(), 0o600)
	os.Chdir(dir)
	defer os.Chdir(old)
	if got := pathCompletionMatches("@a"); len(got) != 1 || got[0] != "@a.txt" {
		t.Fatalf("unexpected file completion: %v", got)
	}
	if got := pathCompletionMatches("@w"); len(got) != 1 || got[0] != "@'with space.png'" {
		t.Fatalf("unexpected quoted completion: %v", got)
	}
}

func TestStreamWaitsForVisibleOutput(t *testing.T) {
	calls := 0
	s := newStreamRenderer(func() { calls++ })
	s.feed("partial")
	if calls != 0 {
		t.Fatal("spinner stopped on a partial delta")
	}
	s.feed(" line\nnext")
	if calls != 1 {
		t.Fatalf("expected first visible output, got %d", calls)
	}
	s.finish()
	if calls != 1 {
		t.Fatal("output callback ran more than once")
	}
}

func TestFinishWithoutNewlineSignalsOutput(t *testing.T) {
	calls := 0
	s := newStreamRenderer(func() { calls++ })
	s.feed("single line without newline")
	s.finish()
	if calls != 1 {
		t.Fatalf("finish did not signal output: %d", calls)
	}
}

func TestSearchStateCanOverrideThinking(t *testing.T) {
	indicator := newThinkingIndicator()
	indicator.setState("Pondering")
	indicator.setState("Searching")
	if indicator.state != "Pondering" {
		t.Fatalf("ordinary state update should not override: %q", indicator.state)
	}
	indicator.setState("Searching", true)
	if indicator.state != "Searching" {
		t.Fatalf("forced search update was ignored: %q", indicator.state)
	}
}
