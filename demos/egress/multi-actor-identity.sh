#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Many actors, one worker, one identity each.
#
# Substrate multiplexes a large set of actors onto a small set of ready worker
# pods. This script makes that visible at the egress gateway: it squeezes the
# demo's WorkerPool down to a single worker, creates several actors on it, and
# has each one fetch the same URL in turn. Every fetch leaves the worker over
# the same tunnel machinery, but carries its own actor certificate, so the
# gateway's access log names a different actor each time even though one pod
# served them all.
#
# A worker hosts exactly one actor at a time (internal/ateomcapacity:
# actorsPerAteom = 1), and nothing suspends an idle actor yet, so the script
# suspends each actor after its turn to free the worker for the next one. That
# suspend is what the router would otherwise park a request waiting for.
#
# Prerequisites: a substrate cluster installed with
# --atenet-dataplane=agentgateway and --deploy-demo-egress, plus kubectl and
# kubectl-ate on PATH. See demos/egress/README.md.
#
# Usage:
#   demos/egress/multi-actor-identity.sh              # run the demo
#   demos/egress/multi-actor-identity.sh --cleanup    # remove what it created
#   ACTORS="a b c" demos/egress/multi-actor-identity.sh

set -o errexit -o nounset -o pipefail

CTX="${KUBECTL_CONTEXT:-kind-substrate}"
ATESPACE="${ATESPACE:-ate-demo-egress}"
TEMPLATE="${TEMPLATE:-egress}"
POOL_NS="${POOL_NS:-ate-demo-egress}"
POOL="${POOL:-egress}"
ACTORS="${ACTORS:-alpha bravo charlie delta echo}"
# As an array, so nothing downstream has to re-split it.
read -r -a ACTOR_LIST <<<"${ACTORS}"
TARGET_NS="${TARGET_NS:-egress-target}"
TARGET_PORT="${TARGET_PORT:-80}"
ROUTER_PORT="${ROUTER_PORT:-18100}"

K="kubectl --context ${CTX}"
KATE="kubectl-ate --context ${CTX}"
# Array form, for the calls that pass the command to retry as arguments.
KATE_CMD=(kubectl-ate --context "${CTX}")

