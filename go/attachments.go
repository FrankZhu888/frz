package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxAttachments          = 8
	maxTextAttachmentBytes  = 2 * 1024 * 1024
	maxImageAttachmentBytes = 10 * 1024 * 1024
	maxTotalAttachmentBytes = 15 * 1024 * 1024
	attachmentCleanGrace    = 24 * time.Hour
)

// Attachment is the canonical, provider-independent form. Content and Data are
// retained only for reading and migrating sessions written by older versions.
type Attachment struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int    `json:"size"`
	MediaType string `json:"media_type,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Content   string `json:"content,omitempty"`
	Data      string `json:"data,omitempty"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func writeBytesAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".frz-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func attachmentBlobPath(digest string) (string, error) {
	if !sha256Pattern.MatchString(digest) {
		return "", fmt.Errorf("invalid attachment SHA-256 in session")
	}
	return filepath.Join(attachDir, digest), nil
}

func storeAttachment(data []byte) (string, error) {
	ensureDirs()
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	path := filepath.Join(attachDir, digest)
	if existing, err := os.ReadFile(path); err == nil {
		current := sha256.Sum256(existing)
		if hex.EncodeToString(current[:]) == digest {
			now := time.Now()
			if err := os.Chtimes(path, now, now); err == nil {
				return digest, nil
			}
		}
	}
	if err := writeBytesAtomic(path, data, 0o600); err != nil {
		return "", err
	}
	return digest, nil
}

func attachmentBytes(a Attachment) ([]byte, error) {
	if a.SHA256 != "" {
		path, err := attachmentBlobPath(a.SHA256)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("attachment snapshot missing or unreadable: %s (%s)", a.Name, a.SHA256[:12])
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != a.SHA256 {
			return nil, fmt.Errorf("attachment checksum failed: %s (%s)", a.Name, a.SHA256[:12])
		}
		return data, nil
	}
	if a.Kind == "image" && a.Data != "" {
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("invalid legacy image Base64: %s", a.Name)
		}
		return data, nil
	}
	if a.Kind == "text" && a.Content != "" {
		return []byte(a.Content), nil
	}
	return nil, fmt.Errorf("incomplete attachment data: %s", a.Name)
}

func attachmentText(a Attachment) (string, error) {
	data, err := attachmentBytes(a)
	if err != nil {
		return "", err
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", fmt.Errorf("text attachment is not valid UTF-8: %s", a.Name)
	}
	return string(data), nil
}

func externalizeSessionAttachments(s *Session) error {
	for i := range s.Messages {
		for j := range s.Messages[i].Attachments {
			a := &s.Messages[i].Attachments[j]
			if a.SHA256 != "" {
				continue
			}
			data, err := attachmentBytes(*a)
			if err != nil {
				return err
			}
			digest, err := storeAttachment(data)
			if err != nil {
				return err
			}
			a.SHA256, a.Size = digest, len(data)
			a.Content, a.Data = "", ""
		}
	}
	return nil
}

func referencedAttachmentHashes() (map[string]bool, error) {
	refs := map[string]bool{}
	paths, _ := filepath.Glob(filepath.Join(sessDir, "*.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read session %s; attachment cleanup skipped", filepath.Base(path))
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("cannot parse session %s; attachment cleanup skipped", filepath.Base(path))
		}
		for _, m := range s.Messages {
			for _, a := range m.Attachments {
				if sha256Pattern.MatchString(a.SHA256) {
					refs[a.SHA256] = true
				}
			}
		}
	}
	return refs, nil
}

