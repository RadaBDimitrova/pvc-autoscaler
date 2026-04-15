#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/hack/common.sh"

NAMESPACE="pvc-autoscaler-system"
TOTAL=200
FILL_COUNT=100
FILL_PERCENT=85
PVC_SIZE_BYTES=$((1 * 1024 * 1024 * 1024))  # 1Gi
FILL_BYTES=$(( PVC_SIZE_BYTES * FILL_PERCENT / 100 ))
FILL_MB=$(( FILL_BYTES / 1024 / 1024 ))
FILL_HALF_MB=$(( FILL_MB / 2 ))
PARALLELISM=20
RESIZE_POLL_SEC=10
RESIZE_MAX_ATTEMPTS=30
EXPECTED_CAPACITY="2Gi"
INITIAL_DELAY=10
THRESHOLD_PERCENT=80
RESIZE_TIMEOUT=300

_msg_info "Creating namespace ${NAMESPACE} if needed"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

###############################################################################
# Step 1: Generate and apply StatefulSets + PVCAs (targeting PVCs)
###############################################################################
_msg_info "Generating ${TOTAL} StatefulSets + PVCAutoscalers"

MANIFEST_FILE=$(mktemp)
trap "rm -f ${MANIFEST_FILE}" EXIT

# Build the in-pod script for filled pods (first FILL_COUNT)
# The pod will:
#   1. Wait INITIAL_DELAY seconds (so all pods start filling roughly together)
#   2. Write first half of data, log timestamp
#   3. Write second half of data, log timestamp
#   4. Log when usage crosses the threshold
#   5. Monitor for volume resize, log timestamp and compute duration
read -r -d '' FILL_SCRIPT <<'EOFSCRIPT' || true
#!/bin/sh
log() { echo "[$(date +%Y-%m-%dT%H:%M:%S)] $1"; }

INITIAL_DELAY=__INITIAL_DELAY__
FILL_HALF_MB=__FILL_HALF_MB__
THRESHOLD_PCT=__THRESHOLD_PERCENT__
RESIZE_TIMEOUT=__RESIZE_TIMEOUT__

get_usage_pct() {
  df /data | awk 'NR==2 {gsub(/%/,"",$5); print $5}'
}

get_total_kb() {
  df /data | awk 'NR==2 {print $2}'
}

get_avail_kb() {
  df /data | awk 'NR==2 {print $4}'
}

log "POD_STARTED"
log "Sleeping ${INITIAL_DELAY}s before filling..."
sleep "${INITIAL_DELAY}"

# --- Phase 1: Write first half ---
log "FILL_PHASE1_START: writing ${FILL_HALF_MB}MB to /data/fill_part1.dat"
dd if=/dev/zero of=/data/fill_part1.dat bs=1M count="${FILL_HALF_MB}" 2>/dev/null
log "FILL_PHASE1_DONE: wrote ${FILL_HALF_MB}MB"

# --- Phase 2: Write second half ---
log "FILL_PHASE2_START: writing ${FILL_HALF_MB}MB to /data/fill_part2.dat"
dd if=/dev/zero of=/data/fill_part2.dat bs=1M count="${FILL_HALF_MB}" 2>/dev/null
FILL_DONE_TS=$(date +%s)
log "FILL_PHASE2_DONE: wrote ${FILL_HALF_MB}MB (total: $((FILL_HALF_MB * 2))MB)"

# --- Phase 3: Check if threshold is crossed ---
USAGE_PCT=$(get_usage_pct)
TOTAL_KB=$(get_total_kb)
AVAIL_KB=$(get_avail_kb)
log "USAGE_AFTER_FILL: ${USAGE_PCT}% (total: ${TOTAL_KB}KB, avail: ${AVAIL_KB}KB)"

if [ "${USAGE_PCT}" -ge "${THRESHOLD_PCT}" ]; then
  THRESHOLD_TS=$(date +%s)
  log "THRESHOLD_REACHED: usage ${USAGE_PCT}% >= ${THRESHOLD_PCT}%"
else
  log "THRESHOLD_NOT_REACHED: usage ${USAGE_PCT}% < ${THRESHOLD_PCT}% (volume may already be resized from previous run)"
  THRESHOLD_TS=${FILL_DONE_TS}
fi

# --- Phase 4: Monitor for volume resize ---
INITIAL_SIZE=$(get_total_kb)
log "INITIAL_VOLUME_SIZE_KB: ${INITIAL_SIZE}"

