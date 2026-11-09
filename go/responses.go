package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func jsonText(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func responsesContent(m Message) ([]map[string]interface{}, error) {
	attachments := m.Attachments
	textSections := []string{m.Content}
	for _, a := range attachments {
		if a.Kind != "text" {
			continue
		}
		content, err := attachmentText(a)
		if err != nil {
			return nil, err
		}
		textSections = append(textSections, fmt.Sprintf("\n\n<attached_file name=%s path=%s>\n%s\n</attached_file>",
			jsonText(a.Name), jsonText(a.Path), content))
	}
	parts := []map[string]interface{}{}
	text := strings.Join(textSections, "")
	if text != "" || len(attachments) == 0 {
		parts = append(parts, map[string]interface{}{"type": "input_text", "text": text})
	}
	for _, a := range attachments {
		if a.Kind == "text" {
			continue
		}
		if a.Kind != "image" {
			return nil, fmt.Errorf("unknown attachment type: %s", a.Kind)
		}
		if a.MediaType == "" {
			return nil, fmt.Errorf("image attachment has no MIME type: %s", a.Name)
		}
		data, err := attachmentBytes(a)
		if err != nil {
			return nil, err
		}
		parts = append(parts, map[string]interface{}{
			"type": "input_image", "image_url": fmt.Sprintf("data:%s;base64,%s", a.MediaType, base64.StdEncoding.EncodeToString(data)),
		})
	}
	return parts, nil
}

// callOpenAIResponses speaks the OpenAI Responses API with SSE streaming.
func callOpenAIResponses(ctx context.Context, messages []Message, system, model, apiKey, baseURL string,
	onDelta, onReasoning func(string), webSearch bool, onSearch func(string)) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("openai_responses provider requires base_url; pass --base-url.")
	}
	items := []map[string]interface{}{}
	for _, m := range withoutEmptyAssistant(messages) {
		content, err := responsesContent(m)
		if err != nil {
			return "", err
		}
		items = append(items, map[string]interface{}{"role": m.Role, "content": content})
	}
	payload := map[string]interface{}{"model": model, "input": items, "stream": true}
	if webSearch {
		// Declaration only: the model decides whether a search is needed.
		payload["tools"] = []map[string]interface{}{{"type": "web_search"}}
	}
	if system != "" {
		payload["instructions"] = system
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+"/responses", bytes.NewReader(data))
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
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return "", fmt.Errorf("HTTP %d; failed to read error response: %v", resp.StatusCode, readErr)
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var full strings.Builder
	currentEvent := ""
	completed := false
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(line[len("event:"):])
			} else if strings.HasPrefix(line, "data:") {
				dataStr := strings.TrimSpace(line[len("data:"):])
				eventForData := currentEvent
				currentEvent = ""
				if dataStr != "" && dataStr != "[DONE]" {
					var evt map[string]interface{}
					if json.Unmarshal([]byte(dataStr), &evt) == nil {
						etype := asString(evt["type"])
						if etype == "" {
							etype = eventForData
						}
						switch {
						case etype == "response.output_text.delta":
							if d := asString(evt["delta"]); d != "" {
								full.WriteString(d)
								if onDelta != nil {
									onDelta(d)
								}
							}
						case etype == "response.reasoning_summary_text.delta":
							if onReasoning != nil {
								onReasoning(asString(evt["delta"]))
							}
						case strings.HasPrefix(etype, "response.web_search_call."):
							if onSearch != nil {
								parts := strings.Split(etype, ".")
								onSearch(parts[len(parts)-1])
							}
						case etype == "error" || etype == "response.failed":
							response := asMap(evt["response"])
							msg := asString(asMap(response["error"])["message"])
							if msg == "" {
								msg = asString(asMap(evt["error"])["message"])
							}
							if msg == "" {
								msg = "unknown error"
							}
							return "", fmt.Errorf("upstream error: %s", msg)
						case etype == "response.incomplete":
							response := asMap(evt["response"])
							details := asMap(response["status_details"])
							reason := asString(details["reason"])
							if reason == "" {
								reason = asString(response["status"])
							}
							if reason == "" {
								reason = "unknown reason"
							}
							return "", fmt.Errorf("incomplete upstream response: %s", reason)
						case etype == "response.completed":
							completed = true
						}
					}
				}
			}
		}
		if completed {
			break
		}
		if readErr != nil {
			if ctx.Err() == context.Canceled {
				return "", errInterrupted
			}
			return "", fmt.Errorf("stream ended prematurely (before response.completed): %v", readErr)
		}
	}
	return full.String(), nil
}
