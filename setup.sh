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

