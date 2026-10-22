// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package downscale implements the controller that executes PVCDownscaleTask objects.
//
// Downscaling a PersistentVolumeClaim cannot be done in place, because a PVC's requested
// storage can only grow. The only safe way to shrink a volume is to provision a new,
// smaller volume, copy the data across, and swap the new volume in under the original PVC
// name. The data copy is delegated to a [pvc.Copier].
package downscale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	scaleclient "k8s.io/client-go/scale"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/pvc"
)

// ControllerName is the name of the downscale controller.
const ControllerName = "downscale"

// TempPVCSuffix is appended to the original PVC name to form the temporary PVC name.
const TempPVCSuffix = "-temp-pvca"

// CopyTypeRsync is the only supported data-copy strategy, backed by [pvc.RsyncCopier].
const CopyTypeRsync = "rsync"

// ConditionTypeReady is the condition type tracking whether a downscale task has completed.
const ConditionTypeReady = "Ready"

// Condition reasons for the Ready condition, each describing an observed situation.
const (
	ReasonScalingDown  = "ScalingDownTarget"
	ReasonProvisioning = "ProvisioningNewVolume"
	ReasonCopying      = "CopyingData"
	ReasonSwapping     = "SwappingVolume"
	ReasonRebinding    = "RebindingVolume"
	ReasonCompleted    = "Completed"
	ReasonFailed       = "Failed"
)

// ErrNoClient is returned when the [Reconciler] is configured without a client.
var ErrNoClient = errors.New("no client provided")

// ErrNoScaleClient is returned when the [Reconciler] is configured without a scale client.
var ErrNoScaleClient = errors.New("no scale client provided")

// ErrNoRESTMapper is returned when the [Reconciler] is configured without a REST mapper.
var ErrNoRESTMapper = errors.New("no REST mapper provided")

// ErrNoCopier is returned when the [Reconciler] is configured without a copier.
var ErrNoCopier = errors.New("no copier provided")

// Reconciler executes [v1alpha1.PVCDownscaleTask] objects.
type Reconciler struct {
	client         client.Client
	eventRecorder  record.EventRecorder
	autoscalerName string
	scaleClient    scaleclient.ScalesGetter
	restMapper     apimeta.RESTMapper
	copier         pvc.Copier
}

var _ reconcile.Reconciler = (*Reconciler)(nil)

// Option is a function which configures the [Reconciler].
type Option func(*Reconciler)

// New creates a new [Reconciler] with the given options.
func New(opts ...Option) (*Reconciler, error) {
	r := &Reconciler{}
	for _, opt := range opts {
		opt(r)
	}

	if r.client == nil {
		return nil, ErrNoClient
	}

	if r.eventRecorder == nil {
		return nil, common.ErrNoEventRecorder
	}

	if r.scaleClient == nil {
		return nil, ErrNoScaleClient
	}

	if r.restMapper == nil {
		return nil, ErrNoRESTMapper
	}

	if r.copier == nil {
		return nil, ErrNoCopier
	}

	return r, nil
}

// WithClient configures the [Reconciler] with the given client.
func WithClient(c client.Client) Option {
	return func(r *Reconciler) {
		r.client = c
	}
}

// WithEventRecorder configures the [Reconciler] with the given event recorder.
func WithEventRecorder(recorder record.EventRecorder) Option {
	return func(r *Reconciler) {
		r.eventRecorder = recorder
	}
}

// WithAutoscalerName configures the [Reconciler] to act only on [v1alpha1.PVCDownscaleTask]
// objects whose spec.autoscalerName matches the given value.
func WithAutoscalerName(name string) Option {
	return func(r *Reconciler) {
		r.autoscalerName = name
	}
}

// WithScaleClient configures the [Reconciler] with the given scale client.
func WithScaleClient(sc scaleclient.ScalesGetter) Option {
	return func(r *Reconciler) {
		r.scaleClient = sc
	}
}

