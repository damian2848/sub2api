package geminiweb

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Shape taken from the production startup page (2026-10-17): the picker is a
// doubly escaped feature-flag string next to unrelated ID lists.
const startupPagePicker = `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid" ` +
	`\"45749278\":\"[[\\\"8c46e95b1a07cecc\\\",\\\"1d44b34bcaa1c04d\\\"]]\",` +
	`\"45734007\":\"[[\\\"thinking\\\\u003de6fa609c3fa255c0,9d8ca3786ebdfbea,797f3d0293f288ad\\\",\\\"fast\\\\u003d56fdd199312815e2,fbb127bbb056c959\\\"]]\"`

func TestParseModelPicker(t *testing.T) {
	got := parseModelPicker(startupPagePicker)
	want := []string{"e6fa609c3fa255c0", "9d8ca3786ebdfbea", "797f3d0293f288ad", "56fdd199312815e2", "fbb127bbb056c959"}
	if len(got) != len(want) {
		t.Fatalf("ids=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids=%v, want %v", got, want)
		}
	}
}

func TestAvailableModels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		page    string
		models  []string
		unknown []string
		err     error
	}{
		{"picker", startupPagePicker,
			[]string{"gemini-3.1-pro", "gemini-3.8-flash", "gemini-3.6-flash"},
			[]string{"9d8ca3786ebdfbea", "797f3d0293f288ad"}, nil},
		{"no picker", `"SNlM0e":"token" "cfb2h":"build" "FdrFJe":"sid"`, nil, nil, ErrCatalogUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			do := func(req *http.Request) (*http.Response, error) {
				return response(http.StatusOK, tc.page, nil), nil
			}
			got, err := NewClient().AvailableModels(context.Background(), Credential{Cookie: "__Secure-1PSID=psid"}, do)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err=%v, want %v", err, tc.err)
			}
			ids := []string{}
			for _, m := range got.Models {
				ids = append(ids, m.ID)
			}
			if len(ids) != len(tc.models) || len(got.UnknownIDs) != len(tc.unknown) {
				t.Fatalf("models=%v unknown=%v", ids, got.UnknownIDs)
			}
			for i := range tc.models {
				if ids[i] != tc.models[i] {
					t.Fatalf("models=%v, want %v", ids, tc.models)
				}
			}
			for i := range tc.unknown {
				if got.UnknownIDs[i] != tc.unknown[i] {
					t.Fatalf("unknown=%v, want %v", got.UnknownIDs, tc.unknown)
				}
			}
		})
	}
}
