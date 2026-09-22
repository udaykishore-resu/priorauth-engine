package evidence

import (
	"context"
	"time"
)

// Note is a free-text clinical document attached to a request.
type Note struct {
	ID       string    `json:"id"`
	Type     string    `json:"type,omitempty"` // e.g. progress_note, imaging_report
	Authored time.Time `json:"authored,omitempty"`
	Text     string    `json:"text"`
}

// Input is everything an extractor may look at.
type Input struct {
	// Bundle is a FHIR R4 Bundle (JSON) containing Patient, Condition,
	// Observation, MedicationStatement, Procedure resources. May be nil.
	Bundle []byte
	// BirthDate lets the demographic extractor compute age even when the
	// bundle has no Patient resource.
	BirthDate time.Time
	Sex       string
	// Notes are free-text documents. Only the LLM extractor reads them.
	Notes []Note
	// AsOf is the reference time for age and recency computations.
	AsOf time.Time
}

// Extractor produces facts from an input. Implementations must be safe for
// concurrent use and must set Provenance.Origin and Provenance.Extractor on
// every fact they emit.
type Extractor interface {
	Name() string
	Extract(ctx context.Context, in Input) (Set, error)
}
