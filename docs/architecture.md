# Architecture

## Containers

```mermaid
flowchart LR
  subgraph Providers
    EHR[EHR / provider portal]
    UM[UM nurse / clinician UI]
  end

  subgraph priorauth-engine
    API[HTTP API<br/>net/http, OpenAPI 3.1]
    APP[Application service<br/>use cases, metrics, tracing]
    subgraph Domain
      RULES[rules<br/>JSON DSL, 3-valued evaluator,<br/>explain tree, hot reload]
      EVID[evidence<br/>facts, FHIR extractor,<br/>corroboration]
      WF[workflow<br/>event-sourced aggregate,<br/>guards, SLA timers, exceptions]
      PAS[pas<br/>Claim bundle / ClaimResponse]
    end
    LOOPS[Background loops<br/>SLA ticker · payer poller · rules watcher]
  end

  subgraph Adapters
    REPO[(Repository<br/>Postgres events + views<br/>or in-memory)]
    KAFKA[[Kafka<br/>priorauth.events]]
    PAYER[Payer gateway<br/>HTTP PAS or in-process sim]
    LLM[LLM extractor<br/>OpenAI-compatible, notes only]
  end

  OBS[Prometheus · OTLP traces · slog JSON]

  EHR -->|POST /v1/requests| API
  UM -->|/v1/queue/exceptions · /review| API
  API --> APP
  APP --> RULES & EVID & WF & PAS
  APP --> REPO
  APP -.after commit.-> KAFKA
  APP --> PAYER
  EVID -.proposals.-> LLM
  LOOPS --> APP
  APP --> OBS
```

Rules of the shape:

- `internal/domain/*` is pure: no I/O, deterministic, table-tested, ≥ 85% covered.
- `internal/app` is the only package that touches both domain and ports.
- `internal/adapters/*` implement `internal/ports`; every port has an in-memory adapter, so
  `make run` and the whole test suite need no infrastructure.

## Hot path: create → determine → assemble → submit

```mermaid
sequenceDiagram
  autonumber
  participant P as Provider
  participant API as HTTP API
  participant S as app.Service
  participant R as rules.Store
  participant X as Extractors
  participant W as workflow.Request
  participant DB as Repository
  participant K as Kafka
  participant PY as Payer

  P->>API: POST /v1/requests (Intake, Idempotency-Key)
  API->>S: Create
  S->>DB: Reserve(key) → new id
  S->>W: Create → request.created
  S->>DB: Save(events + projection)  [one tx]
  S-->>K: Publish (best effort)
  API-->>P: 201 Request{state: draft}

  P->>API: POST /v1/requests/{id}/determine
  S->>DB: Load(stream)
  S->>X: FHIRExtractor.Extract(bundle)
  S->>R: Current().Determine(payer, plan, CPT, ICDs, facts)
  R-->>S: Determination{required, criteria, explain, rule@v, set hash}
  S->>W: RecordDetermination → determination.recorded (+ exception if indeterminate)
  S->>DB: Save
  API-->>P: 200 Request{state: determined}

  P->>API: POST /v1/requests/{id}/assemble
  S->>X: FHIRExtractor + LLM proposer (notes only)
  S->>W: BuildPackage: corroborate, apply reviews, re-evaluate, documentation checklist
  S->>W: RecordEvidence → evidence.assembled (+ exceptions: criteria / proposals / docs)
  S->>DB: Save
  API-->>P: 200 Request{state: assembled, evidence.complete}

  P->>API: POST /v1/requests/{id}/submit
  S->>W: CanSubmit (complete package or appeal)
  S->>PY: POST /Claim/$submit (PAS Bundle)
  PY-->>S: ClaimResponse
  S->>W: MarkSubmitted → submission.sent
  S->>W: RecordPayerDecision → payer.responded (approved | denied | pended)
  S->>DB: Save (both events, one tx)
  API-->>P: 200 Request{state: approved | denied | pended}
```

## State machine

```mermaid
stateDiagram-v2
  [*] --> draft: request.created
  draft --> determined: determination.recorded
  determined --> determined: re-determine (rules reload)
  determined --> assembled: evidence.assembled (Required only)
  assembled --> assembled: re-assemble (review / new data)
  assembled --> submitted: submission.sent
  submitted --> pended: payer.responded(pended)
  submitted --> approved: payer.responded(approved)
  submitted --> denied: payer.responded(denied)
  pended --> approved: poll / sync
  pended --> denied: poll / sync
  denied --> appealed: review(appeal)
  appealed --> appealed: submission.sent(appeal) · pended
  appealed --> approved: payer.responded(approved)
  appealed --> denied: payer.responded(denied)
  draft --> closed: review(withdraw)
  determined --> closed: review(withdraw)
  assembled --> closed: review(withdraw)
  submitted --> closed: review(withdraw)
  pended --> closed: review(withdraw)
  denied --> closed: review(withdraw)
  appealed --> closed: review(withdraw)
  approved --> [*]
  closed --> [*]
```

Guards live in `internal/domain/workflow/request.go` (`RecordDetermination`,
`CanAssemble`, `CanSubmit`, `RecordPayerDecision`, `Review`). Exceptions are
raised/resolved by the same commands so the queue always mirrors the state.

## Data

| Table | Purpose |
| --- | --- |
| `request_events` | append-only stream, `UNIQUE (request_id, seq)` is the concurrency guard |
| `request_views` | JSONB projection of `workflow.Request`, indexed by state/payer/updated_at |
| `idempotency_keys` | key → request id |
| `schema_migrations` | applied embedded migrations |

## Scaling model

Stateless replicas behind a Service; the HPA scales on CPU/memory. Writes to
one request are serialised by optimistic concurrency, so many replicas can
run the SLA and poll loops concurrently (a loser gets `ErrConcurrency` and
skips). Postgres is the write bottleneck; at ~50 events per request and a
few KB per event, 10k requests/day is well under a single instance.
