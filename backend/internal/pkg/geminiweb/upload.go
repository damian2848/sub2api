package geminiweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
)

// The browser uploads attachments to Google's content-push service before the
// StreamGenerate call and then references the returned identifier, such as
// "/contrib_service/ttl_1d/...", in the prompt. The upload is authorized by the
// Push-ID header alone; no account cookie is sent.
const (
	uploadEndpoint = "https://content-push.googleapis.com/upload"
	uploadPushID   = "feeds/mcudyrk2a4khkz"
)

// Attachment is a file sent with the prompt, as the browser's upload button
// does. Name should carry an extension that matches MimeType: Gemini Web uses
// it to pick how the file is read.
type Attachment struct {
	Name     string
	MimeType string
	Data     []byte
}

var ErrUploadFailed = errors.New("gemini web attachment upload failed")

// uploadAttachments uploads every attachment concurrently and returns the
// file list for slot 3 of the message content: [[[id], name], ...].
func (c *Client) uploadAttachments(ctx context.Context, cred Credential, attachments []Attachment, do Doer) ([]any, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	ids := make([]string, len(attachments))
	errs := make([]error, len(attachments))
	var wg sync.WaitGroup
	names := make([]string, len(attachments))
	for i := range attachments {
		names[i] = AttachmentName(attachments[i], i)
		att := attachments[i]
		att.Name = names[i]
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = uploadAttachment(ctx, cred, att, do)
		}(i)
	}
	wg.Wait()
	files := make([]any, len(attachments))
	for i := range attachments {
		if errs[i] != nil {
			return nil, errs[i]
		}
		files[i] = []any{[]any{ids[i]}, names[i]}
	}
	return files, nil
}

func uploadAttachment(ctx context.Context, cred Credential, att Attachment, do Doer) (string, error) {
	if len(att.Data) == 0 {
		return "", fmt.Errorf("%w: attachment is empty", ErrUploadFailed)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": att.Name}))
	header.Set("Content-Type", defaultString(att.MimeType, "application/octet-stream"))
	part, err := form.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(att.Data); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadEndpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Push-ID", uploadPushID)
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("User-Agent", defaultString(cred.UserAgent, defaultUserAgent))
	resp, err := do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUploadFailed, err)
	}
	if resp == nil || resp.Body == nil {
		return "", fmt.Errorf("%w: no response body", ErrUploadFailed)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: %s", ErrUploadFailed, resp.Status)
	}
	id := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(id, "/") || strings.ContainsAny(id, " \n<") {
		return "", fmt.Errorf("%w: unexpected upload response", ErrUploadFailed)
	}
	return id, nil
}

// commonExtensions are the extensions Gemini Web expects for the types it
// reads. mime.ExtensionsByType is only the fallback: it returns several
// extensions in an order that depends on the host's mime tables.
var commonExtensions = map[string]string{
	"image/png":        ".png",
	"image/jpeg":       ".jpg",
	"image/webp":       ".webp",
	"image/gif":        ".gif",
	"image/heic":       ".heic",
	"image/heif":       ".heif",
	"application/pdf":  ".pdf",
	"text/plain":       ".txt",
	"text/csv":         ".csv",
	"text/markdown":    ".md",
	"text/html":        ".html",
	"application/json": ".json",
	"audio/mpeg":       ".mp3",
	"audio/mp3":        ".mp3",
	"audio/wav":        ".wav",
	"audio/x-wav":      ".wav",
	"audio/ogg":        ".ogg",
	"audio/flac":       ".flac",
	"audio/aac":        ".aac",
	"video/mp4":        ".mp4",
	"video/webm":       ".webm",
	"video/quicktime":  ".mov",
}

// AttachmentName is the file name sent for attachment i: its own name when it
// has one, otherwise "attachment-<n>" with an extension for its mime type.
func AttachmentName(att Attachment, i int) string {
	if name := strings.TrimSpace(att.Name); name != "" {
		return name
	}
	mediaType, _, _ := mime.ParseMediaType(att.MimeType)
	ext := commonExtensions[strings.ToLower(mediaType)]
	if ext == "" {
		if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
			ext = exts[0]
		} else {
			ext = ".bin"
		}
	}
	return fmt.Sprintf("attachment-%d%s", i+1, ext)
}
