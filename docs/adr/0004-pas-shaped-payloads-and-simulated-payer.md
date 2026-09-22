# ADR 0004: Da Vinci PAS-shaped FHIR payloads, simulated payer, no X12 278

- Status: Accepted
- Date: 2026-09-21

## Context

CMS-0057-F pushes payers towards FHIR prior authorization APIs following the
Da Vinci CRD/DTR/PAS implementation guides. Today most payers still receive
X12 278 through a clearinghouse, which translates from FHIR. Building an X12
translator here would duplicate the clearinghouse and dwarf the rest of the
codebase.

## Decision

- Submission payloads are **PAS-shaped**: a `Bundle` (collection) with a
  `Claim` (`use: preauthorization`), `Patient`, `Coverage`, `Practitioner`
  and `Organization`, where `supportingInfo` references every usable fact
  and every satisfied documentation artefact, and an extension carries the
  rule reference and criteria outcome. Appeals set `Claim.related` to the
  prior claim and payer reference.
- Responses are interpreted from `ClaimResponse` with PAS `reviewAction`
  codes first (A1/A2/A6 approved, A3/C denied, A4/CT pended) and FHIR
  `outcome` as a fallback; anything else raises a `payer_response_invalid`
  exception rather than guessing.
- The payer is behind `ports.PayerGateway`. `cmd/payer-sim` is a real HTTP
  server (`POST /Claim/$submit`, `GET /ClaimResponse/{ref}`) with
  configurable modes (smart/approve/deny/pend/random), latency, error
  injection and pended→approved flips, and also runs in-process for
  `make run` and tests.
- **X12 278 is out of scope.** The gateway interface is where a clearinghouse
  adapter would plug in.
- The engine does not claim conformance to the PAS profiles; it uses their
  shapes so a conformant adapter is a mapping, not a redesign.

## Consequences

- End-to-end demos and tests exercise pend, approve, deny, appeal and
  transport failure paths with zero external dependencies.
- Real payer integration means implementing one interface (and probably
  OAuth), not touching the domain.
