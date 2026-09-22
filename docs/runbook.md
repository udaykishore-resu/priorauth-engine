# Runbook

## Service

`priorauth-engine` — HTTP on `:8080`, `/healthz`, `/readyz`, `/metrics`.
Binaries: `priorauth` (engine), `payer-sim` (simulated payer).

## SLOs

| SLI | Target | Measured by |
| --- | --- | --- |
| API availability (non-5xx) | 99.9 % / 30d | `priorauth_http_requests_total` |
| Determine latency p95 | < 250 ms | `priorauth_http_request_duration_seconds{route="/v1/requests/{id}/determine"}` |
| Submit latency p95 (excl. payer) | < 500 ms | same, `route="/v1/requests/{id}/submit"` minus `priorauth_payer_gateway_duration_seconds` |
| Auto-determination ratio | > 90 % | `/v1/metrics/turnaround` `auto_determined_ratio` |
| Exceptions older than review SLA | 0 | `priorauth_sla_breaches_total{timer="review"}` |
| Event publish success | 99.9 % | `priorauth_events_published_total` |

## Metrics

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `priorauth_http_requests_total` | counter | route, method, status | RED: rate + errors |
| `priorauth_http_request_duration_seconds` | histogram | route, method | RED: duration |
| `priorauth_http_in_flight_requests` | gauge | | saturation |
| `priorauth_determinations_total` | counter | decision, criteria, rule | which rules fire and how |
| `priorauth_submissions_total` | counter | result (sent, failed) | PAS submissions |
| `priorauth_payer_decisions_total` | counter | outcome | approved / denied / pended |
| `priorauth_state_transitions_total` | counter | to_state | workflow flow |
| `priorauth_exceptions_open` | gauge | code | queue depth per exception |
| `priorauth_exceptions_total` | counter | code | exceptions raised |
| `priorauth_sla_breaches_total` | counter | timer | SLA misses |
| `priorauth_turnaround_seconds` | histogram | payer, outcome | created → decided |
| `priorauth_llm_proposals_total` | counter | corroborated | LLM proposals and how many were corroborated |
| `priorauth_rules_loaded` / `priorauth_rule_reloads_total` | gauge / counter | | active rule count, hot reloads |
| `priorauth_payer_gateway_duration_seconds` | histogram | op, result | payer latency and errors |
| `priorauth_events_published_total` | counter | result | Kafka publishing |

## Alerts (suggested)

| Alert | Expression (5m) | Severity |
| --- | --- | --- |
| HighErrorRate | `sum(rate(priorauth_http_requests_total{status=~"5.."}[5m])) / sum(rate(priorauth_http_requests_total[5m])) > 0.01` | page |
| PayerDown | `rate(priorauth_payer_gateway_duration_seconds_count{result="error"}[5m]) > 0 and rate(...{result="ok"}[5m]) == 0` | page |
| PublishFailing | `rate(priorauth_events_published_total{result="error"}[5m]) > 0` | ticket |
| ReviewSLABreach | `increase(priorauth_sla_breaches_total{timer="review"}[1h]) > 0` | ticket (UM lead) |
| QueueGrowing | `sum(priorauth_exceptions_open) > 50` | ticket |
| RulesStale | `time() - priorauth_rule_reloads_total changes == 0` after a known policy release | ticket |
| NotReady | `up == 0` or readiness failing for 2m | page |

## Dashboards

1. **API RED** — request rate, error ratio, p50/p95 by route, in-flight.
2. **Workflow** — transitions per state, exceptions open by code, SLA breaches, turnaround p50/p95 per payer.
3. **Rules** — determinations by rule/decision/criteria, indeterminate ratio, reload count.
4. **Payer** — gateway latency, outcomes, pended backlog (`state=pended` count from `/v1/requests?state=pended`).

## Common failures

| Symptom | Likely cause | Action |
| --- | --- | --- |
| `/readyz` 503 `repository: fail` | Postgres unreachable / DSN wrong | check `PA_POSTGRES_DSN`, pgbouncer, network policy egress 5432 |
| `/readyz` 503 `payer_gateway: fail` | payer endpoint down | requests still accepted; `submit` returns 502 and records `submission_failed`; retry with `POST .../submit` after recovery |
| Many `indeterminate_rule` exceptions | new payer/plan/CPT without a rule | add a rule file (bump version), watcher reloads within `PA_RULES_RELOAD_INTERVAL`; humans use `set_determination` meanwhile |
| Log `rules reload rejected` | invalid JSON / schema in `rules/` | fix the file; previous set stays active, nothing is down |
| `409 concurrent_modification` | two writers on one request (e.g. poll + human) | client retries; expected under load |
| `evidence_review` piling up | LLM proposals waiting for humans | staff the queue or raise `MinConfidence`; the LLM is optional (`PA_LLM_BASE_URL=""`) |
| `payer_response_invalid` | payer returned a non-PAS response | inspect `decision.raw` on the request; `POST .../sync` after payer fix |
| `publish failed` errors | Kafka down | commands still succeed; consumers replay from `request_events` once Kafka is back |

## Operations

- **Rules change**: edit/add a file in the mounted `rules/` ConfigMap, bump `version`. Verify with `GET /v1/rules` (hash changes) and `priorauth_rule_reloads_total`.
- **Replay a projection**: `SELECT` the stream from `request_events`, call `workflow.Load`, upsert `request_views`. A `replay` CLI is on the roadmap; today it is a small script against the repository package.
- **Rollback**: `helm rollback <release> <rev>`. Event and view schemas are additive; a previous binary reads newer streams as long as no new event types were introduced (check `docs/adr/0001`).
- **Scaling**: HPA on CPU; the poll/SLA loops are safe on every replica (optimistic concurrency). Postgres connection pool is 8 per replica.
- **Drain**: `SIGTERM` → `/readyz` 503 → in-flight requests finish (≤ `PA_SHUTDOWN_TIMEOUT`) → exit 0.

## Local reproduction

```bash
make run                                   # engine on :8080 with in-memory adapters
./examples/demo.sh                         # full flow with realistic payloads
PA_LOG_LEVEL=debug PA_PAYER_SIM_MODE=deny make run   # exercise denial + appeal
```
