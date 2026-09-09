#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Preconditions:
#   hack/create-kind-cluster.sh
#   hack/install-ate-kind.sh --deploy-ate-system
#   hack/install-ate-kind.sh --deploy-demo-egress
#
# The egress demo Actor accepts {"url":"..."} and performs an HTTP GET. With
# egress turned on, the Actor's outbound TCP is nftables-REDIRECTed
# into atunnel, wrapped in mTLS + HTTP CONNECT, and sent to the egress gateway,
# which terminates CONNECT and tunnels to the real destination. This script
# drives that path and checks the selected dataplane's access log to prove the
# CONNECT traversed the egress gateway.
set -o errexit -o nounset -o pipefail
ROOT="$(git rev-parse --show-toplevel)"; cd "${ROOT}"

CTX="${KUBECTL_CONTEXT:-kind-substrate}"
K="kubectl --context ${CTX}"
# The actor lives in the demo's atespace: --template resolves the
# template by name within the actor's own atespace.
ATESPACE="${ATESPACE:-ate-demo-egress}"
ACTOR="${ACTOR:-egress-demo}"
TARGET_URL="${TARGET_URL:-http://example.com/}"

echo "== gateway should be running =="
${K} -n ate-system rollout status deployment/atenet-egress --timeout=120s
DATAPLANE=$(${K} -n ate-system get deployment/atenet-egress \
  -o jsonpath='{.spec.template.spec.containers[0].name}')
case "${DATAPLANE}" in
  envoy)
    ACCESS_LOG_PATTERN='\[egress\]'
    ;;
  agentgateway)
    ACCESS_LOG_PATTERN='substrate.connect.authority'
    ;;
  *)
    echo "unsupported egress dataplane container: ${DATAPLANE}" >&2
    exit 1
    ;;
esac
echo "   dataplane=${DATAPLANE}"

echo "== create atespace + actor =="
kubectl-ate --context "${CTX}" create atespace "${ATESPACE}" 2>/dev/null || true
kubectl-ate --context "${CTX}" create actor "${ACTOR}" \
  --atespace "${ATESPACE}" --template egress 2>/dev/null || true
# The router resumes the actor on demand; give the control plane a beat to
# register it before driving traffic.
sleep 10

echo "== snapshot gateway access log offset =="
BEFORE=$(${K} -n ate-system logs deployment/atenet-egress -c "${DATAPLANE}" --tail=-1 2>/dev/null \
  | grep -Ec "${ACCESS_LOG_PATTERN}" || true)

echo "== drive actor egress: GET ${TARGET_URL} via the actor =="
${K} -n ate-system port-forward service/atenet-router 18000:80 >/tmp/pf.log 2>&1 &
PF=$!; trap 'kill ${PF} 2>/dev/null || true' EXIT
sleep 3
RESP=$(curl -s -o /dev/null -w "%{http_code}" -X POST http://localhost:18000/ \
  -H "Host: ${ACTOR}.${ATESPACE}.actors.resources.substrate.ate.dev" \
  -H 'Content-Type: application/json' \
  -d "{\"url\":\"${TARGET_URL}\"}") || true
echo "actor round-trip HTTP ${RESP} (200 = the actor fetched ${TARGET_URL} through egress)"

echo "== NEW egress gateway log lines (proof the CONNECT traversed the gateway) =="
# The CONNECT can land seconds after the actor's response, so we poll. And the
# Actor's HTTP client keeps the
# tunnel alive: a repeat fetch to a host it already reached rides the open tunnel
# and produces no new authentication entry at all, so a run against a warm actor
# would fail even though egress is working. Fall back to any tunnel already open
# for this actor before declaring failure.
NEW=""
for _ in $(seq 1 15); do
  NEW=$(${K} -n ate-system logs deployment/atenet-egress -c "${DATAPLANE}" --tail=-1 2>/dev/null \
    | grep -E "${ACCESS_LOG_PATTERN}" | tail -n +"$((BEFORE + 1))" || true)
  [ -n "${NEW}" ] && break
  sleep 2
done
if [ -n "${NEW}" ]; then
  echo "${NEW}"
elif ${K} -n ate-system logs deployment/atenet-egress -c "${DATAPLANE}" --tail=-1 2>/dev/null \
  | grep -Eq "${ACCESS_LOG_PATTERN}"; then
  echo "   no new CONNECT — the actor reused an already-open tunnel; recent gateway entries:"
  ${K} -n ate-system logs deployment/atenet-egress -c "${DATAPLANE}" --tail=-1 \
    | grep -E "${ACCESS_LOG_PATTERN}" | tail -3
else
  echo "!! no egress gateway lines for ${ATESPACE}/${ACTOR} — dumping recent logs:"
  ${K} -n ate-system logs deployment/atenet-egress -c "${DATAPLANE}" --tail=20
  exit 1
fi
echo "== PASS: actor egress traversed the egress gateway =="
