#!/usr/bin/env bash
# hack/assert-reconciliation.sh
#
# Polls until a sample AgentDeployment in the integration-test namespace
# reaches status.phase == Running, then exits 0.
# Exits non-zero (and fails the Jenkins stage) if the deadline is exceeded.
#
# Environment variables (all optional — defaults shown):
#   TEST_NS      Namespace to apply the sample manifest into (default: agentrax-jenkins-test)
#   TIMEOUT_SEC  Maximum seconds to wait for Running phase (default: 60)
#   KUBECTL      kubectl binary to use (default: kubectl)

set -euo pipefail

: "${TEST_NS:=agentrax-jenkins-test}"
: "${TIMEOUT_SEC:=60}"
: "${KUBECTL:=kubectl}"

SAMPLE_MANIFEST="$(dirname "$0")/testdata/sample-agentdeployment.yaml"
POLL_INTERVAL=3

# ---------------------------------------------------------------------------
# 1. Ensure the test namespace exists
# ---------------------------------------------------------------------------
echo "[assert-reconciliation] Ensuring namespace '${TEST_NS}' exists..."
${KUBECTL} create namespace "${TEST_NS}" --dry-run=client -o yaml | ${KUBECTL} apply -f -

# ---------------------------------------------------------------------------
# 2. Apply the sample AgentDeployment
# ---------------------------------------------------------------------------
if [[ ! -f "${SAMPLE_MANIFEST}" ]]; then
  echo "[assert-reconciliation] ERROR: sample manifest not found at ${SAMPLE_MANIFEST}" >&2
  exit 1
fi

echo "[assert-reconciliation] Applying sample AgentDeployment from ${SAMPLE_MANIFEST}..."
${KUBECTL} apply -f "${SAMPLE_MANIFEST}" -n "${TEST_NS}"

# ---------------------------------------------------------------------------
# 3. Poll until status.phase == Running
# ---------------------------------------------------------------------------
AD_NAME=$(${KUBECTL} get agentdeployment -n "${TEST_NS}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

if [[ -z "${AD_NAME}" ]]; then
  echo "[assert-reconciliation] ERROR: no AgentDeployment found in namespace '${TEST_NS}'" >&2
  exit 1
fi

echo "[assert-reconciliation] Waiting up to ${TIMEOUT_SEC}s for AgentDeployment '${AD_NAME}' to reach Running..."

elapsed=0
while true; do
  phase=$(${KUBECTL} get agentdeployment "${AD_NAME}" -n "${TEST_NS}" \
    -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")

  echo "[assert-reconciliation] t=${elapsed}s  status.phase=${phase}"

  if [[ "${phase}" == "Running" ]]; then
    echo "[assert-reconciliation] ✅ AgentDeployment '${AD_NAME}' reached Running in ${elapsed}s."
    exit 0
  fi

  if [[ "${phase}" == "RolloutFailed" || "${phase}" == "Degraded" ]]; then
    echo "[assert-reconciliation] ❌ Terminal failure phase '${phase}' detected." >&2
    ${KUBECTL} describe agentdeployment "${AD_NAME}" -n "${TEST_NS}" >&2
    exit 1
  fi

  if (( elapsed >= TIMEOUT_SEC )); then
    echo "[assert-reconciliation] ❌ Timeout after ${TIMEOUT_SEC}s — last phase: '${phase}'" >&2
    echo "[assert-reconciliation] --- describe output ---" >&2
    ${KUBECTL} describe agentdeployment "${AD_NAME}" -n "${TEST_NS}" >&2
    exit 1
  fi

  sleep "${POLL_INTERVAL}"
  elapsed=$(( elapsed + POLL_INTERVAL ))
done
