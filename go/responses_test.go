package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responsesTestServer(t *testing.T, stream string, payload *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if payload != nil {
			json.NewDecoder(r.Body).Decode(payload)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, stream)
	}))
}

func TestWebSearchToolsAndEvents(t *testing.T) {
	var payload map[string]interface{}
	stream := "event: response.web_search_call.in_progress\n" +
		"data: {\"type\":\"response.web_search_call.in_progress\"}\n" +
		"event: response.web_search_call.searching\n" +
		"data: {\"type\":\"response.web_search_call.searching\"}\n" +
		"event: response.web_search_call.completed\n" +
		"data: {\"type\":\"response.web_search_call.completed\"}\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\"}\n"
	server := responsesTestServer(t, stream, &payload)
	defer server.Close()
	states := []string{}
	reply, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "search"}},
		"", "test", "secret", server.URL, nil, nil, true, func(state string) { states = append(states, state) })
	if err != nil || reply != "done" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if len(states) != 3 || states[0] != "in_progress" || states[2] != "completed" {
		t.Fatalf("unexpected states: %v", states)
	}
	tools, ok := payload["tools"].([]interface{})
	if !ok || len(tools) != 1 || tools[0].(map[string]interface{})["type"] != "web_search" {
		t.Fatalf("unexpected tools: %#v", payload["tools"])
	}
	if _, forced := payload["tool_choice"]; forced {
		t.Fatal("web search must remain automatic")
	}
}

func TestWebSearchDisabledOmitsTools(t *testing.T) {
	var payload map[string]interface{}
	server := responsesTestServer(t, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n", &payload)
	defer server.Close()
	_, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "plain"}},
		"", "test", "secret", server.URL, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["tools"]; ok {
		t.Fatal("tools present while web search is disabled")
	}
}

func TestEOFWithoutCompletedIsError(t *testing.T) {
	server := responsesTestServer(t, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n", nil)
	defer server.Close()
	_, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "test"}},
		"", "test", "secret", server.URL, nil, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("expected premature stream error, got %v", err)
	}
}

func TestIncompleteEventIsError(t *testing.T) {
	stream := "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"status_details\":{\"reason\":\"max_output_tokens\"}}}\n"
	server := responsesTestServer(t, stream, nil)
	defer server.Close()
	_, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "test"}},
		"", "test", "secret", server.URL, nil, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected incomplete error, got %v", err)
	}
}

func TestDataOnlyFailureIsNotHiddenByStaleEvent(t *testing.T) {
	stream := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"upstream failed\"}}}\n"
	server := responsesTestServer(t, stream, nil)
	defer server.Close()
	_, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "test"}},
		"", "test", "secret", server.URL, nil, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "upstream failed") {
		t.Fatalf("expected upstream failure, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (b failingBody) Close() error             { return nil }

func TestReadFailuresAreWrapped(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: failingBody{io.ErrUnexpectedEOF}, Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { httpClient = oldClient })
	_, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "test"}},
		"", "test", "secret", "https://example.invalid", nil, nil, false, nil)
	if err == nil {
		t.Fatal("expected response read error")
	}
	if _, err := httpPostJSON(context.Background(), "https://example.invalid", nil, nil); err == nil {
		t.Fatal("expected JSON read error")
	}
}

func TestNetworkErrorIsReturned(t *testing.T) {
	oldClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("timeout")
	})}
	t.Cleanup(func() { httpClient = oldClient })
	_, err := callOpenAIResponses(context.Background(), []Message{{Role: "user", Content: "test"}},
		"", "test", "secret", "https://example.invalid", nil, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "network error") {
		t.Fatalf("expected network error, got %v", err)
	}
}

func TestEmptyResponseIsNotSavedOrSentNextRound(t *testing.T) {
	useTempState(t)
	var payloads []map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		payloads = append(payloads, payload)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(payloads) > 1 {
			_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()

	session := newSession("empty-response", "openai_responses", "test", "", server.URL, true)
	sendMessage(session, "first question", "openai_responses", "test-key")
	if len(session.Messages) != 0 {
		t.Fatalf("empty response was recorded: %#v", session.Messages)
	}

	sendMessage(session, "second question", "openai_responses", "test-key")
	if len(payloads) != 2 {
		t.Fatalf("got %d requests, want 2", len(payloads))
	}
	input, ok := payloads[1]["input"].([]interface{})
	if !ok || len(input) != 1 {
		t.Fatalf("second request input = %#v, want one user message", payloads[1]["input"])
	}
	item := input[0].(map[string]interface{})
	if item["role"] != "user" {
		t.Fatalf("second request role = %#v, want user", item["role"])
	}
	content := item["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"]
	if text != "second question" {
		t.Fatalf("second request text = %#v", text)
	}
	if len(session.Messages) != 2 || session.Messages[1].Content != "ok" {
		t.Fatalf("normal response was not recorded: %#v", session.Messages)
	}
}

func TestRequestBuilderFiltersEmptyAssistant(t *testing.T) {
	var payload map[string]interface{}
	stream := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n"
	server := responsesTestServer(t, stream, &payload)
	defer server.Close()
	messages := []Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: ""},
		{Role: "assistant", Content: "   \n\t"},
		{Role: "user", Content: "new question"},
	}
	reply, err := callOpenAIResponses(context.Background(), messages, "", "test", "secret", server.URL, nil, nil, false, nil)
	if err != nil || reply != "ok" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	input, ok := payload["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("request input = %#v, want two user messages", payload["input"])
	}
	for i, raw := range input {
		item := raw.(map[string]interface{})
		if item["role"] != "user" {
			t.Fatalf("input[%d] role = %#v, want user", i, item["role"])
		}
	}
	content := input[1].(map[string]interface{})["content"].([]interface{})
	if got := content[0].(map[string]interface{})["text"]; got != "new question" {
		t.Fatalf("last input text = %#v", got)
	}
}
