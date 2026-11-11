package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func captureVerboseStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	defer func() {
		os.Stderr = oldStderr
		_ = w.Close()
		_ = r.Close()
	}()
	fn()
	_ = w.Close()
	os.Stderr = oldStderr
	return <-done
}

func TestUpstreamFailureDiagnostics(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet", true: "verbose"}[enabled], func(t *testing.T) {
			var payload map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", "req-debug-123")
				w.Header().Set("X-Tt-Logid", "ark-log-456")
				_, _ = io.WriteString(w, "event: response.failed\n"+
					"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-debug-789\",\"status\":\"failed\",\"error\":{\"code\":\"search_backend_unavailable\"}}}\n\n")
			}))
			defer server.Close()

			oldVerbose := verbose
			verbose = enabled
			t.Cleanup(func() { verbose = oldVerbose })
			var callErr error
			debug := captureVerboseStderr(t, func() {
				_, callErr = callOpenAIResponses(context.Background(),
					[]Message{{Role: "user", Content: "private question"}}, "", "kimi-k3", "test-secret",
					server.URL, nil, nil, true, nil)
			})
			if callErr == nil {
				t.Fatal("expected upstream failure")
			}
			for _, expected := range []string{"response_id=resp-debug-789", "code=search_backend_unavailable"} {
				if !strings.Contains(callErr.Error(), expected) {
					t.Fatalf("error %q does not contain %q", callErr, expected)
				}
			}
			if strings.Contains(callErr.Error(), "unknown error") {
				t.Fatalf("real error was hidden: %v", callErr)
			}
			tools, ok := payload["tools"].([]interface{})
			if !ok || len(tools) != 1 || tools[0].(map[string]interface{})["type"] != "web_search" {
				t.Fatalf("web search tools missing: %#v", payload["tools"])
			}

			if !enabled {
				if debug != "" {
					t.Fatalf("quiet mode emitted debug logs: %s", debug)
				}
				return
			}
			for _, expected := range []string{
				"web_search=true", "response.failed", "search_backend_unavailable",
				"req-debug-123", "ark-log-456",
			} {
				if !strings.Contains(debug, expected) {
					t.Fatalf("debug output does not contain %q:\n%s", expected, debug)
				}
			}
			for _, secret := range []string{"test-secret", "private question"} {
				if strings.Contains(debug, secret) {
					t.Fatalf("debug output leaked %q:\n%s", secret, debug)
				}
			}
		})
	}
}

func TestVerboseFlagAndREPLToggle(t *testing.T) {
	flags, _, err := parseFlags([]string{"--verbose"}, mainFlagNames, mainBoolFlags)
	if err != nil || flags["verbose"] != "true" {
		t.Fatalf("--verbose was not accepted: flags=%v err=%v", flags, err)
	}
	if !strings.Contains(helpText, "/verbose [on|off]") || !strings.Contains(helpUsage, "--verbose") {
		t.Fatal("verbose help is incomplete")
	}
	matches, _ := (frzCompleter{}).Do([]rune("/verb"), len([]rune("/verb")))
	if len(matches) != 1 || string(matches[0]) != "ose" {
		t.Fatalf("verbose completion = %q, want suffix %q", matches, "ose")
	}

	oldVerbose := verbose
	verbose = false
	t.Cleanup(func() { verbose = oldVerbose })
	session := newSession("verbose-repl", "openai_responses", "test", "", "http://example.invalid", false)
	output := runREPLForTest(t, session,
		"/verbose on\n/verbose\n/verbose OFF\n/verbose\n/verbose invalid\n/exit\n")
	for _, expected := range []string{
		"verbose debug logging enabled", "on (stderr)", "verbose debug logging disabled",
		"verbose debug logging: off", "usage: /verbose [on|off]",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("REPL output does not contain %q:\n%s", expected, output)
		}
	}
	if verbose {
		t.Fatal("/verbose OFF did not disable debug logging")
	}
}

func TestVerboseEnvironmentAndURLRedaction(t *testing.T) {
	for _, value := range []string{"1", "true", "YES", "on"} {
		t.Setenv("FRZ_VERBOSE", value)
		if !envBool("FRZ_VERBOSE") {
			t.Fatalf("FRZ_VERBOSE=%q should enable verbose", value)
		}
	}
	for _, value := range []string{"", "0", "false", "off", "random"} {
		t.Setenv("FRZ_VERBOSE", value)
		if envBool("FRZ_VERBOSE") {
			t.Fatalf("FRZ_VERBOSE=%q should not enable verbose", value)
		}
	}

	safe := debugURL("https://user:password@example.invalid/responses?key=secret&x=1&access_token=token")
	for _, secret := range []string{"user:password", "=secret", "=token"} {
		if strings.Contains(safe, secret) {
			t.Fatalf("debug URL leaked %q: %s", secret, safe)
		}
	}
	if !strings.Contains(safe, "<redacted>") {
		t.Fatalf("debug URL was not redacted: %s", safe)
	}
}
