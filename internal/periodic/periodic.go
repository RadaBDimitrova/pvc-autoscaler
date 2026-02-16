// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package periodic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	metricssource "github.com/gardener/pvc-autoscaler/internal/metrics/source"
	"github.com/gardener/pvc-autoscaler/internal/utils"
)

// UnknownUtilizationValue is the value which will be used when the free
// space/inodes utilization is unknown.
const UnknownUtilizationValue = "unknown"

// ErrNoMetricsSource is returned when the [Runner] is configured without a
// metrics source.
var ErrNoMetricsSource = errors.New("no metrics source provided")

// ErrVolumeModeIsNotFilesystem is an error which is returned if a target PVC
// for resizing is not using the Filesystem VolumeMode.
var ErrVolumeModeIsNotFilesystem = errors.New("volume mode is not filesystem")

// ErrStorageClassNotFound is an error which is returned when the storage class
// for a PVC is not found.
var ErrStorageClassNotFound = errors.New("no storage class found")

// ErrStorageClassDoesNotSupportExpansion is an error which is returned when an
// annotated PVC uses a storage class that does not support volume expansion.
var ErrStorageClassDoesNotSupportExpansion = errors.New("storage class does not support expansion")

// ErrNoClient is an error which is returned when the periodic [Runner] was
// configured configured without a Kubernetes API client.
var ErrNoClient = errors.New("no client provided")

// Runner is a [sigs.k8s.io/controller-runtime/pkg/manager.Runnable], which
// processes [v1alpha1.PersistentVolumeClaimAutoscaler] items on a regular basis
// and performs PVC resizing when thresholds are reached.
type Runner struct {
	client        client.Client
	interval      time.Duration
	metricsSource metricssource.Source
	eventRecorder record.EventRecorder
}

var _ manager.Runnable = &Runner{}

// Option is a function which configures the [Runner].
type Option func(r *Runner)

// New creates a new [Runner] with the given options.
func New(opts ...Option) (*Runner, error) {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}

	if r.metricsSource == nil {
		return nil, ErrNoMetricsSource
	}

	if r.eventRecorder == nil {
		return nil, common.ErrNoEventRecorder
	}

	if r.client == nil {
		return nil, ErrNoClient
	}

	return r, nil
}

// WithClient configures the [Runner] with the given client.
func WithClient(c client.Client) Option {
	opt := func(r *Runner) {
		r.client = c
	}

	return opt
}

// WithInterval configures the [Runner] with the given interval.
func WithInterval(interval time.Duration) Option {
	opt := func(r *Runner) {
		r.interval = interval
	}

	return opt
}

// WithMetricsSource configures the [Runner] to use the given source of metrics.
func WithMetricsSource(src metricssource.Source) Option {
	opt := func(r *Runner) {
		r.metricsSource = src
	}

	return opt
}

// WithEventRecorder configures the [Runner] to use the given event recorder.
func WithEventRecorder(recorder record.EventRecorder) Option {
	opt := func(r *Runner) {
		r.eventRecorder = recorder
	}

	return opt
}

// Start implements the
// [sigs.k8s.io/controller-runtime/pkg/manager.Runnable] interface.
func (r *Runner) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	logger := log.FromContext(ctx, "controller", common.ControllerName)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := r.reconcileAll(ctx); err != nil {
				logger.Error(err, "failed to reconcile persistentvolumeclaimautoscalers")
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// reconcileAll processes all [v1alpha1.PersistentVolumeClaimAutoscaler]
// resources and resizes PVCs when thresholds are reached.
func (r *Runner) reconcileAll(ctx context.Context) error {
	var items v1alpha1.PersistentVolumeClaimAutoscalerList
	if err := r.client.List(ctx, &items); err != nil {
		return err
	}

	// Nothing to do for now
	if len(items.Items) == 0 {
		return nil
	}

	metricsData, err := r.metricsSource.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get metrics: %w", err)
	}

	for _, item := range items.Items {
		pvcObjKey := client.ObjectKey{Namespace: item.Namespace, Name: item.Spec.TargetRef.Name}
		volInfo := metricsData[pvcObjKey]
		logger := log.FromContext(
			ctx,
			"controller", common.ControllerName,
			"namespace", item.Namespace,
			"name", item.Name,
			"pvc", item.Spec.TargetRef.Name,
		)

		ok, err := r.shouldReconcilePVC(ctx, &item, volInfo)
		if err != nil {
			logger.Info("skipping persistentvolumeclaim", "reason", err.Error())
			metrics.SkippedTotal.WithLabelValues(item.Namespace, item.Name, err.Error()).Inc()
			condition := metav1.Condition{
				Type:    utils.ConditionTypeHealthy,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: err.Error(),
			}
			if err := item.SetCondition(ctx, r.client, condition); err != nil {
				logger.Info("failed to update status condition", "reason", err.Error())
			}

			continue
		}

		if ok {
			// Directly resize the PVC instead of sending to channel
			if err := r.resizePVC(ctx, &item); err != nil {
				logger.Error(err, "failed to resize pvc")
			}
		} else {
			condition := metav1.Condition{
				Type:    utils.ConditionTypeHealthy,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: "Successfully reconciled",
			}
			if err := item.SetCondition(ctx, r.client, condition); err != nil {
				logger.Info("failed to update status condition", "reason", err.Error())
			}
		}
	}

	return nil
}

