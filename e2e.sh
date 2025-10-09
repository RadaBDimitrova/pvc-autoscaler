#!/usr/bin/env bash

set -e

# Manually patch storage class to allow volume expansion
kubectl patch storageclass standard -p '{"allowVolumeExpansion": true}'

# Create fake metrics
kubectl apply -f fake-metrics/minimal-fake-metrics.yaml

# Patch the running pvc-autoscaler deployment to use fake metrics
echo "Patching pvc-autoscaler deployment to use fake metrics..."
kubectl patch deployment pvc-autoscaler-controller-manager -n pvc-autoscaler-system --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--metrics-available-bytes-query=kubelet_volume_stats_available_bytes{type=\"fake\"}"
  },
  {
    "op": "add", 
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--metrics-capacity-bytes-query=kubelet_volume_stats_capacity_bytes{type=\"fake\"}"
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-", 
    "value": "--metrics-available-inodes-query=kubelet_volume_stats_inodes_free{type=\"fake\"}"
  },
  {
    "op": "add",
    "path": "/spec/template/spec/containers/0/args/-",
    "value": "--metrics-capacity-inodes-query=kubelet_volume_stats_inodes{type=\"fake\"}"
  }
]'

# Wait for deployment to rollout
kubectl rollout status deployment/pvc-autoscaler-controller-manager -n pvc-autoscaler-system --timeout=60s 


# Wait for PVC autoscaler to process and resize the PVC
echo "Waiting for PVC autoscaler to resize vali-vali-0 PVC..."
sleep 120

# Check that vali-vali-0 PVC in garden namespace has been resized to 110Gi
echo "Checking PVC capacity..."
ACTUAL_CAPACITY=$(kubectl get pvc vali-vali-0 -n garden -o jsonpath='{.spec.resources.requests.storage}')
EXPECTED_CAPACITY="110Gi"

echo "Expected capacity: ${EXPECTED_CAPACITY}"
echo "Actual capacity: ${ACTUAL_CAPACITY}"

if [ "${ACTUAL_CAPACITY}" = "${EXPECTED_CAPACITY}" ]; then
    echo "✅ SUCCESS: PVC vali-vali-0 has been resized to ${EXPECTED_CAPACITY}"
    kubectl -n garden get pvc vali-vali-0 -o yaml
    exit 0
else
    echo "❌ FAILURE: Expected ${EXPECTED_CAPACITY}, but got ${ACTUAL_CAPACITY}"
    echo "PVC Status:"
    kubectl get pvc vali-vali-0 -n garden -o yaml
    echo "PVCA Status:"
    kubectl get pvca -n garden -o yaml
    exit 1
fi

