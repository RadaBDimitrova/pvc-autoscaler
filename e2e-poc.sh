#!/usr/bin/env bash

set -e

# Build local image and load it to kind cluster
docker build -t pvc-autoscaler-local .
kind load docker-image pvc-autoscaler-local:latest --name gardener-local --nodes gardener-local-control-plane
# Use static service name for Prometheus
# 
prometheus_cache_service_ip=$(kubectl -n garden get service prometheus-cache -o yaml | yq .spec.clusterIP)
echo Prometheus service IP: $prometheus_cache_service_ip
sed -i -E 's@prometheus-address=.+@prometheus-address=http://'"$prometheus_cache_service_ip"':80"@' config/default/manager_config_patch.yaml 
echo "Config path:"
cat config/default/manager_config_patch.yaml
# Apply pvc-autoscaler CRDs
bin/kustomize build config/default | kubectl apply -f -
sleep 3
# TODO: Use webhooks
kubectl delete validatingwebhookconfigurations pvc-autoscaler-validating-webhook-configuration
kubectl delete mutatingwebhookconfigurations pvc-autoscaler-mutating-webhook-configuration
# Manually patch storage class to allow volume expansion
kubectl patch storageclass standard -p '{"allowVolumeExpansion": true}'
# Apply example PVC Autoscaler
kubectl apply -f example-pvca.yaml
# Label namespace for network policies to work
kubectl label namespace pvc-autoscaler-system gardener.cloud/role=shoot
# Create fake metrics
kubectl apply -f fake-metrics/minimal-fake-metrics.yaml

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