// updatePVCAStatus updates the status of the
// [v1alpha1.PersistentVolumeClaimAutoscaler] with the latest observed
// information about the target [corev1.PersistentVolumeClaim].
func (r *Runner) updatePVCAStatus(ctx context.Context, obj *v1alpha1.PersistentVolumeClaimAutoscaler, volInfo *metricssource.VolumeInfo) error {
	patch := client.MergeFrom(obj.DeepCopy())
	now := time.Now()
	nextCheck := now.Add(r.interval)

	freeSpaceStr := UnknownUtilizationValue
	usedSpaceStr := UnknownUtilizationValue
	freeInodesStr := UnknownUtilizationValue
	usedInodesStr := UnknownUtilizationValue

	if volInfo != nil {
		if freeSpace, err := volInfo.FreeSpacePercentage(); err == nil {
			freeSpaceStr = fmt.Sprintf("%.2f%%", freeSpace)
		}

		if usedSpace, err := volInfo.UsedSpacePercentage(); err == nil {
			usedSpaceStr = fmt.Sprintf("%.2f%%", usedSpace)
		}

		if freeInodes, err := volInfo.FreeInodesPercentage(); err == nil {
			freeInodesStr = fmt.Sprintf("%.2f%%", freeInodes)
		}

		if usedInodes, err := volInfo.UsedInodesPercentage(); err == nil {
			usedInodesStr = fmt.Sprintf("%.2f%%", usedInodes)
		}
	}

	obj.Status.LastCheck = metav1.NewTime(now)
	obj.Status.NextCheck = metav1.NewTime(nextCheck)
	obj.Status.UsedSpacePercentage = usedSpaceStr
	obj.Status.FreeSpacePercentage = freeSpaceStr
	obj.Status.UsedInodesPercentage = usedInodesStr
	obj.Status.FreeInodesPercentage = freeInodesStr

	return r.client.Status().Patch(ctx, obj, patch)
}

