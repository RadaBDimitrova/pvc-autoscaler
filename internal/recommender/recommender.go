package recommender

import (
	"fmt"
	"math"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
)

// Recommendation is the outcome of [RecommendResize]. It captures both whether a
// resize is warranted and, when it is, the fully computed target size so the
// resizer only has to perform the patch. A nil TargetSize means no resize is
// warranted (including when the PVC is already at max capacity).
type Recommendation struct {
	// TargetSize is the size the PVC should be resized to. It is nil when no
	// resize is warranted.
	TargetSize *resource.Quantity
	// ClampedToMaxCapacity reports that TargetSize was clamped down to the
	// policy's max capacity.
	ClampedToMaxCapacity bool
}

// RecommendResize decides whether the [corev1.PersistentVolumeClaim] should be
// resized for the given scalingReason and, when it should, computes the target
// size. It does not mutate the PVC, it only provides recommendations.
func RecommendResize(logger logr.Logger, eventRecorder record.EventRecorder, pvc *corev1.PersistentVolumeClaim, scalingReason string, policy v1alpha1.VolumePolicy, volumeRecommendation v1alpha1.VolumeRecommendation, resizingConditions *conditions.ResizingConditionAggregator) Recommendation {
	var (
		threshold         = *policy.ScaleUp.UtilizationThresholdPercent
		usedSpacePercent  = ptr.Deref(volumeRecommendation.Current.UsedSpacePercent, 0)
		usedInodesPercent = ptr.Deref(volumeRecommendation.Current.UsedInodesPercent, 0)
		specSize          = pvc.Spec.Resources.Requests.Storage()
	)

	switch scalingReason {
	// Already at max capacity, so do not resize
	case common.ScalingReasonMaxCapacity:
		eventRecorder.Eventf(
			pvc,
			corev1.EventTypeWarning,
			"MaxCapacityReached",
			"max capacity (%s) has been reached, will not resize",
			policy.MaxCapacity.String(),
		)

		// The PVC is already at max capacity, so bump the metric and record the
		// condition (unless resizing is turned off for this policy).
		if policy.ScaleUp.ResizeStrategy != v1alpha1.OffVolumeResizeStrategy {
			metrics.MaxCapacityReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name).Inc()
			resizingConditions.AddCondition(metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeResizing),
				Status:  metav1.ConditionFalse,
				Reason:  conditions.ReasonReconcile,
				Message: fmt.Sprintf("%s: max capacity reached", pvc.Name),
			})
		}

		return Recommendation{}

	// Used space reached threshold
	case common.ScalingReasonStorageThreshold:
		eventRecorder.Eventf(
			pvc,
			corev1.EventTypeWarning,
			"UsedSpaceThresholdReached",
			"used space (%d%%) exceeds the configured threshold (%d%%)",
			usedSpacePercent,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name, "space").Inc()

	// Used inodes reached threshold
	case common.ScalingReasonInodesThreshold:
		eventRecorder.Eventf(
			pvc,
			corev1.EventTypeWarning,
			"UsedInodesThresholdReached",
			"used inodes (%d%%) exceeds the configured threshold (%d%%)",
			usedInodesPercent,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name, "inodes").Inc()

	// No need to reconcile the PVC for now
	default:
		return Recommendation{}
	}

	// Compute the target size for the resize
	stepPercent := float64(*policy.ScaleUp.StepPercent)
	increment := math.Max(float64(specSize.Value())*(stepPercent/100.0), float64(policy.ScaleUp.MinStepAbsolute.Value()))
	targetSizeBytes := int64(math.Ceil((float64(specSize.Value())+increment)/1073741824)) * 1073741824
	targetSize := resource.NewQuantity(targetSizeBytes, resource.BinarySI)

	// Check that we've got a valid new size
	switch targetSize.Cmp(*specSize) {
	case 0:
		logger.Info("new and current size are the same")

		return Recommendation{}
	case -1:
		logger.Info("new size is less than current")

		return Recommendation{}
	}

	// We don't want to exceed the max capacity
	clampedToMaxCapacity := false
	if targetSize.Value() >= policy.MaxCapacity.Value() {
		// Clamp to max capacity instead of overshooting it
		targetSize = &policy.MaxCapacity
		clampedToMaxCapacity = true
	}

	if policy.ScaleUp.ResizeStrategy != v1alpha1.OffVolumeResizeStrategy && policy.ScaleUp.CooldownDuration != nil {
		lastResizeTime := volumeRecommendation.LastResizeTime
		if lastResizeTime != nil {
			elapsed := time.Since(lastResizeTime.Time)
			cooldown := policy.ScaleUp.CooldownDuration.Duration
			if elapsed < cooldown {
				remaining := cooldown - elapsed
				logger.Info("cooldown period not elapsed", "remaining", remaining.String())
				resizingConditions.AddCondition(metav1.Condition{
					Type:    string(v1alpha1.ConditionTypeResizing),
					Status:  metav1.ConditionFalse,
					Reason:  conditions.ReasonPVCResizeCooldown,
					Message: fmt.Sprintf("%s: cooldown duration has not elapsed yet", pvc.Name),
				})

				return Recommendation{}
			}
		}
	}

	return Recommendation{
		TargetSize:           targetSize,
		ClampedToMaxCapacity: clampedToMaxCapacity,
	}
}
