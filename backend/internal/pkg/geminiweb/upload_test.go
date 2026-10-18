package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestClientCompleteUploadsAttachmentsOnceAndReferencesThem(t *testing.T) {
	var mu sync.Mutex
	uploads := map[string]string{}
	generates := 0
	do := func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "content-push.googleapis.com":
			if req.Header.Get("Push-ID") != uploadPushID || req.Header.Get("Cookie") != "" {
				t.Fatalf("upload headers: push-id=%q cookie=%q", req.Header.Get("Push-ID"), req.Header.Get("Cookie"))
			}
			file, header, err := req.FormFile("file")
			if err != nil {
				t.Fatalf("upload form: %v", err)
			}
			data, _ := io.ReadAll(file)
			mu.Lock()
			uploads[header.Filename] = string(data)
			mu.Unlock()
			if got := header.Header.Get("Content-Type"); got == "" {
				t.Fatalf("upload part has no content type")
			}
			return response(http.StatusOK, "/contrib_service/ttl_1d/"+header.Filename+"\n", nil), nil
		case req.Method == http.MethodGet:
			return response(http.StatusOK, `<html>"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"</html>`, nil), nil
		}
		generates++
		if generates == 1 {
			// A stale session is retried; the retry must reuse the uploads.
			return response(http.StatusBadRequest, "", nil), nil
		}
		body, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(body))
		var outer []any
		if err := json.Unmarshal([]byte(values.Get("f.req")), &outer); err != nil {
			t.Fatalf("f.req: %v", err)
		}
		innerText, ok := outer[1].(string)
		if !ok {
			t.Fatalf("f.req[1] = %T, want string", outer[1])
		}
		var inner []any
		if err := json.Unmarshal([]byte(innerText), &inner); err != nil {
			t.Fatalf("inner: %v", err)
		}
		message, ok := inner[0].([]any)
		if !ok || len(message) < 4 {
			t.Fatalf("inner[0] = %#v, want the message array", inner[0])
		}
		files, _ := json.Marshal(message[3])
		want := `[[["/contrib_service/ttl_1d/cat.png"],"cat.png"],[["/contrib_service/ttl_1d/attachment-2.pdf"],"attachment-2.pdf"]]`
		if string(files) != want {
			t.Fatalf("message files = %s, want %s", files, want)
		}
		return response(http.StatusOK, frameBody("a cat and a pdf"), nil), nil
	}
	result, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid", BaseURL: "https://gemini.test"}, Request{
		Model: "gemini-3.8-flash", Prompt: "what are these?",
		Attachments: []Attachment{
			{Name: "cat.png", MimeType: "image/png", Data: []byte("png")},
			{MimeType: "application/pdf", Data: []byte("pdf")},
		},
	}, do)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if result.Text != "a cat and a pdf" || result.Timing.Attachments != 2 || result.Timing.Upload <= 0 {
		t.Fatalf("unexpected result: text=%q timing=%+v", result.Text, result.Timing)
	}
	if len(uploads) != 2 || uploads["cat.png"] != "png" || uploads["attachment-2.pdf"] != "pdf" {
		t.Fatalf("uploads = %v, want each attachment uploaded once", uploads)
	}
}

func TestClientCompleteFailsWhenUploadFails(t *testing.T) {
	do := func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "content-push.googleapis.com" {
			return response(http.StatusOK, "<html>error</html>", nil), nil
		}
		t.Fatalf("no request may follow a failed upload: %s", req.URL)
		return nil, nil
	}
	_, err := NewClient().Complete(context.Background(), Credential{Cookie: "__Secure-1PSID=psid"}, Request{
		Model: "gemini-3.8-flash", Prompt: "x", Attachments: []Attachment{{MimeType: "image/png", Data: []byte("png")}},
	}, do)
	if !errors.Is(err, ErrUploadFailed) {
		t.Fatalf("err = %v, want ErrUploadFailed", err)
	}
}

func TestAttachmentNameUsesMimeExtension(t *testing.T) {
	for mimeType, want := range map[string]string{
		"image/jpeg":                "attachment-1.jpg",
		"IMAGE/PNG":                 "attachment-1.png",
		"text/plain; charset=utf-8": "attachment-1.txt",
		"application/x-unknown":     "attachment-1.bin",
	} {
		if got := AttachmentName(Attachment{MimeType: mimeType}, 0); got != want {
			t.Errorf("AttachmentName(%q) = %q, want %q", mimeType, got, want)
		}
	}
	if got := AttachmentName(Attachment{Name: " report.pdf ", MimeType: "image/png"}, 3); got != "report.pdf" {
		t.Errorf("explicit name = %q", got)
	}
	if strings.Contains(AttachmentName(Attachment{}, 1), "/") {
		t.Errorf("name must not contain a path")
	}
}
