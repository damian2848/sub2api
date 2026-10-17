package geminiweb

import (
	"strconv"
	"testing"
	"time"
)

func TestParseQuotaPayloadResetShapes(t *testing.T) {
	observed := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	soon := observed.Add(2*time.Hour + 13*time.Minute)
	cases := []struct {
		name      string
		bucket    []any
		wantReset bool
		want      time.Time
	}{
		{name: "timestamp at index 3", bucket: []any{"", "0.1", 1, []any{strconv.FormatInt(soon.Unix(), 10), "0"}}, wantReset: true, want: soon},
		{name: "seconds without nanos", bucket: []any{"", "0.1", 1, []any{strconv.FormatInt(soon.Unix(), 10)}}, wantReset: true, want: soon},
		{name: "wrapped timestamp", bucket: []any{"", "0.1", 1, []any{[]any{strconv.FormatInt(soon.Unix(), 10), nil}}}, wantReset: true, want: soon},
		{name: "RFC3339 string", bucket: []any{"", "0.1", 1, soon.Format(time.RFC3339)}, wantReset: true, want: soon},
		{name: "shifted to a later field", bucket: []any{"", "0.1", 1, nil, []any{strconv.FormatInt(soon.Unix(), 10), "0"}}, wantReset: true, want: soon},
		{name: "implausible later field ignored", bucket: []any{"", "0.1", 1, nil, []any{"1000000000", "0"}}},
		{name: "missing", bucket: []any{"", "0.1", 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, ok := parseQuotaPayload([]any{2, []any{tc.bucket}}, observed)
			if !ok || len(q.Windows) != 1 {
				t.Fatalf("parseQuotaPayload() = %#v, %v", q, ok)
			}
			w := q.Windows[0]
			if w.HasReset != tc.wantReset || (tc.wantReset && !w.ResetAt.Equal(tc.want)) {
				t.Fatalf("reset = %v (has=%v), want %v (has=%v)", w.ResetAt, w.HasReset, tc.want, tc.wantReset)
			}
		})
	}
}

func TestParseQuotaPayloadPlan(t *testing.T) {
	cases := []struct {
		code any
		plan string
		raw  string
	}{
		{code: 1, plan: "FREE", raw: "1"},
		{code: 4, plan: "PLUS", raw: "4"},
		{code: 2, plan: "PRO", raw: "2"},
		{code: 3, plan: "ULTRA", raw: "3"},
		{code: 6, plan: "ULTRA", raw: "6"},
		{code: 9, plan: "", raw: "9"},
		{code: nil, plan: "", raw: ""},
	}
	for _, tc := range cases {
		q, ok := parseQuotaPayload([]any{tc.code, []any{}}, time.Now())
		if !ok || q.Plan != tc.plan || q.PlanCode != tc.raw {
			t.Fatalf("code %v: plan=%q raw=%q ok=%v, want %q/%q", tc.code, q.Plan, q.PlanCode, ok, tc.plan, tc.raw)
		}
	}
}
