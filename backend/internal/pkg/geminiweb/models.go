package geminiweb

import "strings"

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
