# ADR 0002: Coverage rules as versioned JSON with three-valued evaluation

- Status: Accepted
- Date: 2026-09-21

## Context

Payer medical policies are prose documents that change several times a
year. Encoding them in Go means a deploy per policy change and no way for a
clinical policy team to review what the engine actually enforces. Encoding
them in a general-purpose expression language (CEL, Rego, a scripting VM)
gives flexibility nobody asked for and makes explainability harder.

Clinical data is also incomplete far more often than it is contradictory:
"no HbA1c on file" is a different situation from "HbA1c is 6.1%", and the
right action differs (chase the lab vs. deny).

## Decision

- Rules are **JSON documents** (`rules/*.json`) with a closed predicate
  vocabulary: `all` / `any` / `not` / `at_least` combinators over leaf facts
  `age`, `sex`, `diagnosis`, `medication`, `procedure`, `observation`, each
  with code patterns, status filters, recency windows, counts, durations and
  unit-aware numeric comparison. Unknown fields fail validation at load time.
- Evaluation is **three-valued**: `met`, `not_met`, `unknown`. `unknown` means
  the data to decide is absent; it propagates through combinators with
  Kleene semantics and surfaces as `missing_data` the reviewer can act on.
- Every evaluation produces an **explain tree** with the fact IDs that decided
  each node. The tree is stored in the event stream and shown to reviewers
  and payers.
- Rules carry `id` + `version`; each determination records `id@version` and
  a content hash of the whole active set. A rule is never edited in place
  once it has produced decisions; the version is bumped.
- Selection is deterministic by specificity: exact plan > diagnosis scope >
  exact service code > exact payer, then highest version, then lexical id.
- Rule files are **hot-reloaded** by a fingerprint poller; an invalid set is
  rejected and the previous one stays active.
- Unit conversion is an explicit table (mass, length, time, glucose, HbA1c,
  LDL). Anything not in the table evaluates to `unknown`, never a guess.

## Alternatives considered

- **CEL / Rego**: powerful, but explain trees would have to be reconstructed
  from ASTs and non-engineers cannot review the rules. Rejected.
- **FHIR CQL**: the standards-correct answer for clinical logic and the
  Da Vinci DTR direction, but a CQL engine is a project in itself. The JSON
  format is deliberately small enough that a CQL→JSON compiler for this
  subset is feasible later.

## Consequences

- A policy analyst can read `rules/acme-glp1-semaglutide.json` and check it
  against the policy PDF.
- Determinism plus versioning gives reproducible decisions: same facts, same
  rule version, same output, forever.
- New predicate types need code (and tests); that is the price of a closed
  vocabulary and it is the right trade.
