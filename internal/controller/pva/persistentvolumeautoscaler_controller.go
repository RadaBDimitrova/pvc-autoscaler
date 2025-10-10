// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package pva

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
)

// PersistentVolumeAutoscalerReconciler reconciles a PersistentVolumeAutoscaler object
type PersistentVolumeAutoscalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeautoscalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeautoscalers/finalizers,verbs=update
//+kubebuilder:rbac:groups=autoscaling.gardener.cloud,resources=persistentvolumeclaimautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets;deployments;daemonsets;replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims;pods,verbs=get;list;watch

// Reconcile implements the reconciliation logic for PersistentVolumeAutoscaler
func (r *PersistentVolumeAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PersistentVolumeAutoscaler instance
	pva := &v1alpha1.PersistentVolumeAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, pva); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PersistentVolumeAutoscaler resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PersistentVolumeAutoscaler")
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling PersistentVolumeAutoscaler",
		"targetKind", pva.Spec.TargetRef.Kind,
		"targetName", pva.Spec.TargetRef.Name)

	// Discover PVCs for the target workload
	pvcs, err := r.discoverTargetPVCs(ctx, pva)
	if err != nil {
		logger.Error(err, "Failed to discover target PVCs")
		return r.updateStatusError(ctx, pva, fmt.Sprintf("Failed to discover PVCs: %v", err))
	}

	logger.Info("Discovered PVCs", "count", len(pvcs))

	// Ensure PVCA resources exist for each PVC
	managedPVCAs := 0
	for _, pvc := range pvcs {
		if err := r.ensurePVCAForPVC(ctx, pva, pvc); err != nil {
			logger.Error(err, "Failed to ensure PVCA for PVC", "pvc", pvc.Name)
			return r.updateStatusError(ctx, pva, fmt.Sprintf("Failed to manage PVCA for PVC %s: %v", pvc.Name, err))
		}
		managedPVCAs++
	}

	// Clean up orphaned PVCAs (PVCAs that no longer have corresponding PVCs)
	if err := r.cleanupOrphanedPVCAs(ctx, pva, pvcs); err != nil {
		logger.Error(err, "Failed to cleanup orphaned PVCAs")
		return r.updateStatusError(ctx, pva, fmt.Sprintf("Failed to cleanup orphaned PVCAs: %v", err))
	}

	// Update status
	if err := r.updateStatusSuccess(ctx, pva, len(pvcs), managedPVCAs); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled PersistentVolumeAutoscaler")
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// discoverTargetPVCs discovers PVCs generically by finding pods owned by the target controller
func (r *PersistentVolumeAutoscalerReconciler) discoverTargetPVCs(ctx context.Context, pva *v1alpha1.PersistentVolumeAutoscaler) ([]*corev1.PersistentVolumeClaim, error) {
	logger := log.FromContext(ctx)

	// Step 1: Get all pods in the namespace
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(pva.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", pva.Namespace, err)
	}

	// Step 2: Find pods that are ultimately owned by our target controller
	var targetPods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]

		if r.isOwnedByTarget(ctx, pod, pva.Spec.TargetRef, pva.Namespace) {
			targetPods = append(targetPods, pod)
		}
	}

	if len(targetPods) == 0 {
		logger.Info("No pods found for target controller", "targetRef", pva.Spec.TargetRef)
		return nil, nil
	}

	// Step 3: Extract unique PVCs from the pods
	pvcMap := make(map[string]*corev1.PersistentVolumeClaim)

	for _, pod := range targetPods {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				pvcName := volume.PersistentVolumeClaim.ClaimName

				// Skip if we already have this PVC
				if _, exists := pvcMap[pvcName]; exists {
					continue
				}

				// Fetch the PVC
				pvc := &corev1.PersistentVolumeClaim{}
				if err := r.Get(ctx, types.NamespacedName{
					Namespace: pva.Namespace,
					Name:      pvcName,
				}, pvc); err != nil {
					if errors.IsNotFound(err) {
						logger.Info("PVC referenced by pod not found", "pvc", pvcName, "pod", pod.Name)
						continue
					}
					return nil, fmt.Errorf("failed to get PVC %s: %w", pvcName, err)
				}

				pvcMap[pvcName] = pvc
			}
		}
	}

	// Convert map to slice
	var pvcs []*corev1.PersistentVolumeClaim
	for _, pvc := range pvcMap {
		pvcs = append(pvcs, pvc)
	}

	logger.Info("Discovered PVCs for target controller",
		"targetRef", pva.Spec.TargetRef,
		"pods", len(targetPods),
		"pvcs", len(pvcs))

	return pvcs, nil
}

