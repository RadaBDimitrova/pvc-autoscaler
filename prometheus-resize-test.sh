#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/hack/common.sh"

NAMESPACE="monitoring"
THRESHOLD_PERCENT=20
RESIZE_POLL_SEC=10
RESIZE_MAX_ATTEMPTS=30
RESIZE_TIMEOUT=300
PROM_DATA_DIR="/prometheus"
PROM_CONTAINER="prometheus"
FILL_CHUNK_MB=900
INITIAL_UPSCALE=false

usage() {
  echo "Usage: $0 [--initial-upscale]"
  echo ""
  echo "Modes:"
  echo "  (default)           Deploy with normal PVC, fill incrementally until threshold is crossed"
  echo "  --initial-upscale   Deploy with small PVCA threshold so that usage is already over threshold from the start"
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --initial-upscale) INITIAL_UPSCALE=true; shift ;;
    -h|--help) usage ;;
    *) _msg_error "Unknown flag: $1" 0; usage ;;
  esac
done

if [[ "${INITIAL_UPSCALE}" == "true" ]]; then
  _msg_info "Mode: initial-upscale — Prometheus PVC will start already over threshold"
else
  _msg_info "Mode: incremental-fill — Prometheus PVC will be filled incrementally until threshold is crossed"
fi

###############################################################################
# Step 1: Discover Prometheus StatefulSet, Pod, and PVC
###############################################################################
_msg_info "Discovering Prometheus resources in namespace ${NAMESPACE}"

PROM_STS=$(kubectl get sts -n "${NAMESPACE}" -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep -i prometheus | head -1 || true)
if [[ -z "${PROM_STS}" ]]; then
  _msg_error "No Prometheus StatefulSet found in namespace ${NAMESPACE}" 1
fi
_msg_info "Found StatefulSet: ${PROM_STS}"

PROM_POD="${PROM_STS}-0"
_msg_info "Using pod: ${PROM_POD}"

# Wait for pod to be ready
_msg_info "Waiting for pod ${PROM_POD} to be ready"
kubectl wait pod "${PROM_POD}" -n "${NAMESPACE}" --for=condition=Ready --timeout=120s

# Discover the PVC by naming convention
PROM_PVC=$(kubectl get pvc -n "${NAMESPACE}" -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep -i prometheus | head -1 || true)

if [[ -z "${PROM_PVC}" ]]; then
  _msg_error "No Prometheus PVC found in namespace ${NAMESPACE}" 1
fi
_msg_info "Found PVC: ${PROM_PVC}"

# Get current PVC capacity
INITIAL_CAPACITY=$(_pvc_capacity "${PROM_PVC}" "${NAMESPACE}")
_msg_info "Current PVC capacity: ${INITIAL_CAPACITY}"

# Parse capacity to bytes
CAPACITY_VALUE=$(echo "${INITIAL_CAPACITY}" | sed 's/[^0-9]//g')
CAPACITY_UNIT=$(echo "${INITIAL_CAPACITY}" | sed 's/[0-9]//g')

# Compute expected capacity after resize (current + max(10%, 1Gi)),
# matching the autoscaler's default StepPercent=10 and MinStepAbsolute=1Gi.
INCREASE=$(( (CAPACITY_VALUE + 9) / 10 ))
if [[ ${INCREASE} -lt 1 ]]; then
  INCREASE=1
fi
EXPECTED_CAPACITY="$(( CAPACITY_VALUE + INCREASE ))${CAPACITY_UNIT}"
_msg_info "Expected capacity after resize: ${EXPECTED_CAPACITY}"

###############################################################################
# Step 2: Create PersistentVolumeClaimAutoscaler for the Prometheus PVC
###############################################################################
PVCA_NAME="pvca-prometheus"
MAX_CAPACITY="60${CAPACITY_UNIT}"

if [[ "${INITIAL_UPSCALE}" == "true" ]]; then
  PVCA_THRESHOLD=2
else
  PVCA_THRESHOLD="${THRESHOLD_PERCENT}"
