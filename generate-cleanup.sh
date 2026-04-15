#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="pvc-autoscaler-system"
TOTAL=200

echo "=== Deleting ${TOTAL} StatefulSets + PVCAutoscalers ==="

for i in $(seq 1 "${TOTAL}"); do
  kubectl delete sts "demo-sts-${i}" -n "${NAMESPACE}" --ignore-not-found &
  kubectl delete pvca "pvca-sts-${i}" -n "${NAMESPACE}" --ignore-not-found &
  if (( i % 20 == 0 )); then
    wait
    echo "  ... deleted ${i}/${TOTAL}"
  fi
done
wait

echo "=== Deleting leftover PVCs ==="
kubectl delete pvc -n "${NAMESPACE}" --all --ignore-not-found

echo "=== Cleanup complete ==="