// WithRESTMapper configures the [Reconciler] with the given REST mapper.
func WithRESTMapper(rm apimeta.RESTMapper) Option {
	return func(r *Reconciler) {
		r.restMapper = rm
	}
}

// WithCopier configures the [Reconciler] with the given data [pvc.Copier].
func WithCopier(c pvc.Copier) Option {
	return func(r *Reconciler) {
		r.copier = c
	}
}

// SetupWithManager wires the [Reconciler] into the given manager. It watches PVCDownscaleTask
// SetupWithManager wires the [Reconciler] into the given manager. It watches only
// PVCDownscaleTask objects belonging to this autoscaler instance; the asynchronous steps
// (volume binding, PVC deletion, copy completion) are polled via RequeueAfter rather than by
// watching the PersistentVolumeClaims, PersistentVolumes and copy Jobs involved.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&v1alpha1.PVCDownscaleTask{}, builder.WithPredicates(r.autoscalerNamePredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// autoscalerNamePredicate limits reconciliation to PVCDownscaleTask objects whose
// spec.autoscalerName matches this autoscaler instance.
func (r *Reconciler) autoscalerNamePredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		task, ok := obj.(*v1alpha1.PVCDownscaleTask)
		if !ok {
			return false
		}

		return task.Spec.AutoscalerName == r.autoscalerName
	})
}

// Reconcile drives a single PVCDownscaleTask toward completion. It is level-triggered:
// it reads the live state and progresses as far as that state allows, then requeues after a
// fixed interval to re-poll when it must wait for another controller (binding, deletion, the
// copy). Each step is idempotent, so a transient failure is simply retried on the next poll.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx, "controller", ControllerName, "task", req.NamespacedName)

	task := &v1alpha1.PVCDownscaleTask{}
	if err := r.client.Get(ctx, req.NamespacedName, task); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}

		return reconcile.Result{}, fmt.Errorf("failed to get PVCDownscaleTask: %w", err)
	}

	if task.DeletionTimestamp != nil {
		return reconcile.Result{}, nil
	}

	if task.Spec.AutoscalerName != r.autoscalerName {
		logger.Info("PVCDownscaleTask does not belong to this autoscaler instance, skipping")

		return reconcile.Result{}, nil
	}

	if apimeta.IsStatusConditionTrue(task.Status.Conditions, ConditionTypeReady) {
		return reconcile.Result{}, nil
	}

	if ct := task.Spec.CopyType; ct != "" && ct != CopyTypeRsync {
		return r.fail(ctx, logger, task, fmt.Errorf("unsupported copyType %q, only %q is supported", ct, CopyTypeRsync))
	}

	return r.reconcile(ctx, logger, task)
}