// ensurePVCAForPVC ensures a PVCA resource exists for the given PVC
func (r *PersistentVolumeAutoscalerReconciler) ensurePVCAForPVC(ctx context.Context, pva *v1alpha1.PersistentVolumeAutoscaler, pvc *corev1.PersistentVolumeClaim) error {
	pvcaName := fmt.Sprintf("%s-%s", pva.Name, pvc.Name)
	pvca := &v1alpha1.PersistentVolumeClaimAutoscaler{}

	// Create desired PVCA spec
	desired := &v1alpha1.PersistentVolumeClaimAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcaName,
			Namespace: pva.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: pva.APIVersion,
					Kind:       pva.Kind,
					Name:       pva.Name,
					UID:        pva.UID,
					Controller: ptr.To(true),
				},
			},
			Labels: map[string]string{
				"pva.gardener.cloud/managed-by": pva.Name,
				"pva.gardener.cloud/pvc":        pvc.Name,
			},
		},
		Spec: v1alpha1.PersistentVolumeClaimAutoscalerSpec{
			IncreaseBy:  getValueOrDefault(pva.Spec.IncreaseBy, common.DefaultIncreaseByValue),
			Threshold:   getValueOrDefault(pva.Spec.Threshold, common.DefaultThresholdValue),
			MaxCapacity: pva.Spec.MaxCapacity,
			ScaleTargetRef: corev1.LocalObjectReference{
				Name: pvc.Name,
			},
		},
	}

	// Check if PVCA already exists
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: pva.Namespace,
		Name:      pvcaName,
	}, pvca); err != nil {
		if errors.IsNotFound(err) {
			// Create new PVCA
			return r.Create(ctx, desired)
		}
		return err
	}

	// Update existing PVCA if needed
	if needsPVCAUpdate(pvca, desired) {
		pvca.Spec = desired.Spec
		pvca.Labels = desired.Labels
		return r.Update(ctx, pvca)
	}

	return nil
}

// cleanupOrphanedPVCAs removes PVCA resources that no longer have corresponding PVCs
func (r *PersistentVolumeAutoscalerReconciler) cleanupOrphanedPVCAs(ctx context.Context, pva *v1alpha1.PersistentVolumeAutoscaler, currentPVCs []*corev1.PersistentVolumeClaim) error {
	// Get all PVCAs managed by this PVA
	pvcaList := &v1alpha1.PersistentVolumeClaimAutoscalerList{}
	if err := r.List(ctx, pvcaList,
		client.InNamespace(pva.Namespace),
		client.MatchingLabels{"pva.gardener.cloud/managed-by": pva.Name},
	); err != nil {
		return err
	}

	// Create a set of current PVC names for quick lookup
	currentPVCNames := make(map[string]bool)
	for _, pvc := range currentPVCs {
		currentPVCNames[pvc.Name] = true
	}

	// Delete PVCAs that don't have corresponding PVCs
	for _, pvca := range pvcaList.Items {
		pvcName := pvca.Labels["pva.gardener.cloud/pvc"]
		if pvcName == "" || !currentPVCNames[pvcName] {
			if err := r.Delete(ctx, &pvca); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("failed to delete orphaned PVCA %s: %w", pvca.Name, err)
			}
		}
	}

	return nil
}

// updateStatusSuccess updates the PVA status with success information
func (r *PersistentVolumeAutoscalerReconciler) updateStatusSuccess(ctx context.Context, pva *v1alpha1.PersistentVolumeAutoscaler, totalPVCs, managedPVCAs int) error {
	patch := client.MergeFrom(pva.DeepCopy())
	pva.Status.Phase = "Ready"
	pva.Status.Message = fmt.Sprintf("Managing %d PVCs with %d PVCA resources", totalPVCs, managedPVCAs)
	pva.Status.TotalPVCs = totalPVCs
	pva.Status.ManagedPVCAs = managedPVCAs
	pva.Status.LastUpdate = metav1.NewTime(time.Now())

	return r.Status().Patch(ctx, pva, patch)
}

