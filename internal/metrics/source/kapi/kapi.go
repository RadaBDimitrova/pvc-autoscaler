// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package kapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	metricssource "github.com/gardener/pvc-autoscaler/internal/metrics/source"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// KAPISource implements the metrics source interface using direct KAPI access.
type KAPISource struct {
	client     client.Client
	httpClient *http.Client
	apiHost    string
}

// Option is a function which configures the [KAPISource].
type Option func(*KAPISource)

// WithClient configures the [KAPISource] with the given Kubernetes client.
func WithClient(c client.Client) Option {
	return func(s *KAPISource) {
		s.client = c
	}
}

// WithHTTPClient configures the [KAPISource] with the given HTTP client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(s *KAPISource) {
		s.httpClient = httpClient
	}
}

// WithAPIHost configures the [KAPISource] with the given API server host.
func WithAPIHost(apiHost string) Option {
	return func(s *KAPISource) {
		s.apiHost = apiHost
	}
}

// New creates a new kubelet API metrics source with the given options.
func New(opts ...Option) (*KAPISource, error) {
	s := &KAPISource{}

	for _, opt := range opts {
		opt(s)
	}

	if s.client == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}

	if s.httpClient == nil {
		s.httpClient = &http.Client{}
	}

	return s, nil
}

// Get implements the metricssource.Source interface by fetching volume metrics
// directly from kubelet via the Kubernetes API proxy.
func (s *KAPISource) Get(ctx context.Context) (metricssource.Metrics, error) {
	logger := log.FromContext(ctx).WithName("kapi-source")

	// Get all nodes in the cluster
	nodeList := &corev1.NodeList{}
	if err := s.client.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	if len(nodeList.Items) == 0 {
		logger.Info("no nodes found in cluster")
		return metricssource.Metrics{}, nil
	}

	metrics := make(metricssource.Metrics)

	// Try to get metrics from each node
	for _, node := range nodeList.Items {
		nodeMetrics, err := s.getNodeMetrics(ctx, node.Name)
		if err != nil {
			logger.V(1).Info("failed to get metrics from node", "node", node.Name, "error", err)
			continue
		}

		// Merge node metrics into the overall metrics map
		for key, value := range nodeMetrics {
			metrics[key] = value
		}
	}

	logger.V(1).Info("collected kubelet metrics", "pvcCount", len(metrics))
	return metrics, nil
}

// getNodeMetrics fetches volume metrics from a specific node's kubelet.
func (s *KAPISource) getNodeMetrics(ctx context.Context, nodeName string) (metricssource.Metrics, error) {
	logger := log.FromContext(ctx).WithName("kapi-source")

	// Build kubelet proxy URL
	url := fmt.Sprintf("%s/api/v1/nodes/%s/proxy/metrics", s.apiHost, nodeName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubelet request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get kubelet metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubelet request failed with status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read kubelet response: %w", err)
	}

	metrics, err := s.parseKubeletMetrics(string(body))
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubelet metrics: %w", err)
	}

	logger.V(1).Info("retrieved metrics from node", "node", nodeName, "pvcCount", len(metrics))
	return metrics, nil
}

// parseKubeletMetrics parses the Prometheus format metrics from kubelet
// and extracts volume statistics for PVCs.
func (s *KAPISource) parseKubeletMetrics(metricsData string) (metricssource.Metrics, error) {
	metrics := make(metricssource.Metrics)

	// Regular expressions to match volume metrics
	availableBytesRegex := regexp.MustCompile(`kubelet_volume_stats_available_bytes\{.*namespace="([^"]+)".*persistentvolumeclaim="([^"]+)".*\}\s+([0-9.e+-]+)`)
	capacityBytesRegex := regexp.MustCompile(`kubelet_volume_stats_capacity_bytes\{.*namespace="([^"]+)".*persistentvolumeclaim="([^"]+)".*\}\s+([0-9.e+-]+)`)
	availableInodesRegex := regexp.MustCompile(`kubelet_volume_stats_inodes_free\{.*namespace="([^"]+)".*persistentvolumeclaim="([^"]+)".*\}\s+([0-9.e+-]+)`)
	capacityInodesRegex := regexp.MustCompile(`kubelet_volume_stats_inodes\{.*namespace="([^"]+)".*persistentvolumeclaim="([^"]+)".*\}\s+([0-9.e+-]+)`)

	// Parse available bytes
	for _, line := range strings.Split(metricsData, "\n") {
		if matches := availableBytesRegex.FindStringSubmatch(line); matches != nil {
			namespace, pvcName, valueStr := matches[1], matches[2], matches[3]
			floatValue, err := strconv.ParseFloat(valueStr, 64)
			if err != nil {
				continue
			}
			value := int(floatValue)

			key := types.NamespacedName{Namespace: namespace, Name: pvcName}
			if metrics[key] == nil {
				metrics[key] = &metricssource.VolumeInfo{}
			}
			metrics[key].AvailableBytes = value
		}

		if matches := capacityBytesRegex.FindStringSubmatch(line); matches != nil {
			namespace, pvcName, valueStr := matches[1], matches[2], matches[3]
			floatValue, err := strconv.ParseFloat(valueStr, 64)
			if err != nil {
				continue
			}
			value := int(floatValue)

			key := types.NamespacedName{Namespace: namespace, Name: pvcName}
			if metrics[key] == nil {
				metrics[key] = &metricssource.VolumeInfo{}
			}
			metrics[key].CapacityBytes = value
		}

		if matches := availableInodesRegex.FindStringSubmatch(line); matches != nil {
			namespace, pvcName, valueStr := matches[1], matches[2], matches[3]
			floatValue, err := strconv.ParseFloat(valueStr, 64)
			if err != nil {
				continue
			}
			value := int(floatValue)

			key := types.NamespacedName{Namespace: namespace, Name: pvcName}
			if metrics[key] == nil {
				metrics[key] = &metricssource.VolumeInfo{}
			}
			metrics[key].AvailableInodes = value
		}

		if matches := capacityInodesRegex.FindStringSubmatch(line); matches != nil {
			namespace, pvcName, valueStr := matches[1], matches[2], matches[3]
			floatValue, err := strconv.ParseFloat(valueStr, 64)
			if err != nil {
				continue
			}
			value := int(floatValue)

			key := types.NamespacedName{Namespace: namespace, Name: pvcName}
			if metrics[key] == nil {
				metrics[key] = &metricssource.VolumeInfo{}
			}
			metrics[key].CapacityInodes = value
		}
	}

	return metrics, nil
}
