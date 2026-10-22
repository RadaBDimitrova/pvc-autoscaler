// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pvcdt
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=`.spec.pvcName`
// +kubebuilder:printcolumn:name="TargetSize",type=string,JSONPath=`.spec.targetSize`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`

// PVCDownscaleTask is the Schema for the pvcdownscaletasks API. It represents a single
// downscale (shrink) operation for a PersistentVolumeClaim: a smaller volume is provisioned,
// its data is copied across, and it is swapped in under the original PVC name.
type PVCDownscaleTask struct {
	metav1.TypeMeta   `json:",inline"`            // nolint:revive
	metav1.ObjectMeta `json:"metadata,omitempty"` // nolint:revive

	Spec   PVCDownscaleTaskSpec   `json:"spec,omitempty"`
	Status PVCDownscaleTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PVCDownscaleTaskList contains a list of PVCDownscaleTask
type PVCDownscaleTaskList struct {
	metav1.TypeMeta `json:",inline"` // nolint:revive
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PVCDownscaleTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PVCDownscaleTask{}, &PVCDownscaleTaskList{})
}

// PVCDownscaleTaskSpec defines the desired state of a PVCDownscaleTask.
type PVCDownscaleTaskSpec struct {
	// PVCName is the name of the PersistentVolumeClaim to shrink. It lives in the same
	// namespace as this task.
	PVCName string `json:"pvcName"`

	// TargetSize is the new (smaller) size to which the PVC will be shrunk.
	TargetSize resource.Quantity `json:"targetSize"`

	// CopyType selects the strategy used to copy data from the old volume to the new one.
	// Currently only "rsync" is supported, which is also the default.
	// +kubebuilder:default=rsync
	// +kubebuilder:validation:Enum=rsync
	// +optional
	CopyType string `json:"copyType,omitempty"`

	// TargetRef references the workload controller (e.g. StatefulSet) that owns the PVC.
	// It is scaled down to zero for the duration of the downscale and scaled back up
	// afterwards.
	TargetRef autoscalingv1.CrossVersionObjectReference `json:"targetRef"`

	// AutoscalerName optionally assigns this task to a named autoscaler instance, mirroring
	// PersistentVolumeClaimAutoscaler.spec.autoscalerName. Only an autoscaler whose
	// --autoscaler-name matches acts on this task.
	// +kubebuilder:default=""
	// +optional
	AutoscalerName string `json:"autoscalerName,omitempty"`
}

// PVCDownscaleTaskStatus defines the observed state of a PVCDownscaleTask.
//
// The status deliberately records only facts that cannot be re-derived by observing the
// cluster once the downscale is under way. Progress is NOT tracked here; it is recomputed
// from the live objects on every reconcile.
type PVCDownscaleTaskStatus struct {
	// OriginalReplicas records the target controller's replica count before it was scaled
	// down to zero, so it can be restored afterwards. Once the target is scaled down this
	// value can no longer be observed from the cluster, which is why it is persisted.
	// +optional
	OriginalReplicas *int32 `json:"originalReplicas,omitempty"`

	// NewVolumeName is the name of the PersistentVolume provisioned for the smaller volume.
	// It is recorded once the temporary PVC binds, because after the PVC swap the binding
	// can no longer be observed from the temporary PVC (which is deleted).
	// +optional
	NewVolumeName string `json:"newVolumeName,omitempty"`

	// Conditions specifies the status conditions. The "Ready" condition is True once the
	// downscale has completed; while in progress or on failure it is False with a reason
	// describing the observed situation.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}