// updateStatusError updates the PVA status with error information
func (r *PersistentVolumeAutoscalerReconciler) updateStatusError(ctx context.Context, pva *v1alpha1.PersistentVolumeAutoscaler, message string) (ctrl.Result, error) {
	patch := client.MergeFrom(pva.DeepCopy())
	pva.Status.Phase = "Error"
	pva.Status.Message = message
	pva.Status.LastUpdate = metav1.NewTime(time.Now())

	if err := r.Status().Patch(ctx, pva, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// getValueOrDefault returns the value if not empty, otherwise returns the default
func getValueOrDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

// needsPVCAUpdate checks if the PVCA needs to be updated
func needsPVCAUpdate(current, desired *v1alpha1.PersistentVolumeClaimAutoscaler) bool {
	return current.Spec.IncreaseBy != desired.Spec.IncreaseBy ||
		current.Spec.Threshold != desired.Spec.Threshold ||
		!current.Spec.MaxCapacity.Equal(desired.Spec.MaxCapacity) ||
		current.Spec.ScaleTargetRef.Name != desired.Spec.ScaleTargetRef.Name
}

// isOwnedByTarget checks if a pod is ultimately owned by the target controller
// by walking up the ownership chain to find the topmost controller
func (r *PersistentVolumeAutoscalerReconciler) isOwnedByTarget(ctx context.Context, pod *corev1.Pod, targetRef v1alpha1.WorkloadReference, namespace string) bool {
	visited := make(map[string]bool)
	return r.isOwnerInChain(ctx, pod, targetRef, namespace, visited)
}

// isOwnerInChain recursively walks the ownership chain to find if targetRef is an owner
func (r *PersistentVolumeAutoscalerReconciler) isOwnerInChain(ctx context.Context, obj client.Object, targetRef v1alpha1.WorkloadReference, namespace string, visited map[string]bool) bool {
	// Prevent infinite loops in ownership chains
	objKey := fmt.Sprintf("%s/%s/%s", obj.GetObjectKind().GroupVersionKind().String(), obj.GetNamespace(), obj.GetName())
	if visited[objKey] {
		return false
	}
	visited[objKey] = true

	// Check if this object directly matches our target
	if r.matchesTargetRef(obj, targetRef, namespace) {
		return true
	}

	// Walk up the ownership chain
	for _, ownerRef := range obj.GetOwnerReferences() {
		if !ptr.Deref(ownerRef.Controller, false) {
			continue // Skip non-controller references
		}

		// Create a typed object based on the owner's kind
		var owner client.Object
		switch ownerRef.Kind {
		case "StatefulSet":
			owner = &appsv1.StatefulSet{}
		case "Deployment":
			owner = &appsv1.Deployment{}
		case "DaemonSet":
			owner = &appsv1.DaemonSet{}
		case "ReplicaSet":
			owner = &appsv1.ReplicaSet{}
		default:
			// For unknown kinds, skip
			continue
		}

		// Fetch the owner object
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      ownerRef.Name,
		}, owner); err != nil {
			continue // Skip if we can't fetch the owner
		}

		// Set the GVK since the client doesn't always populate it
		gvk := schema.GroupVersionKind{
			Group:   "",
			Version: "",
			Kind:    ownerRef.Kind,
		}

		// Parse the API version to get group and version
		if ownerRef.APIVersion == "apps/v1" {
			gvk.Group = "apps"
			gvk.Version = "v1"
		} else if ownerRef.APIVersion == "v1" {
			gvk.Group = ""
			gvk.Version = "v1"
		}

		owner.GetObjectKind().SetGroupVersionKind(gvk)

		// Recursively check if this owner matches our target or has an owner that does
		if r.isOwnerInChain(ctx, owner, targetRef, namespace, visited) {
			return true
		}
	}

	return false
}

// matchesTargetRef checks if an object matches the target reference
func (r *PersistentVolumeAutoscalerReconciler) matchesTargetRef(obj client.Object, targetRef v1alpha1.WorkloadReference, namespace string) bool {
	gvk := obj.GetObjectKind().GroupVersionKind()

	// Match by kind and name
	if gvk.Kind != targetRef.Kind || obj.GetName() != targetRef.Name {
		return false
	}

	// Match by namespace (objects should be in the same namespace as PVA)
	if obj.GetNamespace() != namespace {
		return false
	}

	// Match by API version if specified
	if targetRef.APIVersion != "" {
		if gvk.GroupVersion().String() != targetRef.APIVersion {
			return false
		}
	}

	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *PersistentVolumeAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PersistentVolumeAutoscaler{}).
		Owns(&v1alpha1.PersistentVolumeClaimAutoscaler{}).
		Complete(r)
}
