// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"fmt"
	"math"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/utils"
)

// Status collects the per-PersistentVolumeClaim [v1alpha1.VolumeRecommendation]
// items that will be written to a
// [v1alpha1.PersistentVolumeClaimAutoscaler]'s status. It owns the lifecycle of
// the recommendation slice: seeding it from the existing status, looking up and
// upserting entries, and producing the sorted result for persistence.
type Status struct {
	Recommendations []v1alpha1.VolumeRecommendation
}

// New seeds a [Status] from the PVCA's existing recommendations, retaining only
// those whose [corev1.PersistentVolumeClaim] is still present and has a matching
// volume policy. Stale entries for PVCs that are no longer managed are dropped.
func New(existing []v1alpha1.VolumeRecommendation, pvcs []*corev1.PersistentVolumeClaim, policies []v1alpha1.VolumePolicy) *Status {
	recommendations := make([]v1alpha1.VolumeRecommendation, 0, len(pvcs))
	for _, recommendation := range existing {
		if !slices.ContainsFunc(pvcs, func(pvc *corev1.PersistentVolumeClaim) bool {
			return pvc.Name == recommendation.Name
		}) {
			continue
		}

		policy, err := utils.GetVolumePolicy(recommendation.Name, policies)
		if err != nil || policy == nil {
			continue
		}

		recommendations = append(recommendations, recommendation)
	}

	return &Status{Recommendations: recommendations}
}

// GetOrCreate returns the [v1alpha1.VolumeRecommendation] for the given
// [corev1.PersistentVolumeClaim] name. If none exists yet, a new one (not yet
// stored) is returned; call [Status.Set] to persist it.
func (s *Status) GetOrCreate(pvcName string) v1alpha1.VolumeRecommendation {
	for i := range s.Recommendations {
		if s.Recommendations[i].Name == pvcName {
			return s.Recommendations[i]
		}
	}

	return v1alpha1.VolumeRecommendation{
		Name: pvcName,
	}
}

// Set stores the [v1alpha1.VolumeRecommendation] for the given
// [corev1.PersistentVolumeClaim] name, replacing an existing entry or appending
// a new one.
func (s *Status) Set(pvcName string, recommendation v1alpha1.VolumeRecommendation) {
	for i := range s.Recommendations {
		if s.Recommendations[i].Name == pvcName {
			s.Recommendations[i] = recommendation

			return
		}
	}

	s.Recommendations = append(s.Recommendations, recommendation)
}

// Sorted returns the recommendations sorted by
// [corev1.PersistentVolumeClaim] name, giving a stable order for persistence.
func (s *Status) Sorted() []v1alpha1.VolumeRecommendation {
	slices.SortFunc(s.Recommendations, func(vr1, vr2 v1alpha1.VolumeRecommendation) int {
		return strings.Compare(vr1.Name, vr2.Name)
	})

	return s.Recommendations
}

// Observe records the current observed state of the [corev1.PersistentVolumeClaim]
// into the recommendation: the used space/inodes percentages, the current size,
// and — when not yet set — the target size defaulted from the PVC spec so the
// field is non-nil. It returns an error wrapping [common.ErrStaleMetrics] when
// the reported capacity deviates too far from the PVC's status capacity, which
// indicates the metrics source is reporting stale data.
func Observe(recommendation v1alpha1.VolumeRecommendation, pvc *corev1.PersistentVolumeClaim, usedSpacePercent, usedInodesPercent, capacityBytes int) (v1alpha1.VolumeRecommendation, error) {
	recommendation.Current.UsedSpacePercent = &usedSpacePercent
	recommendation.Current.UsedInodesPercent = &usedInodesPercent

	currStatusSize := pvc.Status.Capacity.Storage()
	recommendation.Current.Size = currStatusSize

	// If target size has not yet been recommended by the autoscaler, take the size from spec
	// so the field is non-nil.
	if recommendation.Target.Size == nil {
		recommendation.Target.Size = pvc.Spec.Resources.Requests.Storage()
	}

	// Detect whether the metrics source is reporting stale data. Stale
	// metrics data would be when the volume info metrics reported by the
	// metrics source deviate from the current PVC size indicated by
	// `.status.capacity.storage'
	if statusSize, ok := currStatusSize.AsInt64(); ok {
		delta := math.Abs(float64(statusSize) - float64(capacityBytes))
		tolerance := math.Max(common.MaxCapacityDeviationRatio*float64(statusSize), float64(common.ScalingResolutionBytes)/2)
		if delta > tolerance {
			return v1alpha1.VolumeRecommendation{}, fmt.Errorf("stale metrics data detected: pvc size=%d bytes, metrics size=%d bytes: %w", statusSize, capacityBytes, common.ErrStaleMetrics)
		}
	}

	return recommendation, nil
}