// shouldReconcilePVC is a predicate which checks whether the
// [corev1.PersistentVolumeClaim] object targeted by
// [v1alpha1.PersistentVolumeClaimAutoscaler] should be considered for
// reconciliation.
func (r *Runner) shouldReconcilePVC(ctx context.Context, pvca *v1alpha1.PersistentVolumeClaimAutoscaler, volInfo *metricssource.VolumeInfo) (bool, error) {
	pvcObjKey := client.ObjectKey{Namespace: pvca.Namespace, Name: pvca.Spec.TargetRef.Name}
	pvcObj := &corev1.PersistentVolumeClaim{}
	if err := r.client.Get(ctx, pvcObjKey, pvcObj); err != nil {
		return false, err
	}

	if err := r.updatePVCAStatus(ctx, pvca, volInfo); err != nil {
		return false, err
	}

	// No metrics found, nothing to do for now
	if volInfo == nil {
		return false, common.ErrNoMetrics
	}

	// Validate the PVC itself against the spec
	currStatusSize := pvcObj.Status.Capacity.Storage()
	if currStatusSize.IsZero() {
		return false, fmt.Errorf(".status.capacity.storage is invalid: %s", currStatusSize.String())
	}

	// Only one volume policy is supported currently
	policy := pvca.Spec.VolumePolicies[0]
	if policy.MaxCapacity.Value() < currStatusSize.Value() {
		return false, fmt.Errorf("max capacity (%s) cannot be less than current size (%s)", policy.MaxCapacity.String(), currStatusSize.String())
	}

	// We need a StorageClass with expansion support
	scName := ptr.Deref(pvcObj.Spec.StorageClassName, "")
	if scName == "" {
		return false, ErrStorageClassNotFound
	}

	var sc storagev1.StorageClass
	scKey := types.NamespacedName{Name: scName}
	if err := r.client.Get(ctx, scKey, &sc); err != nil {
		return false, err
	}

	if !ptr.Deref(sc.AllowVolumeExpansion, false) {
		return false, ErrStorageClassDoesNotSupportExpansion
	}

	// Detect whether the metrics source is reporting stale data. Stale
	// metrics data would be when the volume info metrics reported by the
	// metrics source are deviate from the current PVC size indicated by
	// `.status.capacity.storage'
	if statusSize, ok := currStatusSize.AsInt64(); ok {
		delta := statusSize - int64(volInfo.CapacityBytes)
		if delta < 0 {
			delta = -delta
		}
		if delta > common.ScalingResolutionBytes/2 {
			return false, fmt.Errorf("stale metrics data detected: pvc size=%d bytes, metrics size=%d bytes:%w", statusSize, volInfo.CapacityBytes, common.ErrStaleMetrics)
		}
	}

	// Getting an error from FreeSpacePercentage() means that the
	// capacity for the volume is zero, which in turn means that we
	// didn't get any metrics for it.
	freeSpace, err := volInfo.FreeSpacePercentage()
	if err != nil {
		return false, common.ErrNoMetrics
	}

	// Even, if we don't have inode metrics we still want to proceed here.
	freeInodes, err := volInfo.FreeInodesPercentage()
	if err != nil {
		return false, common.ErrNoMetrics
	}

	// Get threshold from volume policy
	// Currently only one policy is supported and is enforced by the CRD schema
	threshold := 100.0 - float64(*policy.ScaleUp.UtilizationThresholdPercent)

	// VolumeMode should be Filesystem
	if pvcObj.Spec.VolumeMode == nil {
		return false, nil
	}
	if *pvcObj.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return false, ErrVolumeModeIsNotFilesystem
	}

	// The PVC should be bound
	if pvcObj.Status.Phase != corev1.ClaimBound {
		return false, nil
	}

	switch {
	// Free space reached threshold
	case freeSpace < threshold:
		r.eventRecorder.Eventf(
			pvcObj,
			corev1.EventTypeWarning,
			"FreeSpaceThresholdReached",
			"free space (%.2f%%) is less than the configured threshold (%.2f%%)",
			freeSpace,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvcObj.Namespace, pvcObj.Name, "space").Inc()

		return true, nil

	// Free inodes reached threshold
	case volInfo.CapacityInodes > 0.0 && (freeInodes < threshold):
		r.eventRecorder.Eventf(
			pvcObj,
			corev1.EventTypeWarning,
			"FreeInodesThresholdReached",
			"free inodes (%.2f%%) are less than the configured threshold (%.2f%%)",
			freeInodes,
			threshold,
		)
		metrics.ThresholdReachedTotal.WithLabelValues(pvcObj.Namespace, pvcObj.Name, "inodes").Inc()

		return true, nil

	// No need to reconcile the PVC for now
	default:
		return false, nil
	}
}

