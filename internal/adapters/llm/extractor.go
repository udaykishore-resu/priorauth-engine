// Package llm is the bounded LLM evidence extractor. It calls any
// OpenAI-compatible chat-completions endpoint to propose clinical facts
// from free-text notes.
//
// Boundaries, enforced in code rather than by prompt alone:
//   - it only ever reads Input.Notes; structured data never goes to the model;
//   - every emitted fact is an evidence.OriginLLM proposal, which the rule
//     evaluator ignores until a human confirms it or structured data
//     corroborates it (see evidence.Fact.Usable);
//   - a proposal must quote a snippet that literally occurs in the source
//     note, otherwise it is dropped as unverifiable;
//   - kinds and codes are validated; free text never reaches the evaluator.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

// Version is bumped whenever the prompt or the post-processing changes,
// because it is recorded in every proposal's provenance.
const Version = "1"

// Extractor implements evidence.Extractor over HTTP.
type Extractor struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
	// MaxNoteChars truncates each note before sending (cost/PHI bound).
	MaxNoteChars int
	// MaxProposals caps the number of facts accepted per note.
	MaxProposals int
	// MinConfidence drops proposals the model itself rates below this.
	MinConfidence float64
}

// New returns an extractor with defaults.
func New(baseURL, apiKey, model string, timeout time.Duration) *Extractor {
	return &Extractor{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		APIKey:        apiKey,
		Model:         model,
		Client:        &http.Client{Timeout: timeout},
		MaxNoteChars:  12000,
		MaxProposals:  25,
		MinConfidence: 0.5,
	}
}

// Name implements evidence.Extractor.
func (e *Extractor) Name() string { return "llm:" + e.Model + "@" + Version }

const systemPrompt = `You extract discrete clinical facts from a clinical note for prior-authorization review.
Return ONLY a JSON object of the form {"facts":[...]} where each fact has:
  "kind": one of "diagnosis" (ICD-10-CM), "medication" (RxNorm), "procedure" (CPT/HCPCS), "observation" (LOINC)
  "code": the standard code for the kind
  "system": the coding system URL if known
  "display": short human-readable label
  "value": numeric value (observations only), "unit": unit string (observations only)
  "effective": ISO-8601 date when the fact was true/performed, if stated
  "status": e.g. active, completed
  "snippet": the EXACT substring of the note that supports the fact (verbatim, <= 200 chars)
  "confidence": 0..1
Only include facts explicitly supported by the note text. Never infer codes that are not clearly implied. Do not include demographics.`

type chatRequest struct {
	Model          string         `json:"model"`
	Temperature    float64        `json:"temperature"`
	ResponseFormat map[string]any `json:"response_format,omitempty"`
	Messages       []chatMessage  `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// proposal is the model's output schema.
type proposal struct {
	Kind       string   `json:"kind"`
	Code       string   `json:"code"`
	System     string   `json:"system"`
	Display    string   `json:"display"`
	Value      *float64 `json:"value"`
	Unit       string   `json:"unit"`
	Effective  string   `json:"effective"`
	Status     string   `json:"status"`
	Snippet    string   `json:"snippet"`
	Confidence float64  `json:"confidence"`
}

// Extract implements evidence.Extractor. It never returns structured facts
// and never fails the whole extraction because one note was unusable; per
// note errors are aggregated and returned alongside the facts it did get.
func (e *Extractor) Extract(ctx context.Context, in evidence.Input) (evidence.Set, error) {
	if len(in.Notes) == 0 {
		return nil, nil
	}
	var out evidence.Set
	var errs []error
	for _, note := range in.Notes {
		facts, err := e.extractNote(ctx, note)
		if err != nil {
			errs = append(errs, fmt.Errorf("note %s: %w", note.ID, err))
			continue
		}
		out = append(out, facts...)
	}
	return out.Sorted(), errors.Join(errs...)
}

func (e *Extractor) extractNote(ctx context.Context, note evidence.Note) (evidence.Set, error) {
	text := note.Text
	if e.MaxNoteChars > 0 && len(text) > e.MaxNoteChars {
		text = text[:e.MaxNoteChars]
	}
	body, err := json.Marshal(chatRequest{
		Model:          e.Model,
		Temperature:    0,
		ResponseFormat: map[string]any{"type": "json_object"},
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: "NOTE TYPE: " + note.Type + "\nNOTE DATE: " + note.Authored.Format("2006-01-02") + "\n---\n" + text},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: call: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("llm: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("llm: decode: %w", err)
	}
	if cr.Error != nil {
		return nil, fmt.Errorf("llm: api error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return nil, errors.New("llm: no choices")
	}
	return e.parse(cr.Choices[0].Message.Content, note)
}

// parse validates the model output and converts it to bounded proposals.
func (e *Extractor) parse(content string, note evidence.Note) (evidence.Set, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	var wrapper struct {
		Facts []proposal `json:"facts"`
	}
	if err := json.Unmarshal([]byte(content), &wrapper); err != nil {
		// Some models return a bare array.
		if err2 := json.Unmarshal([]byte(content), &wrapper.Facts); err2 != nil {
			return nil, fmt.Errorf("llm: output is not the expected JSON: %w", err)
		}
	}
	lower := strings.ToLower(note.Text)
	var out evidence.Set
	for i, p := range wrapper.Facts {
		if e.MaxProposals > 0 && len(out) >= e.MaxProposals {
			break
		}
		kind := evidence.Kind(strings.ToLower(strings.TrimSpace(p.Kind)))
		if !evidence.ValidKind(kind) || kind == evidence.KindDemographic {
			continue
		}
		code := strings.TrimSpace(p.Code)
		if code == "" || len(code) > 32 {
			continue
		}
		snippet := strings.TrimSpace(p.Snippet)
		if snippet == "" || !strings.Contains(lower, strings.ToLower(snippet)) {
			continue // unverifiable: the model must quote the note
		}
		if p.Confidence < e.MinConfidence {
			continue
		}
		f := evidence.Fact{
			ID:      fmt.Sprintf("llm:%s#%d", note.ID, i),
			Kind:    kind,
			System:  defaultSystem(kind, p.System),
			Code:    code,
			Display: truncate(p.Display, 120),
			Status:  strings.ToLower(strings.TrimSpace(p.Status)),
			Prov: evidence.Provenance{
				Origin:       evidence.OriginLLM,
				ResourceType: "Note",
				ResourceID:   note.ID,
				Extractor:    e.Name(),
				Snippet:      truncate(snippet, 200),
				Confidence:   p.Confidence,
			},
		}
		if p.Value != nil && kind == evidence.KindObservation {
			f.Value = &evidence.Quantity{Value: *p.Value, Unit: strings.TrimSpace(p.Unit)}
		}
		if p.Effective != "" {
			for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01"} {
				if t, err := time.Parse(layout, p.Effective); err == nil {
					f.Effective = t.UTC()
					break
				}
			}
		}
		if f.Effective.IsZero() && !note.Authored.IsZero() {
			f.Effective = note.Authored
		}
		out = append(out, f)
	}
	return out, nil
}

func defaultSystem(kind evidence.Kind, given string) string {
	if given != "" {
		return given
	}
	switch kind {
	case evidence.KindDiagnosis:
		return evidence.SystemICD10
	case evidence.KindMedication:
		return evidence.SystemRxNorm
	case evidence.KindProcedure:
		return evidence.SystemCPT
	case evidence.KindObservation:
		return evidence.SystemLOINC
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
