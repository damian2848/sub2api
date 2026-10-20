package geminiweb

import (
	"regexp"
	"strings"
)

// Model describes the small model selector used by Gemini's web client.  The
// web endpoint receives a mode number and, for some modes, an opaque model id;
// it does not receive the public model name.
type Model struct {
	ID       string
	Mode     int
	Think    int
	Capacity int
	HeaderID string
}

var models = []Model{
	{ID: "gemini-4-argon", Mode: 8, Think: 4, Capacity: 2, HeaderID: "95a4c0abcba24364"},
	{ID: "gemini-3.8-flash", Mode: 1, Think: 4, Capacity: 2, HeaderID: "56fdd199312815e2"},
	{ID: "gemini-3.8-flash-thinking", Mode: 2, Think: 0},
	{ID: "gemini-3.1-pro", Mode: 3, Think: 4, Capacity: 2, HeaderID: "e6fa609c3fa255c0"},
	{ID: "gemini-auto", Mode: 4, Think: 4},
	{ID: "gemini-3.8-flash-thinking-lite", Mode: 5, Think: 0},
	{ID: "gemini-3.5-flash-lite", Mode: 6, Think: 4, Capacity: 1, HeaderID: "cf41b0e0dd7d53e5"},
	{ID: "gemini-3.6-flash", Mode: 1, Think: 4, Capacity: 1, HeaderID: "fbb127bbb056c959"},
}

var aliases = map[string]string{
	"gemini-flash": "gemini-3.8-flash", "gemini-flash-plus": "gemini-3.8-flash",
	"gemini-flash-thinking": "gemini-3.8-flash-thinking", "gemini-pro": "gemini-3.1-pro",
	"gemini-3-flash": "gemini-3.8-flash", "gemini-3-pro": "gemini-3.1-pro",
	"gemini-2.5-flash": "gemini-3.8-flash", "gemini-2.5-pro": "gemini-3.1-pro",
	"gemini-1.5-flash": "gemini-3.8-flash", "gemini-1.5-pro": "gemini-3.1-pro",
}

func ResolveModel(id string) Model {
	model, ok := LookupModel(id)
	if ok {
		return model
	}
	return models[0]
}

func LookupModel(id string) (Model, bool) {
	id = strings.TrimSpace(strings.ToLower(id))
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	if canonical, ok := aliases[id]; ok {
		for _, m := range models {
			if m.ID == canonical {
				return m, true
			}
		}
	}
	return Model{}, false
}

func Models() []Model { return append([]Model(nil), models...) }

// DefaultModelMapping is the public model policy for Gemini Web accounts that
// have no explicit mapping. Keep this separate from the full protocol catalog:
// administrators may explicitly enable other supported Web modes.
func DefaultModelMapping() map[string]string {
	return map[string]string{
		"gemini-4-argon":        "gemini-4-argon",
		"gemini-3.8-flash":      "gemini-3.8-flash",
		"gemini-3.5-flash-lite": "gemini-3.5-flash-lite",
		"gemini-3.8-flash-high": "gemini-3.8-flash",
		"gemini-4-argon-high":   "gemini-4-argon",
		"gemini-4-argon-low":    "gemini-4-argon",
		"gemini-4-argon-medium": "gemini-4-argon",
	}
}

// responseModel translates known Web protocol IDs into public catalog names.
// Google rotates these IDs, so an unknown ID is named from the display label
// sent beside it (payload[42], e.g. "3.5 Flash-Lite"). The label never
// replaces a missing ID, and an unparseable label keeps the raw ID for audit
// rather than guessing a requested model.
func responseModel(id, label string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for _, model := range models {
		if model.HeaderID != "" && strings.EqualFold(model.HeaderID, id) {
			return model.ID
		}
	}
	if name := modelNameFromLabel(label); name != "" {
		return name
	}
	// Match the shared response observer's bound on untrusted declarations.
	runes := []rune(id)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return id
}

var labelModelPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(-[a-z0-9]+)+$`)

// modelNameFromLabel maps a versioned label such as "3.5 Flash-Lite" or
// "Gemini 4 Argon" to its catalog-style name. Mode labels without a version
// ("Fast", "Thinking") are not model identities and return "".
func modelNameFromLabel(label string) string {
	name := strings.ToLower(strings.TrimSpace(label))
	name = strings.TrimSpace(strings.TrimPrefix(name, "gemini"))
	name = strings.Join(strings.Fields(name), "-")
	if len(name) > 64 || !labelModelPattern.MatchString(name) {
		return ""
	}
	return "gemini-" + name
}
