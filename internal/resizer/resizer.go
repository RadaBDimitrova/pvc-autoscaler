package resizer

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	"github.com/gardener/pvc-autoscaler/internal/recommender"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
)

// ErrVolumeModeIsNotFilesystem is an error which is returned if a target PVC
// for resizing is not using the Filesystem VolumeMode.
var ErrVolumeModeIsNotFilesystem = errors.New("volume mode is not filesystem")

// ErrStorageClassNotFound is an error which is returned when the storage class
// for a PVC is not found.
var ErrStorageClassNotFound = errors.New("no storage class found")

// ErrStorageClassDoesNotSupportExpansion is an error which is returned when an
// annotated PVC uses a storage class that does not support volume expansion.
var ErrStorageClassDoesNotSupportExpansion = errors.New("storage class does not support expansion")

// ErrPVCNotBound is returned when the PVC is not in the Bound phase.
var ErrPVCNotBound = errors.New("PersistentVolumeClaim is not bound")

// ValidatePVC checks whether the [corev1.PersistentVolumeClaim] is eligible for
// resizing based on its current state and the associated volume policy. It
// returns nil when the PVC can be resized.
func ValidatePVC(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, policy v1alpha1.VolumePolicy) error {
	currStatusSize := pvc.Status.Capacity.Storage()
	if currStatusSize.IsZero() {
		return fmt.Errorf(".status.capacity.storage is invalid: %s", currStatusSize.String())
	}

	if policy.MaxCapacity.Value() < currStatusSize.Value() {
		return fmt.Errorf("max capacity (%s) cannot be less than current size (%s)", policy.MaxCapacity.String(), currStatusSize.String())
	}

	// We need a StorageClass with expansion support
	scName := ptr.Deref(pvc.Spec.StorageClassName, "")
	if scName == "" {
		return ErrStorageClassNotFound
	}

	var sc storagev1.StorageClass
	scKey := types.NamespacedName{Name: scName}
	if err := c.Get(ctx, scKey, &sc); err != nil {
		return err
	}

	if !ptr.Deref(sc.AllowVolumeExpansion, false) {
		return ErrStorageClassDoesNotSupportExpansion
	}

	// VolumeMode should be Filesystem
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return ErrVolumeModeIsNotFilesystem
	}

	// The PVC should be bound
	if pvc.Status.Phase != corev1.ClaimBound {
		return ErrPVCNotBound
	}

	return nil
}

// ResizePVC patches the [corev1.PersistentVolumeClaim] with the target size from
// the given [recommender.Recommendation], which must have already been computed by
// the recommender. It records the resize metric, event and condition, and returns
// the updated [v1alpha1.VolumeRecommendation].
func ResizePVC(ctx context.Context, logger logr.Logger, c client.Client, eventRecorder record.EventRecorder, pvc *corev1.PersistentVolumeClaim, recommendation recommender.Recommendation, volumeRecommendation v1alpha1.VolumeRecommendation, resizingConditions *conditions.ResizingConditionAggregator) (v1alpha1.VolumeRecommendation, error) {
	currSpecSize := pvc.Spec.Resources.Requests.Storage()
	targetSize := recommendation.TargetSize

	// When resizing is turned off for this policy, surface the recommended
	// target size in the status but do not modify the PVC.
	if recommendation.ResizeStrategy == v1alpha1.OffVolumeResizeStrategy {
		logger.Info("resize strategy is off, not resizing persistent volume claim", "recommended", targetSize.String())
		volumeRecommendation.Target.Size = targetSize

		return volumeRecommendation, nil
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
		Message: fmt.Sprintf("%s: resizing from %s to %s due to %s", pvc.Name, currSpecSize.String(), targetSize.String(), recommendation.ScalingReason),
	})

	return volumeRecommendation, nil
}