// resizePVC performs the actual resize of the PVC targeted by the given
// [v1alpha1.PersistentVolumeClaimAutoscaler].
func (r *Runner) resizePVC(ctx context.Context, pvca *v1alpha1.PersistentVolumeClaimAutoscaler) error {
	pvcObjKey := client.ObjectKey{Namespace: pvca.Namespace, Name: pvca.Spec.TargetRef.Name}
	pvcObj := &corev1.PersistentVolumeClaim{}
	if err := r.client.Get(ctx, pvcObjKey, pvcObj); err != nil {
		return client.IgnoreNotFound(err)
	}

	logger := log.FromContext(ctx).WithValues("pvc", pvcObj.Name)
	currSpecSize := pvcObj.Spec.Resources.Requests.Storage()
	currStatusSize := pvcObj.Status.Capacity.Storage()

	// Make sure that the PVC is not being modified at the moment.
	if utils.IsPersistentVolumeClaimConditionTrue(pvcObj, corev1.PersistentVolumeClaimResizing) {
		logger.Info("resize has been started")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Resize has been started",
		}
		return pvca.SetCondition(ctx, r.client, condition)
	}

	if utils.IsPersistentVolumeClaimConditionTrue(pvcObj, corev1.PersistentVolumeClaimFileSystemResizePending) {
		logger.Info("filesystem resize is pending")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "File system resize is pending",
		}
		return pvca.SetCondition(ctx, r.client, condition)
	}

	if utils.IsPersistentVolumeClaimConditionTrue(pvcObj, corev1.PersistentVolumeClaimVolumeModifyingVolume) {
		logger.Info("volume is being modified")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Volume is being modified",
		}
		return pvca.SetCondition(ctx, r.client, condition)
	}

	// If previously recorded size is equal to the current status it means
	// we are still waiting for the resize to complete
	if pvca.Status.PrevSize.Equal(*currStatusSize) {
		logger.Info("persistent volume claim is still being resized")
		condition := metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Persistent volume claim is still being resized",
		}
		return pvca.SetCondition(ctx, r.client, condition)
	}

	// Loop through volume policies. Currently only one policy is supported,
	// but this structure allows for better readability and future expansion.
	var condition metav1.Condition
	for _, policy := range pvca.Spec.VolumePolicies {
		// Calculate the new size
		stepPercent := float64(*policy.ScaleUp.StepPercent)
		increment := math.Max(float64(currSpecSize.Value())*(stepPercent/100.0), float64(policy.ScaleUp.MinStepAbsolute.Value()))
		newSizeBytes := int64(math.Ceil((float64(currSpecSize.Value())+increment)/1073741824)) * 1073741824
		newSize := resource.NewQuantity(newSizeBytes, resource.BinarySI)

		// Check that we've got a valid new size
		cmp := newSize.Cmp(*currSpecSize)
		switch cmp {
		case 0:
			logger.Info("new and current size are the same")
			return nil
		case -1:
			logger.Info("new size is less than current")
			return nil
		}

		// We don't want to exceed the max capacity
		if newSize.Value() > policy.MaxCapacity.Value() {
			r.eventRecorder.Eventf(
				pvcObj,
				corev1.EventTypeWarning,
				"MaxCapacityReached",
				"max capacity (%s) has been reached, will not resize",
				policy.MaxCapacity.String(),
			)
			logger.Info("max capacity reached")
			metrics.MaxCapacityReachedTotal.WithLabelValues(pvcObj.Namespace, pvcObj.Name).Inc()
			condition = metav1.Condition{
				Type:    utils.ConditionTypeHealthy,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: "Max capacity reached",
			}
			return pvca.SetCondition(ctx, r.client, condition)
		}

		// And finally we should be good to resize now
		logger.Info("resizing persistent volume claim", "from", currSpecSize.String(), "to", newSize.String())
		metrics.ResizedTotal.WithLabelValues(pvcObj.Namespace, pvcObj.Name).Inc()
		r.eventRecorder.Eventf(
			pvcObj,
			corev1.EventTypeNormal,
			"ResizingStorage",
			"resizing storage from %s to %s",
			currSpecSize.String(),
			newSize.String(),
		)

		// Update PVC and PVCA resources
		pvcPatch := client.MergeFrom(pvcObj.DeepCopy())
		pvcObj.Spec.Resources.Requests[corev1.ResourceStorage] = *newSize
		if err := r.client.Patch(ctx, pvcObj, pvcPatch); err != nil {
			return err
		}

		pvcaPatch := client.MergeFrom(pvca.DeepCopy())
		pvca.Status.PrevSize = *currStatusSize
		pvca.Status.NewSize = *newSize
		if err := r.client.Status().Patch(ctx, pvca, pvcaPatch); err != nil {
			return err
		}

		condition = metav1.Condition{
			Type:    utils.ConditionTypeHealthy,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: fmt.Sprintf("Resizing from %s to %s", currSpecSize.String(), newSize.String()),
		}
	}

	return pvca.SetCondition(ctx, r.client, condition)
}