func cleanUnreferencedAttachments(maxAge time.Duration) (int, int, int, error) {
	ensureDirs()
	refs, err := referencedAttachmentHashes()
	if err != nil {
		return 0, 0, 0, err
	}
	entries, err := os.ReadDir(attachDir)
	if err != nil {
		return 0, 0, 0, err
	}
	removedBytes, removedCount, skipped := 0, 0, 0
	for _, entry := range entries {
		if !sha256Pattern.MatchString(entry.Name()) || refs[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if time.Since(info.ModTime()) < maxAge {
			skipped++
			continue
		}
		if err := os.Remove(filepath.Join(attachDir, entry.Name())); err == nil {
			removedBytes += int(info.Size())
			removedCount++
		}
	}
	return removedBytes, removedCount, skipped, nil
}

func parseAtReferences(text string) (string, []string, error) {
	r := []rune(text)
	out := []rune{}
	refs := []string{}
	for i := 0; i < len(r); {
		if r[i] != '@' || (i > 0 && !unicode.IsSpace(r[i-1])) {
			out = append(out, r[i])
			i++
			continue
		}
		if i+1 < len(r) && r[i+1] == '@' {
			out = append(out, '@')
			i += 2
			continue
		}
		i++
		if i >= len(r) {
			out = append(out, '@')
			break
		}
		var path string
		if r[i] == '\'' || r[i] == '"' {
			quote := r[i]
			i++
			chars := []rune{}
			closed := false
			for i < len(r) {
				if r[i] == '\\' && i+1 < len(r) && (r[i+1] == quote || r[i+1] == '\\') {
					chars = append(chars, r[i+1])
					i += 2
					continue
				}
				if r[i] == quote {
					closed = true
					i++
					break
				}
				chars = append(chars, r[i])
				i++
			}
			if !closed {
				return "", nil, fmt.Errorf("unterminated quote in @ attachment path")
			}
			if i < len(r) && !unicode.IsSpace(r[i]) {
				return "", nil, fmt.Errorf("quoted @ attachment paths must be followed by whitespace")
			}
			path = string(chars)
		} else {
			start := i
			for i < len(r) && !unicode.IsSpace(r[i]) {
				i++
			}
			path = string(r[start:i])
		}
		if path == "" {
			out = append(out, '@')
		} else {
			refs = append(refs, path)
		}
	}
	return strings.TrimSpace(string(out)), refs, nil
}

func imageMediaType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func loadAttachment(reference string, totalBytes int) (Attachment, int, error) {
	if strings.HasPrefix(reference, "~/") {
		reference = filepath.Join(os.Getenv("HOME"), reference[2:])
	}
	path, err := filepath.Abs(reference)
	if err != nil {
		return Attachment{}, 0, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Attachment{}, 0, fmt.Errorf("attachment does not exist or is not a regular file: %s", reference)
	}
	if totalBytes+int(info.Size()) > maxTotalAttachmentBytes {
		return Attachment{}, 0, fmt.Errorf("attachments exceed the %d MiB total limit: %s", maxTotalAttachmentBytes/1024/1024, info.Name())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Attachment{}, 0, err
	}
	size := len(data)
	base := Attachment{Name: info.Name(), Path: path, Size: size}
	if mediaType := imageMediaType(data); mediaType != "" {
		if size > maxImageAttachmentBytes {
			return Attachment{}, 0, fmt.Errorf("image exceeds the %d MiB limit: %s", maxImageAttachmentBytes/1024/1024, info.Name())
		}
		digest, err := storeAttachment(data)
		if err != nil {
			return Attachment{}, 0, err
		}
		base.Kind, base.MediaType, base.SHA256 = "image", mediaType, digest
		return base, size, nil
	}
	if size > maxTextAttachmentBytes {
		return Attachment{}, 0, fmt.Errorf("text attachment exceeds the %d MiB limit: %s", maxTextAttachmentBytes/1024/1024, info.Name())
	}
	if bytes.Contains(data, []byte{0}) || !utf8.Valid(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})) {
		return Attachment{}, 0, fmt.Errorf("unsupported binary file (UTF-8 text and common images only): %s", info.Name())
	}
	digest, err := storeAttachment(data)
	if err != nil {
		return Attachment{}, 0, err
	}
	base.Kind, base.SHA256 = "text", digest
	return base, size, nil
}

func buildUserMessage(input, provider string) (Message, error) {
	text, refs, err := parseAtReferences(input)
	if err != nil {
		return Message{}, err
	}
	if len(refs) > 0 && provider != "openai_responses" {
		return Message{}, fmt.Errorf("@ attachments currently require the openai_responses provider")
	}
	if len(refs) > maxAttachments {
		return Message{}, fmt.Errorf("at most %d attachments are allowed per message", maxAttachments)
	}
	attachments := []Attachment{}
	total := 0
	for _, ref := range refs {
		a, size, err := loadAttachment(ref, total)
		if err != nil {
			return Message{}, err
		}
		attachments = append(attachments, a)
		total += size
	}
	if text == "" && len(attachments) > 0 {
		text = "Please analyze the attachment(s)."
	}
	return Message{Role: "user", Content: text, Attachments: attachments}, nil
}

func formatSize(size int) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(size)/1024/1024)
}

func attachmentLabels(m Message) []string {
	labels := []string{}
	for _, a := range m.Attachments {
		kind := "text"
		if a.Kind == "image" {
			kind = "image"
		}
		labels = append(labels, fmt.Sprintf("%s (%s, %s)", a.Name, kind, formatSize(a.Size)))
	}
	return labels
}