fi

_msg_info "Creating PVCAutoscaler ${PVCA_NAME} for PVC ${PROM_PVC} (maxCapacity: ${MAX_CAPACITY}, threshold: ${PVCA_THRESHOLD}%)"

kubectl apply -f - <<EOF
apiVersion: autoscaling.gardener.cloud/v1alpha1
kind: PersistentVolumeClaimAutoscaler
metadata:
  name: ${PVCA_NAME}
  namespace: ${NAMESPACE}
spec:
  targetRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: ${PROM_PVC}
  volumePolicies:
  - maxCapacity: ${MAX_CAPACITY}
    scaleUp:
      utilizationThresholdPercent: ${PVCA_THRESHOLD}
EOF

###############################################################################
# Step 3: Reach the threshold
###############################################################################
CHUNK=0
TOTAL_WRITTEN=0

USAGE_BEFORE=$(kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
  df "${PROM_DATA_DIR}" 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
_msg_info "Current usage: ${USAGE_BEFORE}%"

if [[ "${INITIAL_UPSCALE}" == "true" ]]; then
  # --initial-upscale: PVCA threshold is set to 5%, so normal usage should already exceed it
  if [[ "${USAGE_BEFORE}" -gt "${PVCA_THRESHOLD}" ]]; then
    FILL_DONE_TS=$(date +%s)
    _msg_info "Usage ${USAGE_BEFORE}% is already > ${PVCA_THRESHOLD}% threshold — waiting for autoscaler to resize"
  else
    _msg_info "Usage ${USAGE_BEFORE}% is below ${PVCA_THRESHOLD}% — filling to exceed threshold"
    FILL_TARGET="${PVCA_THRESHOLD}"
  fi
else
  _msg_info "Filling Prometheus volume in ${FILL_CHUNK_MB}MB increments until usage >= ${THRESHOLD_PERCENT}%"
  FILL_TARGET="${THRESHOLD_PERCENT}"
fi

if [[ -n "${FILL_TARGET:-}" ]]; then
  while true; do
    CHUNK=$((CHUNK + 1))
    FILL_FILE="${PROM_DATA_DIR}/fill_chunk${CHUNK}.dat"

    _msg_info "Chunk ${CHUNK}: Writing ${FILL_CHUNK_MB}MB to ${FILL_FILE}"
    kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
      dd if=/dev/zero of="${FILL_FILE}" bs=1M count="${FILL_CHUNK_MB}" 2>/dev/null

    TOTAL_WRITTEN=$((TOTAL_WRITTEN + FILL_CHUNK_MB))
    CURRENT_USAGE=$(kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
      df "${PROM_DATA_DIR}" 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
    _msg_info "Chunk ${CHUNK} done — wrote ${TOTAL_WRITTEN}MB total, usage: ${CURRENT_USAGE}%"

    if [[ "${CURRENT_USAGE}" -gt "${FILL_TARGET}" ]]; then
      FILL_DONE_TS=$(date +%s)
      _msg_info "Threshold reached: usage ${CURRENT_USAGE}% > ${FILL_TARGET}% after ${CHUNK} chunks (${TOTAL_WRITTEN}MB)"
      break
    fi
  done
fi

###############################################################################
# Step 4: Wait for PVC to be resized
###############################################################################
_msg_info "Waiting for PVC ${PROM_PVC} to be resized to ${EXPECTED_CAPACITY}"

for attempt in $(seq 1 "${RESIZE_MAX_ATTEMPTS}"); do
  got_capacity=$(_pvc_capacity "${PROM_PVC}" "${NAMESPACE}" 2>/dev/null || echo "unknown")

  _msg_info "[${attempt}/${RESIZE_MAX_ATTEMPTS}] PVC capacity: ${got_capacity}"

  if [[ "${got_capacity}" == "${EXPECTED_CAPACITY}" ]]; then
    RESIZE_TS=$(date +%s)
    DURATION=$(( RESIZE_TS - FILL_DONE_TS ))
    _msg_info "PVC resized to ${EXPECTED_CAPACITY} (took ${DURATION}s after fill completed)"
    break
  fi

  if [[ ${attempt} -eq ${RESIZE_MAX_ATTEMPTS} ]]; then
    _msg_error "PVC was NOT resized to ${EXPECTED_CAPACITY} (still at ${got_capacity})" 1
  fi

  sleep "${RESIZE_POLL_SEC}"
done

# Verify
_ensure_pvc_capacity "${PROM_PVC}" "${NAMESPACE}" "${EXPECTED_CAPACITY}"
_msg_info "PVC verified at ${EXPECTED_CAPACITY}"

###############################################################################
# Step 5: Wait for filesystem resize and verify usage dropped
###############################################################################
_msg_info "Waiting for filesystem to reflect the new size..."

FS_POLL_SEC=5
FS_MAX_ATTEMPTS=60

INITIAL_TOTAL_KB=$(kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
  df "${PROM_DATA_DIR}" 2>/dev/null | awk 'NR==2 {print $2}')

for attempt in $(seq 1 "${FS_MAX_ATTEMPTS}"); do
  CURRENT_TOTAL_KB=$(kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
    df "${PROM_DATA_DIR}" 2>/dev/null | awk 'NR==2 {print $2}')
  CURRENT_USAGE=$(kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
    df "${PROM_DATA_DIR}" 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')

  if [[ "${CURRENT_TOTAL_KB}" -gt "${INITIAL_TOTAL_KB}" ]]; then
    _msg_info "Filesystem resized: ${INITIAL_TOTAL_KB}KB -> ${CURRENT_TOTAL_KB}KB (usage: ${CURRENT_USAGE}%)"
    break
  fi

  if [[ ${attempt} -eq ${FS_MAX_ATTEMPTS} ]]; then
    _msg_info "Filesystem did not resize within timeout (still ${CURRENT_TOTAL_KB}KB)"
    break
  fi

  if (( attempt % 10 == 0 )); then
    _msg_info "[${attempt}/${FS_MAX_ATTEMPTS}] Filesystem still at ${CURRENT_TOTAL_KB}KB (usage: ${CURRENT_USAGE}%)"
  fi
  sleep "${FS_POLL_SEC}"
done

###############################################################################
# Step 6: Cleanup fill files
###############################################################################
if [[ ${CHUNK} -gt 0 ]]; then
  _msg_info "Removing ${CHUNK} fill files from Prometheus volume"
  for c in $(seq 1 "${CHUNK}"); do
    kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
      rm "${PROM_DATA_DIR}/fill_chunk${c}.dat" 2>/dev/null || true
  done
else
  _msg_info "No fill files to clean up (--initial-upscale mode)"
fi

FINAL_USAGE=$(kubectl exec "${PROM_POD}" -n "${NAMESPACE}" -c "${PROM_CONTAINER}" -- \
  df "${PROM_DATA_DIR}" 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
_msg_info "Usage after cleanup: ${FINAL_USAGE}%"

###############################################################################
# Summary
###############################################################################
echo ""
_msg_info "=== Summary ==="
_msg_info "  Mode:              $( [[ "${INITIAL_UPSCALE}" == "true" ]] && echo "initial-upscale" || echo "incremental-fill" )"
_msg_info "  Prometheus pod:    ${PROM_POD}"
_msg_info "  Prometheus PVC:    ${PROM_PVC}"
_msg_info "  Initial capacity:  ${INITIAL_CAPACITY}"
if [[ ${CHUNK} -gt 0 ]]; then
  _msg_info "  Data written:      ${TOTAL_WRITTEN}MB in ${CHUNK} chunks"
fi
_msg_info "  Resized to:        ${EXPECTED_CAPACITY}"
_msg_info "  Final usage:       ${FINAL_USAGE}%"
_msg_info "  PVCA:              ${PVCA_NAME} (maxCapacity: ${MAX_CAPACITY})"