log()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
pass() { printf '\033[1;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0

require() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1"; exit 1; }; }

# Every kubectl-ate invocation opens its own port-forward to ateapi, which can
# time out on a loaded machine. Retry rather than abandoning the run.
retry() {
  local attempt
  for attempt in 1 2 3 4 5; do
    "$@" && return 0
    sleep $((attempt * 2))
  done
  echo "giving up after 5 attempts: $*" >&2
  return 1
}

cleanup() {
  log "cleanup"
  for actor in "${ACTOR_LIST[@]}"; do
    ${KATE} suspend actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
    ${KATE} delete actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
  done
  # Wait it out: a namespace still terminating silently swallows the recreate
  # on the next run, and the actors then fetch a Service with no endpoints.
  ${K} delete namespace "${TARGET_NS}" --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
  info "the WorkerPool is left at its current replica count; scale it back with:"
  info "  kubectl --context ${CTX} -n ${POOL_NS} scale workerpool/${POOL} --replicas=2"
  info "done"
}

if [[ "${1:-}" == "--cleanup" ]]; then require kubectl; require kubectl-ate; cleanup; exit 0; fi

require kubectl
require kubectl-ate
require curl

##############################################################################
log "preflight: the egress gateway runs agentgateway"
##############################################################################
${K} -n ate-system rollout status deployment/atenet-egress --timeout=120s
DATAPLANE=$(${K} -n ate-system get deployment/atenet-egress \
  -o jsonpath='{.spec.template.spec.containers[0].name}')
info "egress dataplane = ${DATAPLANE}"
if [[ "${DATAPLANE}" != "agentgateway" ]]; then
  echo "This demo reads agentgateway's access log. Reinstall with" >&2
  echo "  hack/install-ate-kind.sh --deploy-ate-system --atenet-dataplane=agentgateway" >&2
  exit 1
fi

##############################################################################
log "one worker pod for every actor"
##############################################################################
${K} -n "${POOL_NS}" scale "workerpool/${POOL}" --replicas=1
# The pool reports readiness through its own status, not a Deployment's.
for _ in $(seq 1 60); do
  READY=$(${K} -n "${POOL_NS}" get "workerpool/${POOL}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
  [[ "${READY:-0}" == "1" ]] && break
  sleep 2
done
${K} -n "${POOL_NS}" get "workerpool/${POOL}"
# The control plane retires the scaled-down worker's record a little after the
# pod goes, so poll rather than reading the count once.
for _ in $(seq 1 30); do
  WORKERS=$(${KATE} get workers -n "${POOL_NS}" -o json | jq '.workers | length')
  [[ "${WORKERS}" == "1" ]] && break
  sleep 2
done
if [[ "${WORKERS}" == "1" ]]; then
  pass "the pool has exactly one worker"
else
  fail "expected 1 worker in ${POOL_NS}, found ${WORKERS}"
fi

##############################################################################
log "an in-cluster target every actor will fetch"
##############################################################################
# A namespace left terminating by an earlier run would swallow these creates.
for _ in $(seq 1 60); do
  PHASE=$(${K} get namespace "${TARGET_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "${PHASE}" != "Terminating" ]] && break
  sleep 2
done
${K} create namespace "${TARGET_NS}" >/dev/null 2>&1 || true
${K} -n "${TARGET_NS}" create deployment whoami --image=traefik/whoami >/dev/null 2>&1 || true
${K} -n "${TARGET_NS}" expose deployment whoami --port="${TARGET_PORT}" --target-port=80 >/dev/null 2>&1 || true
${K} -n "${TARGET_NS}" rollout status deployment/whoami --timeout=120s
TARGET_IP=$(${K} -n "${TARGET_NS}" get svc whoami -o jsonpath='{.spec.clusterIP}')
if [[ -z "${TARGET_IP}" ]]; then
  echo "the ${TARGET_NS}/whoami Service has no ClusterIP; is the namespace still terminating?" >&2
  exit 1
fi
# The Service must have an endpoint before any actor dials it, or every fetch
# comes back 503 "Connection refused" and looks like an egress failure.
for _ in $(seq 1 60); do
  ENDPOINTS=$(${K} -n "${TARGET_NS}" get endpoints whoami -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)
  [[ -n "${ENDPOINTS}" ]] && break
  sleep 2
done
[[ -n "${ENDPOINTS}" ]] || { echo "${TARGET_NS}/whoami has no endpoints" >&2; exit 1; }
GW_IP=$(${K} -n ate-system get pod -l app=atenet-egress -o jsonpath='{.items[0].status.podIP}')
info "target  = ${TARGET_IP}:${TARGET_PORT}"
info "gateway = ${GW_IP}"

##############################################################################
log "create the actors, each with its own egress policy"
##############################################################################
# The gateway denies by default, and the policy must exist before the actor's
# first outbound connection. The actors dial the target by address, so a
# hostname rule would not match at the CONNECT: --cidrs names the target's /32.
for actor in "${ACTOR_LIST[@]}"; do
  ${KATE} create actor "${actor}" -a "${ATESPACE}" --template "${TEMPLATE}" >/dev/null 2>&1 || true
  retry "${KATE_CMD[@]}" create egress-policy "${actor}" -a "${ATESPACE}" --cidrs "${TARGET_IP}/32" >/dev/null
  info "${ATESPACE}/${actor}: actor + egress policy allowing ${TARGET_IP}/32"
done
retry "${KATE_CMD[@]}" get actors -a "${ATESPACE}"

##############################################################################
log "drive one fetch per actor through the single worker"
##############################################################################
${K} -n ate-system port-forward service/atenet-router "${ROUTER_PORT}:80" >/tmp/multi-actor-pf.log 2>&1 &
PF=$!; trap 'kill ${PF} 2>/dev/null || true' EXIT
sleep 4

SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
SERVING_PODS=""
for actor in "${ACTOR_LIST[@]}"; do
  CODE=$(curl -s -o /tmp/multi-actor-body.txt -w '%{http_code}' -X POST "http://localhost:${ROUTER_PORT}/" \
    -H "ate-target-actor: ${ATESPACE}/${actor}" \
    -H 'Content-Type: application/json' \
    -d "{\"url\":\"http://${TARGET_IP}:${TARGET_PORT}/\"}" || true)
  # A single named resource prints as a bare object, not a list.
  POD=$(${KATE} get actors "${actor}" -a "${ATESPACE}" -o json 2>/dev/null \
    | jq -r '.status.workerAssignment.workerPod // "?"')
  # Not `[[ ... ]] && assign`: under errexit a false test would end the run.
  if [[ "${POD}" != "?" ]]; then
    SERVING_PODS="${SERVING_PODS}${POD}"$'\n'
  fi
  if [[ "${CODE}" == "200" ]]; then
    pass "${actor}: HTTP 200 through worker pod ${POD}"
  else
    fail "${actor}: HTTP ${CODE} (want 200); body: $(head -c 200 /tmp/multi-actor-body.txt)"
  fi
  if grep -q "RemoteAddr: ${GW_IP}" /tmp/multi-actor-body.txt 2>/dev/null; then
    info "  the target saw the gateway (${GW_IP}) as its client, not the actor"
  fi
  # Free the only worker for the next actor. Nothing does this automatically:
  # there is no auto-suspend-on-idle, so without it the next request parks and
  # then fails when the budget runs out.
  ${KATE} suspend actor "${actor}" -a "${ATESPACE}" >/dev/null 2>&1 || true
done
kill "${PF}" >/dev/null 2>&1 || true

##############################################################################
log "one worker pod served them all"
##############################################################################
${KATE} get actors -a "${ATESPACE}"
${KATE} get workers -n "${POOL_NS}"
# Counted while each actor was still resident: by now they are all suspended
# and hold no worker assignment at all.
PODS=$(printf '%s' "${SERVING_PODS}" | sed '/^$/d' | sort -u | wc -l | tr -d ' ')
info "distinct worker pods that served these actors: ${PODS}"
printf '%s' "${SERVING_PODS}" | sed '/^$/d' | sort -u | sed 's/^/     /'
if [[ "${PODS}" == "1" ]]; then
  pass "all ${#ACTOR_LIST[@]} actors were served by the same worker pod"
else
  fail "expected one serving worker pod, saw ${PODS}"
fi

##############################################################################
log "each fetch carried its own actor identity"
##############################################################################
# agentgateway's substrateEgress policy stamps the actor it authorized at
# CONNECT onto the access-log line for every request inside the tunnel.
LOG=$(${K} -n ate-system logs deployment/atenet-egress -c agentgateway --since-time="${SINCE}" 2>/dev/null || true)
printf '%s\n' "${LOG}" | grep -o 'ate\.actor\.name=[^ ]* ate\.actor\.uid=[^ ]* ate\.atespace=[^ ]*' | sort -u || true

SEEN=$(printf '%s\n' "${LOG}" | grep -o 'ate\.actor\.name=[^ ]*' | sed 's/.*=//' | sort -u)
MISSING=""
for actor in "${ACTOR_LIST[@]}"; do
  printf '%s\n' "${SEEN}" | grep -qx "${actor}" || MISSING="${MISSING} ${actor}"
done
if [[ -z "${MISSING}" ]]; then
  pass "the gateway named every actor: ${SEEN//$'\n'/ }"
else
  fail "no gateway log line for:${MISSING}"
  info "recent gateway lines, for diagnosis:"
  printf '%s\n' "${LOG}" | tail -20
fi

echo
if [[ "${FAILED}" == "0" ]]; then
  printf '\033[1;32mALL CHECKS PASSED\033[0m — %s actors, one worker, one identity each.\n' "${#ACTOR_LIST[@]}"
else
  printf '\033[1;31mSOME CHECKS FAILED\033[0m\n'
fi
info "re-run with --cleanup to remove the actors and the target"
exit "${FAILED}"