RESIZED=0
ELAPSED=0
while [ "${ELAPSED}" -lt "${RESIZE_TIMEOUT}" ]; do
  sleep 5
  ELAPSED=$((ELAPSED + 5))
  CURRENT_SIZE=$(get_total_kb)
  if [ "${CURRENT_SIZE}" -gt "${INITIAL_SIZE}" ]; then
    RESIZE_TS=$(date +%s)
    DURATION_SINCE_FILL=$(( RESIZE_TS - FILL_DONE_TS ))
    DURATION_SINCE_THRESHOLD=$(( RESIZE_TS - THRESHOLD_TS ))
    CURRENT_USAGE=$(get_usage_pct)
    log "VOLUME_RESIZED: ${INITIAL_SIZE}KB -> ${CURRENT_SIZE}KB"
    log "USAGE_AFTER_RESIZE: ${CURRENT_USAGE}%"
    log "TIME_FILL_TO_RESIZE_SEC: ${DURATION_SINCE_FILL}"
    log "TIME_THRESHOLD_TO_RESIZE_SEC: ${DURATION_SINCE_THRESHOLD}"
    RESIZED=1
    break
  fi
done

if [ "${RESIZED}" -eq 0 ]; then
  log "RESIZE_NOT_DETECTED: volume stayed at ${INITIAL_SIZE}KB after ${RESIZE_TIMEOUT}s"
fi

log "MONITORING_COMPLETE: sleeping forever"
sleep infinity
EOFSCRIPT

# Substitute placeholders
FILL_SCRIPT="${FILL_SCRIPT//__INITIAL_DELAY__/${INITIAL_DELAY}}"
FILL_SCRIPT="${FILL_SCRIPT//__FILL_HALF_MB__/${FILL_HALF_MB}}"
FILL_SCRIPT="${FILL_SCRIPT//__THRESHOLD_PERCENT__/${THRESHOLD_PERCENT}}"
FILL_SCRIPT="${FILL_SCRIPT//__RESIZE_TIMEOUT__/${RESIZE_TIMEOUT}}"

# Idle script for non-filled pods
IDLE_SCRIPT='echo "[$(date +%Y-%m-%dT%H:%M:%S)] POD_STARTED (idle)"; sleep infinity'

for i in $(seq 1 "${TOTAL}"); do
  if [[ ${i} -le ${FILL_COUNT} ]]; then
    POD_SCRIPT="${FILL_SCRIPT}"
  else
    POD_SCRIPT="${IDLE_SCRIPT}"
  fi

  cat >> "${MANIFEST_FILE}" <<EOF
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: demo-sts-${i}
  namespace: ${NAMESPACE}
spec:
  serviceName: demo-sts-${i}
  replicas: 1
  selector:
    matchLabels:
      app: demo-sts-${i}
  template:
    metadata:
      labels:
        app: demo-sts-${i}
    spec:
      terminationGracePeriodSeconds: 0
      containers:
      - name: app
        image: busybox:1.37
        command: ["sh", "-c", $(printf '%s' "${POD_SCRIPT}" | jq -Rs .)]
        volumeMounts:
        - name: data
          mountPath: /data
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 1Gi
---
apiVersion: autoscaling.gardener.cloud/v1alpha1
kind: PersistentVolumeClaimAutoscaler
metadata:
  name: pvca-sts-${i}
  namespace: ${NAMESPACE}
spec:
  targetRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: data-demo-sts-${i}-0
  volumePolicies:
  - maxCapacity: 3Gi
EOF
done

_msg_info "Applying manifests (${TOTAL} StatefulSets + PVCAs)"
kubectl apply --server-side --force-conflicts -f "${MANIFEST_FILE}"

###############################################################################
# Step 2: Wait for FILL_COUNT StatefulSets to be ready
###############################################################################
_msg_info "Waiting for first ${FILL_COUNT} StatefulSets to be ready"

