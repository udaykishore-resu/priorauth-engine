# ADR 0001: Event-sourced request aggregate with a projected read model

- Status: Accepted
- Date: 2026-09-21

## Context

A prior authorization request lives for days to weeks, passes through human
and machine hands, and every step must be defensible to a payer, an auditor
or an appeals board: *which rule version said auth was required, which facts
supported the criteria, who confirmed the LLM proposal, when did the payer
pend it, why was the SLA breached*. A row-per-request table with status
columns loses that history the moment it is updated.

The workflow is also a real state machine with guards
(`Draft → Determined → Assembled → Submitted → Pended|Approved|Denied → Appealed`),
and guards are far easier to reason about when the state is rebuilt from the
facts that produced it.

## Decision

- The `workflow.Request` aggregate is **event-sourced**. Commands return
  events; `Apply` folds events into state and never fails; the stream in
  `request_events` is the source of truth.
- Every `Save` writes the new events **and** upserts a JSON projection
  (`request_views`) in the same transaction. The projection is the shape the
  API returns and what the exceptions queue and turnaround metrics read.
- Optimistic concurrency uses the stream length: `Save` asserts the expected
  version (`UNIQUE (request_id, seq)` in Postgres, length check in memory).
  A loser gets `409 concurrent_modification` and retries by reloading.
- Event type names (`request.created`, `payer.responded`, …) are a persisted
  contract and are never renamed; payloads are JSON so additive changes are
  safe.

## Consequences

- Complete audit trail for free; `GET /v1/requests/{id}/events` exposes it.
- The read model can be rebuilt from events at any time (replay), which is
  how a projection bug is fixed without data loss.
- Commands are slightly more verbose (load → command → emit → save) but each
  is a pure function that is trivially table-tested.
- Snapshots are not needed at current stream sizes (tens of events per
  request); if a request ever exceeds a few hundred events we add a snapshot
  every N events rather than change the model.