// reconcile contains the level-based downscale logic. See the package doc for the model.
func (r *Reconciler) reconcile(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask) (reconcile.Result, error) {
	ns := task.Namespace
	pvcName := task.Spec.PVCName
	tempName := pvcName + TempPVCSuffix
	newVolume := task.Status.NewVolumeName

	original, err := r.getPVC(ctx, ns, pvcName)
	if err != nil {
		return reconcile.Result{}, err
	}
	temp, err := r.getPVC(ctx, ns, tempName)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Final stretch: the original-named PVC is already backed by the new volume at the target
	// size. Ensure the new PV is bound to it, restore the target's replicas and clean up.
	if original != nil && newVolume != "" && r.isDownscaled(original, task) {
		pv, err := r.getPV(ctx, newVolume)
		if err != nil {
			return reconcile.Result{}, err
		}
		if pv == nil {
			return r.fail(ctx, logger, task, fmt.Errorf("new PersistentVolume %q not found", newVolume))
		}

		if !boundTo(pv, ns, pvcName) {
			if err := r.rebind(ctx, logger, pv, ns, pvcName); err != nil {
				return reconcile.Result{}, err
			}

			return r.wait(ctx, task, ReasonRebinding, "waiting for the new volume to bind to the recreated PVC")
		}

		if err := r.restoreReplicas(ctx, logger, task); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.deleteCopy(ctx, task); err != nil {
			return reconcile.Result{}, err
		}

		return r.complete(ctx, logger, task)
	}

	// Downscale in progress. The original PVC must exist unless we have already committed the
	// new volume and are mid-swap (where the original may be temporarily absent).
	if original == nil && newVolume == "" {
		return r.fail(ctx, logger, task, fmt.Errorf("PersistentVolumeClaim %s/%s not found", ns, pvcName))
	}

	// 1. Record the target's original replica count once, then keep it scaled to zero so the
	//    volume stays detached for the whole operation.
	if err := r.ensureScaledDown(ctx, logger, task); err != nil {
		return reconcile.Result{}, err
	}

	// 2. Provision and commit the new, smaller volume before any destructive step: create the
	//    temporary PVC, wait for it to bind, retain its PV, and record the PV name. Guarded by
	//    NewVolumeName being empty, so a temp PVC deleted during the later swap is never
	//    mistaken for "needs (re)creation".
	if newVolume == "" {
		if temp == nil {
			if err := r.createTempPVC(ctx, logger, task, original); err != nil {
				return reconcile.Result{}, err
			}

			return r.wait(ctx, task, ReasonProvisioning, "waiting for the temporary PVC to be provisioned")
		}
		if temp.Spec.VolumeName == "" {
			return r.wait(ctx, task, ReasonProvisioning, "waiting for the temporary PVC to bind")
		}
		if err := r.retainPV(ctx, logger, temp.Spec.VolumeName); err != nil {
			return reconcile.Result{}, err
		}
		if err := r.recordNewVolume(ctx, task, temp.Spec.VolumeName); err != nil {
			return reconcile.Result{}, err
		}
		newVolume = temp.Spec.VolumeName
	}

	// 3. Copy the data from the old volume to the new one, while both PVCs still exist (before
	//    the swap). The copy runs as a Job; its completion re-triggers reconciliation through
	//    the Job watch. Once it has succeeded, the swap below deletes the temp PVC, and a later
	//    reconcile observes temp gone and skips straight to the swap.
	if temp != nil && original != nil {
		status, err := r.copier.EnsureCopy(ctx, original, temp)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to ensure data copy: %w", err)
		}

		switch status {
		case pvc.StatusFailed:
			return r.fail(ctx, logger, task, fmt.Errorf("data copy from %s to %s failed", original.Name, temp.Name))
		case pvc.StatusSucceeded:
			// Data is on the new volume; fall through to the swap.
		default:
			return r.wait(ctx, task, ReasonCopying, "waiting for the data copy to complete")
		}
	}

	// 4. Swap the volume in: delete the temporary PVC, then the original (old) PVC, then
	//    recreate the original-named PVC bound to the new volume. Each deletion is awaited by
	//    observation on the next reconcile.
	if temp != nil {
		if err := r.deletePVC(ctx, temp); err != nil {
			return reconcile.Result{}, err
		}

		return r.wait(ctx, task, ReasonSwapping, "waiting for the temporary PVC to be deleted")
	}
	if original != nil {
		if err := r.deletePVC(ctx, original); err != nil {
			return reconcile.Result{}, err
		}

		return r.wait(ctx, task, ReasonSwapping, "waiting for the original PVC to be deleted")
	}

	if err := r.recreatePVC(ctx, logger, task, newVolume); err != nil {
		return reconcile.Result{}, err
	}

	return r.wait(ctx, task, ReasonSwapping, "recreated PVC, waiting for it to bind to the new volume")
}

// isDownscaled reports whether the given PVC is the finished result of the downscale: backed
// by the recorded new volume and requesting the target size.
func (r *Reconciler) isDownscaled(claim *corev1.PersistentVolumeClaim, task *v1alpha1.PVCDownscaleTask) bool {
	if claim.Spec.VolumeName != task.Status.NewVolumeName {
		return false
	}

	requested := claim.Spec.Resources.Requests.Storage()

	return requested != nil && requested.Cmp(task.Spec.TargetSize) == 0
}