wait_for_sts() {
  local idx=$1
  local name="demo-sts-${idx}"
  local retries=0
  local max_retries=120
  while [[ ${retries} -lt ${max_retries} ]]; do
    ready=$(kubectl get sts "${name}" -n "${NAMESPACE}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [[ "${ready}" == "1" ]]; then
      return 0
    fi
    retries=$((retries + 1))
    sleep 2
  done
  _msg_error "${name} not ready after $((max_retries * 2))s" 0
  return 1
}

for i in $(seq 1 "${FILL_COUNT}"); do
  wait_for_sts "${i}" &
  if (( i % PARALLELISM == 0 )); then
    wait
  fi
done
wait
_msg_info "First ${FILL_COUNT} StatefulSets are ready"
_msg_info "Pods will begin filling in ~${INITIAL_DELAY}s (built-in delay)"

###############################################################################
# Step 3: Wait for PVCs to be resized
###############################################################################
_msg_info "Waiting for first ${FILL_COUNT} PVCs to be resized to ${EXPECTED_CAPACITY}"

for attempt in $(seq 1 "${RESIZE_MAX_ATTEMPTS}"); do
  resized_count=0
  pending_pvcs=()

  for i in $(seq 1 "${FILL_COUNT}"); do
    pvc_name="data-demo-sts-${i}-0"
    got_capacity=$(_pvc_capacity "${pvc_name}" "${NAMESPACE}" 2>/dev/null || echo "unknown")

    if [[ "${got_capacity}" == "${EXPECTED_CAPACITY}" ]]; then
      resized_count=$((resized_count + 1))
    else
      pending_pvcs+=("${pvc_name}(${got_capacity})")
    fi
  done

  _msg_info "[${attempt}/${RESIZE_MAX_ATTEMPTS}] Resized: ${resized_count}/${FILL_COUNT}"

  if [[ ${resized_count} -eq ${FILL_COUNT} ]]; then
    _msg_info "All ${FILL_COUNT} PVCs successfully resized to ${EXPECTED_CAPACITY}"
    break
  fi

  if [[ ${attempt} -eq ${RESIZE_MAX_ATTEMPTS} ]]; then
    failed_count=$(( FILL_COUNT - resized_count ))
    _msg_error "${failed_count} PVCs were NOT resized to ${EXPECTED_CAPACITY}" 0
    _msg_info "Sample pending PVCs: ${pending_pvcs[*]:0:10}"
    _msg_error "Timed out waiting for all PVCs to resize" 1
  fi

  sleep "${RESIZE_POLL_SEC}"
done

# Final assertion using _ensure_pvc_capacity
_msg_info "Verifying all ${FILL_COUNT} PVCs with _ensure_pvc_capacity"
for i in $(seq 1 "${FILL_COUNT}"); do
  _ensure_pvc_capacity "data-demo-sts-${i}-0" "${NAMESPACE}" "${EXPECTED_CAPACITY}"
done
_msg_info "All ${FILL_COUNT} PVCs verified at ${EXPECTED_CAPACITY}"

###############################################################################
# Step 4: Collect timing logs from pods
###############################################################################
_msg_info "Waiting for pods to detect the filesystem resize..."

POD_LOG_POLL_SEC=5
POD_LOG_MAX_ATTEMPTS=60  # 5 minutes max

for attempt in $(seq 1 "${POD_LOG_MAX_ATTEMPTS}"); do
  detected_count=0
  for i in $(seq 1 "${FILL_COUNT}"); do
    pod="demo-sts-${i}-0"
    if kubectl logs "${pod}" -n "${NAMESPACE}" -c app 2>/dev/null | grep -q "VOLUME_RESIZED\|RESIZE_NOT_DETECTED"; then
      detected_count=$((detected_count + 1))
    fi
  done

  if [[ ${detected_count} -eq ${FILL_COUNT} ]]; then
    _msg_info "All ${FILL_COUNT} pods have completed resize monitoring"
    break
  fi

  if [[ ${attempt} -eq ${POD_LOG_MAX_ATTEMPTS} ]]; then
    _msg_info "Timed out waiting for pod logs, ${detected_count}/${FILL_COUNT} pods detected resize. Collecting available data."
    break
  fi

  _msg_info "[${attempt}/${POD_LOG_MAX_ATTEMPTS}] Pods with resize detected: ${detected_count}/${FILL_COUNT}"
  sleep "${POD_LOG_POLL_SEC}"
done

_msg_info "Collecting timing data from pods"

echo ""
printf "%-20s %-15s %-15s %-20s\n" "POD" "THRESHOLD_HIT" "RESIZED" "THRESH->RESIZE(s)"
printf "%-20s %-15s %-15s %-20s\n" "----" "-------------" "-------" "-----------------"

for i in $(seq 1 "${FILL_COUNT}"); do
  pod="demo-sts-${i}-0"
  logs=$(kubectl logs "${pod}" -n "${NAMESPACE}" -c app 2>/dev/null || echo "")

  threshold_hit=$(echo "${logs}" | grep -c "THRESHOLD_REACHED" || true)
  resize_hit=$(echo "${logs}" | grep -c "VOLUME_RESIZED" || true)
  thresh_to_resize=$(echo "${logs}" | grep "TIME_THRESHOLD_TO_RESIZE_SEC" | awk '{print $NF}' || true)

  [[ -z "${thresh_to_resize}" ]] && thresh_to_resize="N/A"

  printf "%-20s %-15s %-15s %-20s\n" \
    "${pod}" \
    "$( [[ ${threshold_hit} -gt 0 ]] && echo "yes" || echo "no" )" \
    "$( [[ ${resize_hit} -gt 0 ]] && echo "yes" || echo "no" )" \
    "${thresh_to_resize:-N/A}"
done

echo ""
_msg_info "=== Summary ==="
_msg_info "  ${TOTAL} StatefulSets created"
_msg_info "  ${TOTAL} PVCAutoscalers created (targeting PVCs directly)"
_msg_info "  ${FILL_COUNT} PVCs filled to ~${FILL_PERCENT}%"
_msg_info "  ${resized_count}/${FILL_COUNT} PVCs resized to ${EXPECTED_CAPACITY}"
_msg_info ""
_msg_info "  View individual pod logs:"
_msg_info "    kubectl logs demo-sts-1-0 -n ${NAMESPACE}"