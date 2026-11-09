package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

func changeDirectory(target string) {
	if target == "" {
		target = os.Getenv("HOME")
	}
	if target == "~" {
		target = os.Getenv("HOME")
	} else if strings.HasPrefix(target, "~/") {
		target = filepath.Join(os.Getenv("HOME"), target[2:])
	}
	path, err := filepath.Abs(target)
	if err != nil {
		fmt.Println(stylize("cd: "+err.Error(), "red"))
		return
	}
	if err := os.Chdir(path); err != nil {
		fmt.Println(stylize("cd: "+err.Error(), "red"))
		return
	}
	fmt.Println(path)
}

func runShellCommand(command string) {
	fmt.Println(stylize("$ "+command, "gray"))
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if cmd.ProcessState != nil {
			fmt.Println(stylize(fmt.Sprintf("[exit status: %d]", cmd.ProcessState.ExitCode()), "gray"))
		} else {
			fmt.Println(stylize("command failed: "+err.Error(), "red"))
		}
	}
}

func splitShellWords(command string) ([]string, error) {
	words := []string{}
	var current []rune
	var quote rune
	escaped := false
	flush := func() {
		if len(current) > 0 {
			words = append(words, string(current))
			current = nil
		}
	}
	for _, r := range command {
		if escaped {
			current = append(current, r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current = append(current, r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		current = append(current, r)
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated shell quoting")
	}
	flush()
	return words, nil
}

func handleShellInput(input string) bool {
	explicit := strings.HasPrefix(input, "!")
	command := input
	if explicit {
		command = strings.TrimSpace(input[1:])
		if command == "" {
			fmt.Println("usage: !<command>, e.g. !ls -la (a lone ! does not enter a persistent shell)")
			return true
		}
	} else {
		first := strings.ToLower(strings.SplitN(command, " ", 2)[0])
		if first != "ls" && first != "pwd" && first != "cd" {
			return false
		}
	}
	parts, err := splitShellWords(command)
	if err != nil {
		fmt.Println(stylize("cannot parse command: "+err.Error(), "red"))
		return true
	}
	if len(parts) == 0 {
		return true
	}
	if parts[0] == "cd" && len(parts) <= 2 {
		if len(parts) == 1 {
			changeDirectory("")
		} else {
			changeDirectory(parts[1])
		}
	} else {
		runShellCommand(command)
	}
	return true
}

func pathCompletionMatches(text string) []string {
	raw := strings.TrimPrefix(text, "@")
	quote := ""
	if strings.HasPrefix(raw, "'") || strings.HasPrefix(raw, "\"") {
		quote, raw = raw[:1], raw[1:]
	}
	directory, base := filepath.Split(raw)
	readDir := directory
	if readDir == "" {
		readDir = "."
	}
	if strings.HasPrefix(readDir, "~/") {
		readDir = filepath.Join(os.Getenv("HOME"), readDir[2:])
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		return nil
	}
	matches := []string{}
	for _, entry := range entries {
		if base == "" && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !strings.HasPrefix(entry.Name(), base) {
			continue
		}
		candidate := entry.Name()
		if directory != "" {
			candidate = filepath.Join(directory, candidate)
		}
		if entry.IsDir() {
			candidate += string(os.PathSeparator)
		}
		activeQuote := quote
		if activeQuote == "" && strings.ContainsFunc(candidate, unicode.IsSpace) {
			activeQuote = "'"
		}
		closing := ""
		if activeQuote != "" && !entry.IsDir() {
			closing = activeQuote
		}
		matches = append(matches, "@"+activeQuote+candidate+closing)
		if len(matches) >= 100 {
			break
		}
	}
	sort.Strings(matches)
	return matches
}
