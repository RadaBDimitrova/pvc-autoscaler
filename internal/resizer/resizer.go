package resizer

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
)

// resizePVC performs the actual resize of the [corev1.PersistentVolumeClaim] targeted by the given
// [v1alpha1.PersistentVolumeClaimAutoscaler].
func ResizePVC(ctx context.Context, logger logr.Logger, c client.Client, eventRecorder record.EventRecorder, pvc *corev1.PersistentVolumeClaim, policy v1alpha1.VolumePolicy, scalingReason string, volumeRecommendation v1alpha1.VolumeRecommendation, resizingConditions *conditions.ResizingConditionAggregator) (v1alpha1.VolumeRecommendation, error) {
	currSpecSize := pvc.Spec.Resources.Requests.Storage()

	// Calculate the new size
	stepPercent := float64(*policy.ScaleUp.StepPercent)
	increment := math.Max(float64(currSpecSize.Value())*(stepPercent/100.0), float64(policy.ScaleUp.MinStepAbsolute.Value()))
	targetSizeBytes := int64(math.Ceil((float64(currSpecSize.Value())+increment)/1073741824)) * 1073741824
	targetSize := resource.NewQuantity(targetSizeBytes, resource.BinarySI)

	// Check that we've got a valid new size
	cmp := targetSize.Cmp(*currSpecSize)
	switch cmp {
	case 0:
		logger.Info("new and current size are the same")

		return volumeRecommendation, nil
	case -1:
		logger.Info("new size is less than current")

		return volumeRecommendation, nil
	}

	// We don't want to exceed the max capacity
	if targetSize.Value() > policy.MaxCapacity.Value() {
		// Only clamp to max capacity if the increase is at least one scaling resolution,
		// otherwise the increase is too small to be meaningful
		if policy.MaxCapacity.Value()-currSpecSize.Value() < common.ScalingResolutionBytes {
			eventRecorder.Eventf(
				pvc,
				corev1.EventTypeWarning,
				"MaxCapacityReached",
				"max capacity (%s) has been reached",
				policy.MaxCapacity.String(),
			)
			logger.Info("max capacity reached")

			if policy.ScaleUp.ResizeStrategy != v1alpha1.OffVolumeResizeStrategy {
				metrics.MaxCapacityReachedTotal.WithLabelValues(pvc.Namespace, pvc.Name).Inc()
				resizingConditions.AddCondition(metav1.Condition{
					Type:    string(v1alpha1.ConditionTypeResizing),
					Status:  metav1.ConditionFalse,
					Reason:  conditions.ReasonReconcile,
					Message: fmt.Sprintf("%s: max capacity reached", pvc.Name),
				})
			}

			return volumeRecommendation, nil
		}
		// Clamp to max capacity instead of skipping the resize entirely
		targetSize = &policy.MaxCapacity
	}

	if policy.ScaleUp.ResizeStrategy == v1alpha1.OffVolumeResizeStrategy {
		volumeRecommendation.Target.Size = targetSize

		return volumeRecommendation, nil
	}

	if policy.ScaleUp.CooldownDuration != nil {
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

				return volumeRecommendation, nil
			}
		}
	}

	// And finally we should be good to resize now
	logger.Info("resizing persistent volume claim", "from", currSpecSize.String(), "to", targetSize.String())
	metrics.ResizedTotal.WithLabelValues(pvc.Namespace, pvc.Name).Inc()
	eventRecorder.Eventf(
		pvc,
		corev1.EventTypeNormal,
		"ResizingStorage",
		"resizing storage from %s to %s",
		currSpecSize.String(),
		targetSize.String(),
	)

	// Update the PVC resource.
	pvcPatch := client.MergeFrom(pvc.DeepCopy())

	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[common.AnnotationPreviousSize] = currSpecSize.String()

	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = *targetSize
	if err := c.Patch(ctx, pvc, pvcPatch); err != nil {
		resizingConditions.AddCondition(metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeResizing),
			Status:  metav1.ConditionFalse,
			Reason:  conditions.ReasonReconcile,
			Message: fmt.Sprintf("%s: could not patch PersistentVolumeClaim with new target size %s", pvc.Name, targetSize.String()),
		})

		return volumeRecommendation, err
	}
	volumeRecommendation.Target.Size = targetSize
	volumeRecommendation.LastResizeTime = ptr.To(metav1.Now())

	resizingConditions.AddCondition(metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeResizing),
		Status:  metav1.ConditionTrue,
		Reason:  conditions.ReasonReconcile,
		Message: fmt.Sprintf("%s: resizing from %s to %s due to %s", pvc.Name, currSpecSize.String(), targetSize.String(), scalingReason),
	})

	return volumeRecommendation, nil
}
