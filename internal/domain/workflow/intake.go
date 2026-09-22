// Package workflow is the prior-authorization request aggregate: an
// event-sourced state machine
//
//	Draft → Determined(NotRequired|Required) → Assembled → Submitted →
//	Pended | Approved | Denied → Appealed → Approved | Denied
//
// with transition guards, SLA timers and an exceptions queue. The package is
// pure: it never performs I/O. Commands return events; Apply folds events
// into state; the application layer persists and projects them.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/udaykishore-resu/priorauth-engine/internal/domain/evidence"
)

// Intake is the provider-supplied request payload.
type Intake struct {
	// IdempotencyKey makes POST /v1/requests replay-safe. Optional; when
	// absent the server derives one from the content.
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
	Patient        Patient  `json:"patient"`
	Payer          Payer    `json:"payer"`
	Provider       Provider `json:"provider"`
	Service        Service  `json:"service"`
	// Bundle is a FHIR R4 Bundle with the structured clinical record.
	Bundle json.RawMessage `json:"fhir_bundle,omitempty"`
	// Notes are free-text documents (only the bounded LLM extractor reads them).
	Notes []evidence.Note `json:"notes,omitempty"`
	// Attachments are documentation artefacts matched against rule
	// documentation codes.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Patient identifies the member.
type Patient struct {
	ID        string    `json:"id"`
	MemberID  string    `json:"member_id"`
	Name      string    `json:"name,omitempty"`
	BirthDate time.Time `json:"birth_date"`
	Sex       string    `json:"sex,omitempty"`
}

// Payer identifies the payer and plan.
type Payer struct {
	ID   string `json:"id"`
	Plan string `json:"plan,omitempty"`
}

// Provider identifies the requesting provider.
type Provider struct {
	NPI  string `json:"npi"`
	Name string `json:"name,omitempty"`
}

// Service is the requested service or drug.
type Service struct {
	Code        string    `json:"code"`             // CPT/HCPCS/RxNorm
	System      string    `json:"system,omitempty"` // defaults to CPT
	Description string    `json:"description,omitempty"`
	Diagnoses   []string  `json:"diagnoses"` // ICD-10-CM
	Units       int       `json:"units,omitempty"`
	ServiceDate time.Time `json:"service_date"`
	Urgent      bool      `json:"urgent,omitempty"`
}

// Attachment is a documentation artefact reference.
type Attachment struct {
	Type  string `json:"type"` // matches rules.DocRequirement.Code
	Ref   string `json:"ref"`  // e.g. DocumentReference/123 or a URL
	Title string `json:"title,omitempty"`
}

// ErrValidation marks a client error.
var ErrValidation = errors.New("validation")

// Validate checks required fields and applies defaults.
func (in *Intake) Validate() error {
	var errs []string
	req := func(ok bool, msg string) {
		if !ok {
			errs = append(errs, msg)
		}
	}
	req(strings.TrimSpace(in.Patient.ID) != "", "patient.id is required")
	req(strings.TrimSpace(in.Patient.MemberID) != "", "patient.member_id is required")
	req(!in.Patient.BirthDate.IsZero(), "patient.birth_date is required")
	req(strings.TrimSpace(in.Payer.ID) != "", "payer.id is required")
	req(strings.TrimSpace(in.Provider.NPI) != "", "provider.npi is required")
	req(len(in.Provider.NPI) == 10 && strings.Trim(in.Provider.NPI, "0123456789") == "", "provider.npi must be 10 digits")
	req(strings.TrimSpace(in.Service.Code) != "", "service.code is required")
	req(len(in.Service.Diagnoses) > 0, "service.diagnoses must not be empty")
	req(!in.Service.ServiceDate.IsZero(), "service.service_date is required")
	for i, a := range in.Attachments {
		req(a.Type != "" && a.Ref != "", fmt.Sprintf("attachments[%d] needs type and ref", i))
	}
	for i, n := range in.Notes {
		req(strings.TrimSpace(n.Text) != "", fmt.Sprintf("notes[%d].text is empty", i))
	}
	if len(in.Bundle) > 0 && !json.Valid(in.Bundle) {
		errs = append(errs, "fhir_bundle is not valid JSON")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrValidation, strings.Join(errs, "; "))
	}
	if in.Payer.Plan == "" {
		in.Payer.Plan = "*"
	}
	if in.Service.System == "" {
		in.Service.System = evidence.SystemCPT
	}
	if in.Service.Units <= 0 {
		in.Service.Units = 1
	}
	return nil
}
