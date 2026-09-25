package resizer

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	"github.com/gardener/pvc-autoscaler/internal/recommender"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
)

// ResizePVC patches the [corev1.PersistentVolumeClaim] with the target size from
// the given [recommender.Recommendation], which must have already been computed by
// the recommender. It records the resize metric, event and condition, and returns
// the updated [v1alpha1.VolumeRecommendation].
func ResizePVC(ctx context.Context, logger logr.Logger, c client.Client, eventRecorder record.EventRecorder, pvc *corev1.PersistentVolumeClaim, scalingReason string, recommendation recommender.Recommendation, volumeRecommendation v1alpha1.VolumeRecommendation, resizingConditions *conditions.ResizingConditionAggregator) (v1alpha1.VolumeRecommendation, error) {
	currSpecSize := pvc.Spec.Resources.Requests.Storage()
	targetSize := recommendation.TargetSize

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
