package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

var note = evidence.Note{ID: "n1", Type: "progress_note", Authored: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	Text: "Patient completed 8 sessions of physical therapy. HbA1c 8.2% on 2026-08-20. Started metformin in January."}

func TestParse(t *testing.T) {
	e := New("http://x", "", "m", time.Second)
	tests := []struct {
		name    string
		content string
		want    int
		check   func(*testing.T, evidence.Set)
	}{
		{"object wrapper", `{"facts":[{"kind":"procedure","code":"97110","snippet":"8 sessions of physical therapy","confidence":0.9}]}`, 1, func(t *testing.T, s evidence.Set) {
			require.Equal(t, evidence.OriginLLM, s[0].Prov.Origin)
			require.Equal(t, evidence.SystemCPT, s[0].System, "default system by kind")
			require.Equal(t, note.Authored, s[0].Effective, "falls back to note date")
			require.Equal(t, "llm:m@"+Version, s[0].Prov.Extractor)
			require.False(t, s[0].Usable())
		}},
		{"bare array + fences", "```json\n[{\"kind\":\"observation\",\"code\":\"4548-4\",\"value\":8.2,\"unit\":\"%\",\"effective\":\"2026-08-20\",\"snippet\":\"HbA1c 8.2%\",\"confidence\":0.95}]\n```", 1, func(t *testing.T, s evidence.Set) {
			require.Equal(t, 8.2, s[0].Value.Value)
			require.Equal(t, "%", s[0].Value.Unit)
			require.Equal(t, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), s[0].Effective)
		}},
		{"snippet not in note is dropped", `{"facts":[{"kind":"diagnosis","code":"C73","snippet":"thyroid cancer","confidence":0.99}]}`, 0, nil},
		{"low confidence dropped", `{"facts":[{"kind":"medication","code":"6809","snippet":"metformin","confidence":0.2}]}`, 0, nil},
		{"bad kind / demographic / empty code dropped", `{"facts":[{"kind":"vibes","code":"x","snippet":"metformin","confidence":1},{"kind":"demographic","code":"age","snippet":"metformin","confidence":1},{"kind":"medication","code":"","snippet":"metformin","confidence":1}]}`, 0, nil},
		{"case-insensitive snippet, value ignored for non-observation", `{"facts":[{"kind":"Medication","code":"6809","value":5,"snippet":"started METFORMIN","confidence":0.7,"effective":"2026-01"}]}`, 1, func(t *testing.T, s evidence.Set) {
			require.Nil(t, s[0].Value)
			require.Equal(t, evidence.SystemRxNorm, s[0].System)
			require.Equal(t, 2026, s[0].Effective.Year())
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.parse(tc.content, note)
			require.NoError(t, err)
			require.Len(t, got, tc.want)
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
	_, err := e.parse("not json", note)
	require.Error(t, err)

	e.MaxProposals = 1
	got, err := e.parse(`{"facts":[{"kind":"procedure","code":"97110","snippet":"physical therapy","confidence":0.9},{"kind":"medication","code":"6809","snippet":"metformin","confidence":0.9}]}`, note)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestExtractHTTP(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		switch r.Header.Get("X-Case") {
		case "error-json":
			_, _ = w.Write([]byte(`{"error":{"message":"quota"}}`))
		case "no-choices":
			_, _ = w.Write([]byte(`{"choices":[]}`))
		case "garbage":
			_, _ = w.Write([]byte(`{`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"facts\":[{\"kind\":\"procedure\",\"code\":\"97110\",\"snippet\":\"physical therapy\",\"confidence\":0.9}]}"}}]}`))
		}
	}))
	defer srv.Close()

	e := New(srv.URL, "k", "m", time.Second)
	e.MaxNoteChars = 40
	facts, err := e.Extract(context.Background(), evidence.Input{Notes: []evidence.Note{note}})
	require.NoError(t, err)
	require.Len(t, facts, 1)
	msgs := gotBody["messages"].([]any)
	user := msgs[1].(map[string]any)["content"].(string)
	require.Less(t, len(user), 120, "note truncated to MaxNoteChars")
	require.Equal(t, "m", gotBody["model"])

	// No notes → nothing, no call.
	facts, err = e.Extract(context.Background(), evidence.Input{})
	require.NoError(t, err)
	require.Nil(t, facts)

	for _, c := range []string{"error-json", "no-choices", "garbage"} {
		e.Client = &http.Client{Transport: headerTransport{c}}
		_, err := e.Extract(context.Background(), evidence.Input{Notes: []evidence.Note{note}})
		require.Error(t, err, c)
	}

	// Unreachable endpoint: error is per note, aggregated.
	dead := New("http://127.0.0.1:1", "", "m", 200*time.Millisecond)
	_, err = dead.Extract(context.Background(), evidence.Input{Notes: []evidence.Note{note, {ID: "n2", Text: "x"}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "note n1")
	require.Contains(t, err.Error(), "note n2")
}

type headerTransport struct{ c string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Case", h.c)
	return http.DefaultTransport.RoundTrip(r)
}
