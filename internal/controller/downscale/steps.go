// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package downscale

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
)

// getPVC returns the named PersistentVolumeClaim, or nil if it does not exist.
func (r *Reconciler) getPVC(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to get PersistentVolumeClaim %s/%s: %w", namespace, name, err)
	}

	return pvc, nil
}

// getPV returns the named PersistentVolume, or nil if it does not exist.
func (r *Reconciler) getPV(ctx context.Context, name string) (*corev1.PersistentVolume, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to get PersistentVolume %s: %w", name, err)
	}

	return pv, nil
}

// boundTo reports whether the PV is bound to the PVC identified by namespace/name.
func boundTo(pv *corev1.PersistentVolume, namespace, name string) bool {
	return pv.Status.Phase == corev1.VolumeBound &&
		pv.Spec.ClaimRef != nil &&
		pv.Spec.ClaimRef.Namespace == namespace &&
		pv.Spec.ClaimRef.Name == name
}

// ensureScaledDown records the target's original replica count once (so it can be restored
// later) and keeps the target scaled to zero.
func (r *Reconciler) ensureScaledDown(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask) error {
	if task.Status.OriginalReplicas == nil {
		scale, _, err := r.getScale(ctx, task.Namespace, task.Spec.TargetRef)
		if err != nil {
			return fmt.Errorf("failed to get target scale: %w", err)
		}

		replicas := scale.Spec.Replicas
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.OriginalReplicas = &replicas
		if err := r.client.Status().Patch(ctx, task, patch); err != nil {
			return fmt.Errorf("failed to record original replicas: %w", err)
		}

		logger.Info("recorded original replicas", "replicas", replicas)
	}

	return r.setReplicas(ctx, task.Namespace, task.Spec.TargetRef, 0)
}

// restoreReplicas scales the target back up to its recorded original replica count.
func (r *Reconciler) restoreReplicas(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask) error {
	var replicas int32
	if task.Status.OriginalReplicas != nil {
		replicas = *task.Status.OriginalReplicas
	}

	if err := r.setReplicas(ctx, task.Namespace, task.Spec.TargetRef, replicas); err != nil {
		return err
	}

	logger.Info("restored target replicas", "replicas", replicas)

	return nil
}

// createTempPVC creates the temporary PVC at the target size, mirroring the original PVC's
// storage class, access modes and volume mode.
func (r *Reconciler) createTempPVC(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask, original *corev1.PersistentVolumeClaim) error {
	tempName := task.Spec.PVCName + TempPVCSuffix
	temp := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tempName,
			Namespace: task.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      original.Spec.AccessModes,
			StorageClassName: original.Spec.StorageClassName,
			VolumeMode:       original.Spec.VolumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: task.Spec.TargetSize},
			},
		},
	}

	if err := r.client.Create(ctx, temp); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create temporary PersistentVolumeClaim: %w", err)
	}

	logger.Info("created temporary PersistentVolumeClaim", "tempPVC", tempName, "size", task.Spec.TargetSize.String())

	return nil
}

// retainPV sets the reclaim policy of the given PV to Retain so it survives deletion of the
// temporary PVC.
func (r *Reconciler) retainPV(ctx context.Context, logger logr.Logger, name string) error {
	pv, err := r.getPV(ctx, name)
	if err != nil {
		return err
	}
	if pv == nil {
		return fmt.Errorf("new PersistentVolume %q not found", name)
	}

	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain {
		return nil
	}

	patch := client.MergeFrom(pv.DeepCopy())
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	if err := r.client.Patch(ctx, pv, patch); err != nil {
		return fmt.Errorf("failed to set reclaim policy on PersistentVolume %q: %w", name, err)
	}

	logger.Info("set new PersistentVolume reclaim policy to Retain", "pv", name)

	return nil
}

// recordNewVolume persists the new PV name on the task status.
func (r *Reconciler) recordNewVolume(ctx context.Context, task *v1alpha1.PVCDownscaleTask, name string) error {
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.NewVolumeName = name
	if err := r.client.Status().Patch(ctx, task, patch); err != nil {
		return fmt.Errorf("failed to record new volume name: %w", err)
	}

	return nil
}

// deleteCopy removes the data-copy Job (and its Pods) via the Copier. It is idempotent and
// is used during cleanup once the downscale has completed. The source and destination are
// reconstructed from the task because the live PVCs may already be gone by this point; the
// Copier only needs their names and the destination namespace.
func (r *Reconciler) deleteCopy(ctx context.Context, task *v1alpha1.PVCDownscaleTask) error {
	source := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: task.Spec.PVCName, Namespace: task.Namespace},
	}
	destination := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: task.Spec.PVCName + TempPVCSuffix, Namespace: task.Namespace},
	}

	if err := r.copier.DeleteCopy(ctx, source, destination); err != nil {
		return fmt.Errorf("failed to delete data copy: %w", err)
	}

	return nil
}

// deletePVC deletes the given PVC unless it is already being deleted.
func (r *Reconciler) deletePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if pvc.DeletionTimestamp != nil {
		return nil
	}

	if err := r.client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete PersistentVolumeClaim %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}

	return nil
}

// recreatePVC recreates the original-named PVC at the target size, bound to the new volume.
// The storage class, access modes and volume mode are taken from the new PV so they match.
func (r *Reconciler) recreatePVC(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask, newVolume string) error {
	pv, err := r.getPV(ctx, newVolume)
	if err != nil {
		return err
	}
	if pv == nil {
		return fmt.Errorf("new PersistentVolume %q not found", newVolume)
	}

	storageClass := pv.Spec.StorageClassName
	recreated := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      task.Spec.PVCName,
			Namespace: task.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      pv.Spec.AccessModes,
			StorageClassName: &storageClass,
			VolumeMode:       pv.Spec.VolumeMode,
			VolumeName:       newVolume,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: task.Spec.TargetSize},
			},
		},
	}

	if err := r.client.Create(ctx, recreated); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to recreate PersistentVolumeClaim: %w", err)
	}

	logger.Info("recreated PersistentVolumeClaim bound to new volume", "pvc", task.Spec.PVCName, "pv", newVolume)

	return nil
}

// rebind clears a stale claimRef on the new PV so the binder can bind it to the recreated
// PVC (which references the PV via spec.volumeName). If the claimRef is already absent, the
// PV is simply awaiting binding and nothing is done.
func (r *Reconciler) rebind(ctx context.Context, logger logr.Logger, pv *corev1.PersistentVolume, namespace, name string) error {
	if pv.Spec.ClaimRef == nil || (pv.Spec.ClaimRef.Namespace == namespace && pv.Spec.ClaimRef.Name == name) {
		return nil
	}

	patch := client.MergeFrom(pv.DeepCopy())
	pv.Spec.ClaimRef = nil
	if err := r.client.Patch(ctx, pv, patch); err != nil {
		return fmt.Errorf("failed to clear claimRef on PersistentVolume %q: %w", pv.Name, err)
	}

	logger.Info("cleared stale claimRef on new PersistentVolume", "pv", pv.Name)

	return nil
}