// getScale fetches the scale subresource for the task's target, returning the scale and the
// GroupResource that served it (needed for a subsequent update).
func (r *Reconciler) getScale(ctx context.Context, namespace string, ref autoscalingv1.CrossVersionObjectReference) (*autoscalingv1.Scale, schema.GroupResource, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, schema.GroupResource{}, fmt.Errorf("invalid API version in target reference: %w", err)
	}

	gk := schema.GroupKind{Group: gv.Group, Kind: ref.Kind}
	mappings, err := r.restMapper.RESTMappings(gk)
	if err != nil {
		return nil, schema.GroupResource{}, fmt.Errorf("unable to determine resource for scale target reference: %w", err)
	}

	errs := make([]error, 0, len(mappings))
	for _, mapping := range mappings {
		gr := mapping.Resource.GroupResource()
		scale, err := r.scaleClient.Scales(namespace).Get(ctx, gr, ref.Name, metav1.GetOptions{})
		if err == nil {
			return scale, gr, nil
		}

		errs = append(errs, err)
	}

	return nil, schema.GroupResource{}, errors.Join(errs...)
}

// setReplicas sets the replica count on the task's target via the scale subresource.
func (r *Reconciler) setReplicas(ctx context.Context, namespace string, ref autoscalingv1.CrossVersionObjectReference, replicas int32) error {
	scale, gr, err := r.getScale(ctx, namespace, ref)
	if err != nil {
		return err
	}

	if scale.Spec.Replicas == replicas {
		return nil
	}

	scale.Spec.Replicas = replicas
	if _, err := r.scaleClient.Scales(namespace).Update(ctx, gr, scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to update scale of target %s: %w", ref.String(), err)
	}

	return nil
}

// wait records the given in-progress reason on the Ready condition and requeues after a
// fixed interval to re-poll the external state (binding, deletion, copy progress), since the
// controller does not watch those objects.
func (r *Reconciler) wait(ctx context.Context, task *v1alpha1.PVCDownscaleTask, reason, message string) (reconcile.Result, error) {
	return reconcile.Result{RequeueAfter: 5 * time.Second}, r.setReady(ctx, task, metav1.ConditionFalse, reason, message)
}

// complete marks the task's Ready condition true and emits an event.
func (r *Reconciler) complete(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask) (reconcile.Result, error) {
	logger.Info("downscale completed", "pvc", task.Spec.PVCName, "size", task.Spec.TargetSize.String())
	r.eventRecorder.Eventf(task, corev1.EventTypeNormal, ReasonCompleted, "downscaled PVC %s to %s", task.Spec.PVCName, task.Spec.TargetSize.String())

	return reconcile.Result{}, r.setReady(ctx, task, metav1.ConditionTrue, ReasonCompleted, "downscale completed")
}

// fail records a failure on the task's Ready condition and requeues after a fixed interval,
// so a failure whose cause is transient is retried by the (idempotent) reconcile on the next
// poll. A genuinely terminal cause (e.g. an unsupported copyType) simply keeps re-failing.
func (r *Reconciler) fail(ctx context.Context, logger logr.Logger, task *v1alpha1.PVCDownscaleTask, cause error) (reconcile.Result, error) {
	logger.Error(cause, "downscale failed")
	r.eventRecorder.Event(task, corev1.EventTypeWarning, ReasonFailed, cause.Error())

	return reconcile.Result{RequeueAfter: 5 * time.Second}, r.setReady(ctx, task, metav1.ConditionFalse, ReasonFailed, cause.Error())
}

// setReady sets the Ready condition on the task's status.
func (r *Reconciler) setReady(ctx context.Context, task *v1alpha1.PVCDownscaleTask, status metav1.ConditionStatus, reason, message string) error {
	patch := client.MergeFrom(task.DeepCopy())
	apimeta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:    ConditionTypeReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	if err := r.client.Status().Patch(ctx, task, patch); err != nil {
		return fmt.Errorf("failed to patch PVCDownscaleTask status: %w", err)
	}

	return nil
}
