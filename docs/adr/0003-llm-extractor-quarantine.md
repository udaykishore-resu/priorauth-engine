# ADR 0003: LLM evidence extraction is a quarantined proposer, never a decider

- Status: Accepted
- Date: 2026-09-21

## Context

Much of the evidence a payer wants ("completed six weeks of PT", "new foot
drop") lives in free-text notes, not coded resources. An LLM is good at
pulling such facts out of prose and bad at being trusted with a coverage
decision. Regulators and payers are explicit that automated denials driven by
opaque models are unacceptable; the same applies to approvals.

## Decision

- `evidence.Extractor` is one interface with two kinds of implementation:
  the deterministic `FHIRExtractor` (structured resources) and the
  `llm.Extractor` (free text via any OpenAI-compatible endpoint).
- Facts carry `Provenance.Origin`. **Only `structured` and `human` facts are
  usable by the rule evaluator.** An `llm` fact becomes usable only when:
  1. a structured fact with the same kind and code exists
     (`evidence.Corroborate`), or
  2. a human confirms it by ID through `POST /review`
     (`confirm_evidence`).
  This is enforced in `evidence.Fact.Usable`, a single function the evaluator
  calls, not in prompts.
- The LLM adapter is bounded in code: it only receives `Input.Notes`
  (structured data never leaves the process); a proposal must quote a
  snippet that literally appears in the note or it is dropped; kinds/codes
  are validated; confidence below a threshold is dropped; proposal count and
  note length are capped; the prompt/post-processing version is recorded in
  every proposal's provenance.
- Unconfirmed proposals open an `evidence_review` exception, so a human is
  guaranteed to look. Rejected proposals are removed from the package.
- If the LLM endpoint is down the package is assembled without proposals and
  a warning is recorded; the LLM is an accelerator, never on the critical
  path.

## Consequences

- No determination can ever be traced to model output alone. The audit
  trail shows the snippet, the model/version and the reviewer who confirmed.
- Corroboration makes the LLM useful even without review when it merely
  restates structured data (it strengthens the package sent to the payer).
- Recall is bounded by human attention; that is intended.
