# Security policy

## Scope

`priorauth-engine` processes protected health information (PHI). Treat every
deployment as in-scope for HIPAA technical safeguards.

## Reporting a vulnerability

Email security reports to the maintainer listed in `CODEOWNERS` via the
GitHub security advisory feature of this repository ("Report a vulnerability").
Please do not open public issues for security problems. You will receive an
acknowledgement within 3 business days and a remediation plan within 14.

## Design controls

- No PHI is written to logs: request/trace ids, state names, rule ids and
  counts only. The `Intake` payload is persisted in the event store and the
  read model; encrypt those at rest and in transit.
- The LLM extractor only ever sends free-text notes to the configured
  endpoint; structured FHIR data never leaves the process. Configure
  `PA_LLM_BASE_URL` only for endpoints covered by a BAA, or leave it empty.
- Every LLM proposal is quarantined until a human confirms it or structured
  data corroborates it (`evidence.Fact.Usable`).
- Containers run as non-root on distroless with a read-only root filesystem
  and all capabilities dropped (see the Helm chart).
- Dependencies are scanned with `govulncheck` in CI.

## Supported versions

Only the latest minor release receives security fixes.
