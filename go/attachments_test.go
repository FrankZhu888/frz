package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func useTempState(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	oldSess, oldAttach, oldConfig := sessDir, attachDir, configFile
	sessDir = filepath.Join(root, "sessions")
	attachDir = filepath.Join(root, "attachments")
	configFile = filepath.Join(root, "config.json")
	ensureDirs()
	t.Cleanup(func() { sessDir, attachDir, configFile = oldSess, oldAttach, oldConfig })
}

func testSession(name string) *Session {
	s := newSession(name, "openai_responses", "test", "", "https://example.invalid", false)
	s.Messages = append(s.Messages, Message{Role: "user", Content: "test"})
	return s
}

func testPNG() []byte {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	return data
}

func TestParseAtReferences(t *testing.T) {
	text, refs, err := parseAtReferences("mail a@example.com @@literal")
	if err != nil || len(refs) != 0 || text != "mail a@example.com @literal" {
		t.Fatalf("unexpected parse: text=%q refs=%v err=%v", text, refs, err)
	}
}

func TestTextAndImageAttachments(t *testing.T) {
	useTempState(t)
	dir := t.TempDir()
	textPath := filepath.Join(dir, "a.txt")
	imagePath := filepath.Join(dir, "image with space.png")
	os.WriteFile(textPath, []byte("hello 中文"), 0o600)
	os.WriteFile(imagePath, testPNG(), 0o600)
	m, err := buildUserMessage("analyze @"+textPath+" @'"+imagePath+"'", "openai_responses")
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "analyze" || len(m.Attachments) != 2 {
		t.Fatalf("unexpected message: %+v", m)
	}
	for _, a := range m.Attachments {
		if a.SHA256 == "" || a.Content != "" || a.Data != "" {
			t.Fatalf("attachment was not externalized: %+v", a)
		}
		if _, err := os.Stat(filepath.Join(attachDir, a.SHA256)); err != nil {
			t.Fatal(err)
		}
	}
	parts, err := responsesContent(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0]["type"] != "input_text" || parts[1]["type"] != "input_image" {
		t.Fatalf("unexpected content parts: %+v", parts)
	}
	if !strings.Contains(parts[0]["text"].(string), "hello 中文") {
		t.Fatal("text attachment missing from request")
	}
	if !strings.HasPrefix(parts[1]["image_url"].(string), "data:image/png;base64,") {
		t.Fatal("image data URL missing")
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyAttachmentGetsDefaultPrompt(t *testing.T) {
	useTempState(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(path, []byte("data"), 0o600)
	m, err := buildUserMessage("@"+path, "openai_responses")
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "Please analyze the attachment(s)." {
		t.Fatalf("unexpected default prompt: %q", m.Content)
	}
}

func TestIdenticalAttachmentsShareBlob(t *testing.T) {
	useTempState(t)
	path := filepath.Join(t.TempDir(), "same.txt")
	os.WriteFile(path, []byte("same"), 0o600)
	m, err := buildUserMessage("@"+path+" @"+path, "openai_responses")
	if err != nil {
		t.Fatal(err)
	}
	if m.Attachments[0].SHA256 != m.Attachments[1].SHA256 {
		t.Fatal("identical content has different hashes")
	}
	entries, _ := os.ReadDir(attachDir)
	if len(entries) != 1 {
		t.Fatalf("expected one blob, got %d", len(entries))
	}
}

func TestLegacyAttachmentsMigrateOnSave(t *testing.T) {
	useTempState(t)
	s := testSession("legacy")
	s.Messages[0].Attachments = []Attachment{
		{Kind: "text", Name: "old.txt", Content: "old text", Size: 8},
		{Kind: "image", Name: "old.png", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(testPNG())},
	}
	if _, ok := saveSession(s, false); !ok {
		t.Fatal("save failed")
	}
	stored := loadSession("legacy")
	for _, a := range stored.Messages[0].Attachments {
		if a.SHA256 == "" || a.Content != "" || a.Data != "" {
			t.Fatalf("legacy attachment not migrated: %+v", a)
		}
	}
	parts, err := responsesContent(stored.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parts[0]["text"].(string), "old text") || parts[1]["type"] != "input_image" {
		t.Fatal("migrated attachment request is incomplete")
	}
}

func TestMissingOrCorruptBlobFails(t *testing.T) {
	useTempState(t)
	path := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(path, []byte("data"), 0o600)
	m, err := buildUserMessage("@"+path, "openai_responses")
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(attachDir, m.Attachments[0].SHA256)
	os.Remove(blob)
	if _, err := responsesContent(m); err == nil {
		t.Fatal("expected missing blob error")
	}
	os.WriteFile(blob, []byte("replacement"), 0o600)
	if _, err := responsesContent(m); err == nil {
		t.Fatal("expected checksum error")
	}
}

func TestCleanPreservesReferencesAndRecentOrphans(t *testing.T) {
	useTempState(t)
	s := testSession("keep")
	digest, err := storeAttachment([]byte("keep"))
	if err != nil {
		t.Fatal(err)
	}
	s.Messages[0].Attachments = []Attachment{{Kind: "text", Name: "keep.txt", SHA256: digest}}
	if _, ok := saveSession(s, false); !ok {
		t.Fatal("save failed")
	}
	orphanDigest, _ := storeAttachment([]byte("orphan"))
	orphan := filepath.Join(attachDir, orphanDigest)
	if _, count, skipped, err := cleanUnreferencedAttachments(time.Hour); err != nil || count != 0 || skipped != 1 {
		t.Fatalf("unexpected grace cleanup: count=%d skipped=%d err=%v", count, skipped, err)
	}
	if _, _, _, err := cleanUnreferencedAttachments(0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan was not removed")
	}
	if _, err := os.Stat(filepath.Join(attachDir, digest)); err != nil {
		t.Fatal("referenced blob was removed")
	}
}

func TestCleanAbortsOnCorruptSession(t *testing.T) {
	useTempState(t)
	os.WriteFile(filepath.Join(sessDir, "bad.json"), []byte("not json"), 0o600)
	digest, _ := storeAttachment([]byte("orphan"))
	if _, _, _, err := cleanUnreferencedAttachments(0); err == nil {
		t.Fatal("expected cleanup refusal")
	}
	if _, err := os.Stat(filepath.Join(attachDir, digest)); err != nil {
		t.Fatal("blob removed after corrupt session")
	}
}

func TestBlobReuseRefreshesMtime(t *testing.T) {
	useTempState(t)
	digest, _ := storeAttachment([]byte("reused"))
	path := filepath.Join(attachDir, digest)
	old := time.Unix(1_000_000_000, 0)
	os.Chtimes(path, old, old)
	storeAttachment([]byte("reused"))
	info, _ := os.Stat(path)
	if !info.ModTime().After(old) {
		t.Fatal("reuse did not refresh mtime")
	}
}
