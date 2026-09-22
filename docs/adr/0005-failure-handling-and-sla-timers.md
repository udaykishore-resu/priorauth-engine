# ADR 0005: Failure handling, idempotency and SLA timers

- Status: Accepted
- Date: 2026-09-21

## Context

The engine sits between providers (who retry), payers (who time out) and
humans (who forget). It must be safe to replay, must not lose a request in a
crash, and must surface stalls rather than let them age silently.

## Decision

**Idempotency**
- `POST /v1/requests` reserves an idempotency key (`Idempotency-Key` header,
  body field, or a SHA-256 of the content) atomically before creating the
  stream; a replay returns the original request with `Idempotent-Replay: true`.
- Command endpoints are idempotent through guards: re-running `determine`
  in `determined` re-records; `submit` in a terminal state is `409`; a repeat
  "still pended" poll emits no event. Payer submission is keyed by Claim id
  so a retry after a transport error cannot double-submit.

**Persistence and publishing**
- Events and projection commit in one transaction (ADR 0001). Publishing to
  Kafka happens after commit and is best-effort: a failure is logged and
  counted (`priorauth_events_published_total{result="error"}`), the command
  still succeeds, and consumers recover from the event store via replay.
  The full outbox pattern is deliberately deferred (see Roadmap).

**Payer transport**
- The HTTP gateway retries 5xx/429/transport errors with bounded backoff
  and never retries 4xx. A failed `submit` records a `submission_failed`
  exception, keeps the request in `assembled`, and returns `502` with the
  persisted request so the client sees both.

**SLA timers**
- `workflow.SLAPolicy` derives deadlines from timeline fields
  (`determine`, `assemble`, `submit`, `response`, `pended`, `review`), scaled
  for urgent requests. A background loop (`PA_SLA_TICK`) breaches due timers
  **once** (the aggregate records `breached` timers) and opens an
  `sla_<timer>` exception. Approval or withdrawal clears SLA exceptions.
- Timers are computed, not stored: no scheduler state to lose, and a clock
  injection makes them unit-testable.

**Degradation**
- LLM extractor failure → package assembled with a warning (ADR 0003).
- Invalid rule reload → previous set stays active.
- `/readyz` fails when the repository or payer gateway ping fails and
  flips to 503 during drain so load balancers stop routing before the
  listener closes; `SIGTERM` waits up to `PA_SHUTDOWN_TIMEOUT` for in-flight
  requests.

## Consequences

- Nothing in the hot path depends on Kafka being up.
- Every stall becomes a queue item with an age and a suggested action.
- At-least-once event delivery downstream with a documented gap (outbox)
  rather than an undocumented one.
