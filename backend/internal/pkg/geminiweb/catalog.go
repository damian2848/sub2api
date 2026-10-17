package geminiweb

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// ErrCatalogUnavailable means the startup page loaded but listed no model
// picker entries, so the account's real model set cannot be determined.
var ErrCatalogUnavailable = errors.New("gemini web startup page did not list any models")

// Catalog is the model set an account's Gemini Web picker currently offers.
type Catalog struct {
	// Models are built-in models whose Web ID the picker lists. Modes without
	// a Web ID (auto, the thinking modes) cannot be confirmed and are left out.
	Models []Model
	// UnknownIDs are listed IDs missing from the built-in table; they cannot
	// be requested until their mode and capacity are known.
	UnknownIDs []string
	// UpdatedCookie is the merged cookie after the startup request, if Google
	// rotated it.
	UpdatedCookie string
}

// The startup page carries the picker as feature-flag strings such as
// "thinking=e6fa609c3fa255c0,9d8ca3786ebdfbea" and "fast=56fdd199312815e2".
// Match the group shape rather than the flag number, which Google renumbers.
var pickerGroupPattern = regexp.MustCompile(`"([a-z][a-z_]*)=([0-9a-f]{16}(?:,[0-9a-f]{16})*)"`)

// parseModelPicker returns the picker's Web model IDs in page order.
func parseModelPicker(page string) []string {
	page = strings.NewReplacer(`\\u003d`, "=", `=`, "=", `=`, "=").Replace(page)
	page = strings.ReplaceAll(page, `\`, "")
	seen := map[string]bool{}
	var ids []string
	for _, m := range pickerGroupPattern.FindAllStringSubmatch(page, -1) {
		for _, id := range strings.Split(m[2], ",") {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// AvailableModels loads the account's startup page and reports which built-in
// models its picker offers. A new Client fetches the page fresh; a shared
// Client may answer from its cached session.
func (c *Client) AvailableModels(ctx context.Context, cred Credential, do Doer) (Catalog, error) {
	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if base == "" {
		base = "https://gemini.google.com"
	}
	sess, cookie, err := c.getSession(ctx, cred, base, do)
	out := Catalog{}
	if cookie != "" && cookie != cred.Cookie {
		out.UpdatedCookie = cookie
	}
	if err != nil {
		return out, err
	}
	if len(sess.ModelIDs) == 0 {
		return out, ErrCatalogUnavailable
	}
	for _, id := range sess.ModelIDs {
		known := false
		for _, m := range models {
			if m.HeaderID != "" && strings.EqualFold(m.HeaderID, id) {
				out.Models = append(out.Models, m)
				known = true
				break
			}
		}
		if !known {
			out.UnknownIDs = append(out.UnknownIDs, id)
		}
	}
	return out, nil
}
