#!/usr/bin/env bash
# End-to-end demo against a running engine (default http://localhost:8080).
# Requires curl and jq. Start the engine first:  make run
set -euo pipefail
BASE_URL="${BASE_URL:-http://localhost:8080}"
DIR="$(cd "$(dirname "$0")" && pwd)"

step() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
post() { curl -sS -X POST "$BASE_URL$1" -H 'Content-Type: application/json' "${@:2}"; }

step "readiness"
curl -sS "$BASE_URL/readyz" | jq -c .

step "1. MRI lumbar spine: create → determine → assemble → submit (approved)"
MRI=$(post /v1/requests --data @"$DIR/01-mri-lumbar-spine.json" | jq -r .id)
echo "request id: $MRI"
post "/v1/requests/$MRI/determine" | jq '{state, decision: .determination.decision, criteria: .determination.criteria, rule: .determination.rule_ref, missing: .determination.missing_data}'
post "/v1/requests/$MRI/assemble"  | jq '{state, complete: .evidence.complete, facts: (.evidence.facts|length), docs: [.evidence.documentation[] | {code, satisfied}]}'
post "/v1/requests/$MRI/submit"    | jq '{state, payer_ref: .submission.payer_ref, auth: .decision.auth_number}'

step "2. GLP-1 (semaglutide): explain tree for the T2DM pathway"
GLP=$(post /v1/requests --data @"$DIR/02-glp1-semaglutide.json" | jq -r .id)
post "/v1/requests/$GLP/determine" | jq '.determination.explain | {description, outcome, children: [.children[] | {description, outcome, reason: (.reason // "")}]}'

step "3. Physical therapy: thin evidence → payer pends → sync flips to approved"
PT=$(post /v1/requests --data @"$DIR/03-physical-therapy.json" | jq -r .id)
post "/v1/requests/$PT/determine" > /dev/null
post "/v1/requests/$PT/assemble"  > /dev/null
post "/v1/requests/$PT/submit"    | jq '{state, payer_ref: .submission.payer_ref}'
post "/v1/requests/$PT/sync"      | jq '{state, auth: .decision.auth_number}'

step "4. Urgent MRI with unknown criteria → exceptions queue → clinician override → submit"
REV=$(post /v1/requests --data @"$DIR/04-mri-needs-review.json" | jq -r .id)
post "/v1/requests/$REV/determine" | jq '{state, criteria: .determination.criteria, missing: .determination.missing_data}'
post "/v1/requests/$REV/assemble"  | jq '{state, complete: .evidence.complete, exceptions: [.exceptions[]?.code]}'
curl -sS "$BASE_URL/v1/queue/exceptions" | jq '.items[] | {request_id, urgent, code: .exception.code, severity: .exception.severity, suggested_action}'
post "/v1/requests/$REV/review" --data @"$DIR/review-override.json" | jq '{state, exceptions: [.exceptions[]?.code], override_by: .override.actor}'
post "/v1/requests/$REV/assemble"  | jq '{state, complete: .evidence.complete}'
post "/v1/requests/$REV/submit"    | jq '{state, payer_ref: .submission.payer_ref}'

step "5. Idempotent replay of request 1 returns the same id"
curl -sS -i -X POST "$BASE_URL/v1/requests" -H 'Content-Type: application/json' --data @"$DIR/01-mri-lumbar-spine.json" | grep -Ei '^(HTTP|Idempotent-Replay)'

step "6. Audit trail and turnaround"
curl -sS "$BASE_URL/v1/requests/$MRI/events" | jq '[.events[] | {seq, type, actor}]'
curl -sS "$BASE_URL/v1/metrics/turnaround" | jq '{total, in_flight, open_exceptions, by_state, auto_determined_ratio, payer_outcomes}'
