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
	"github.com/gardener/pvc-autoscaler/internal/utils"
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
	// ScalingReason is the reason the resize is being recommended. It is only
	// meaningful when TargetSize is non-nil.
	ScalingReason string
}

// ScalingReason determines whether — and why — the [corev1.PersistentVolumeClaim]
// should be resized. It returns an empty string when no resize is warranted. It
// is a pure predicate: it emits no events or metrics. Those side effects are
// performed by [RecommendResize] once a resize is actually acted upon.
func ScalingReason(pvc *corev1.PersistentVolumeClaim, policy v1alpha1.VolumePolicy, volumeRecommendation v1alpha1.VolumeRecommendation) string {
	var (
		threshold         = *policy.ScaleUp.UtilizationThresholdPercent
		usedSpacePercent  = ptr.Deref(volumeRecommendation.Current.UsedSpacePercent, 0)
		usedInodesPercent = ptr.Deref(volumeRecommendation.Current.UsedInodesPercent, 0)
	)

	var reason string
	switch {
	// Used space reached threshold
	case usedSpacePercent > threshold:
		reason = common.ScalingReasonStorageThreshold

	// Used inodes reached threshold
	case usedInodesPercent > threshold:
		reason = common.ScalingReasonInodesThreshold

	// No need to resize the PVC for now
	default:
		return ""
	}

	// A resize is warranted, but the PVC may already be at (or within one
	// scaling resolution of) its configured max capacity, in which case there
	// is no room left to grow.
	specSize := pvc.Spec.Resources.Requests.Storage()
	if policy.MaxCapacity.Value()-specSize.Value() < common.ScalingResolutionBytes {
		return common.ScalingReasonMaxCapacity
	}

	return reason
}

// RecommendResize decides whether the [corev1.PersistentVolumeClaim] should be
// resized and, when it should, computes the target size. The decision (and the
// reason for it) is derived from [ScalingReason]. It does not mutate the PVC, it
// only provides recommendations.
func RecommendResize(logger logr.Logger, eventRecorder record.EventRecorder, pvc *corev1.PersistentVolumeClaim, policy v1alpha1.VolumePolicy, volumeRecommendation v1alpha1.VolumeRecommendation, resizingConditions *conditions.ResizingConditionAggregator) Recommendation {
	var (
		threshold         = *policy.ScaleUp.UtilizationThresholdPercent
		usedSpacePercent  = ptr.Deref(volumeRecommendation.Current.UsedSpacePercent, 0)
		usedInodesPercent = ptr.Deref(volumeRecommendation.Current.UsedInodesPercent, 0)
		specSize          = pvc.Spec.Resources.Requests.Storage()
	)

	scalingReason := ScalingReason(pvc, policy, volumeRecommendation)
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

	// A resize is warranted, but if one is already in progress we should not
	// recommend another one.
	if isResizeInProgress(logger, pvc, scalingReason, resizingConditions) {
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
		ScalingReason:        scalingReason,
	}
}

// isResizeInProgress checks whether the [corev1.PersistentVolumeClaim] is currently being resized.
// Returns true if a resize operation is in progress.
func isResizeInProgress(logger logr.Logger, pvc *corev1.PersistentVolumeClaim, scalingReason string, resizingConditions *conditions.ResizingConditionAggregator) bool {
	currStatusSize := pvc.Status.Capacity.Storage()

	if utils.IsPersistentVolumeClaimConditionTrue(pvc, corev1.PersistentVolumeClaimResizing) {
		logger.Info("resize has been started")
		resizingConditions.AddCondition(metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeResizing),
			Status:  metav1.ConditionTrue,
			Reason:  conditions.ReasonReconcile,
			Message: fmt.Sprintf("%s: is being scaled due to %s, resize has been started", pvc.Name, scalingReason),
		})

		return true
	}

	if utils.IsPersistentVolumeClaimConditionTrue(pvc, corev1.PersistentVolumeClaimFileSystemResizePending) {
		logger.Info("filesystem resize is pending")
		resizingConditions.AddCondition(metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeResizing),
			Status:  metav1.ConditionTrue,
			Reason:  conditions.ReasonReconcile,
			Message: fmt.Sprintf("%s: is being scaled due to %s, file system resize is pending", pvc.Name, scalingReason),
		})

		return true
	}

	if utils.IsPersistentVolumeClaimConditionTrue(pvc, corev1.PersistentVolumeClaimVolumeModifyingVolume) {
		logger.Info("volume is being modified")
		resizingConditions.AddCondition(metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeResizing),
			Status:  metav1.ConditionTrue,
			Reason:  conditions.ReasonReconcile,
			Message: fmt.Sprintf("%s: is being scaled due to %s, volume is being modified", pvc.Name, scalingReason),
		})

		return true
	}

	scaledFromAnnotationValue, ok := pvc.Annotations[common.AnnotationPreviousSize]
	if !ok {
		// scaled-from annotation is missing from PVC which means it has not been scaled by the pvc-autoscaler.
		return false
	}

	scaledFrom, err := resource.ParseQuantity(scaledFromAnnotationValue)
	if err != nil {
		resizingConditions.AddCondition(metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeResizing),
			Status:  metav1.ConditionUnknown,
			Reason:  conditions.ReasonReconcile,
			Message: fmt.Sprintf("%s: could not parse %s annotation with value %s: %s", pvc.Name, common.AnnotationPreviousSize, scaledFromAnnotationValue, err.Error()),
		})

		return true
	}

	// If recorded size in the annotation is equal to the current status it means
	// we are still waiting for the resize to complete. This is necessary as the controller responsible
	// to do the resizing might have started it, but not yet updated the PVC's conditions.
	if scaledFrom.Equal(*currStatusSize) {
		logger.Info("persistent volume claim is still being resized")
		resizingConditions.AddCondition(metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeResizing),
			Status:  metav1.ConditionTrue,
			Reason:  conditions.ReasonReconcile,
			Message: fmt.Sprintf("%s: is being scaled due to %s, persistent volume claim is still being resized", pvc.Name, scalingReason),
		})

		return true
	}

	return false
}
