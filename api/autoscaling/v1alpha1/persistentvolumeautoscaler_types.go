// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PersistentVolumeAutoscalerSpec defines the desired state of PersistentVolumeAutoscaler
type PersistentVolumeAutoscalerSpec struct {
	// TargetRef specifies the reference to the workload (StatefulSet, Deployment, etc.)
	TargetRef WorkloadReference `json:"targetRef"`

	// IncreaseBy specifies an increase by percentage value (e.g. 10%, 20%, etc.)
	// by which the Persistent Volume Claims will be resized.
	IncreaseBy string `json:"increaseBy,omitempty"`

	// Threshold specifies the threshold value in percentage (e.g. 10%, 20%, etc.)
	// Once the available capacity (free space) for the PVCs reaches or drops below
	// the specified threshold this will trigger a resize operation.
	Threshold string `json:"threshold,omitempty"`

	// MaxCapacity specifies the maximum capacity up to which PVCs are allowed
	// to be extended.
	MaxCapacity resource.Quantity `json:"maxCapacity,omitempty"`
}

// WorkloadReference defines a reference to a workload resource
type WorkloadReference struct {
	// Kind specifies the kind of the workload (StatefulSet, Deployment, etc.)
	Kind string `json:"kind"`

	// Name specifies the name of the workload
	Name string `json:"name"`

	// APIVersion specifies the API version of the workload
	APIVersion string `json:"apiVersion,omitempty"`
}

// PersistentVolumeAutoscalerStatus defines the observed state of PersistentVolumeAutoscaler
type PersistentVolumeAutoscalerStatus struct {
	// Phase indicates the overall phase of the PVA
	Phase string `json:"phase,omitempty"`

	// Message provides additional information about the current phase
	Message string `json:"message,omitempty"`

	// LastUpdate specifies the last time the status was updated
	LastUpdate metav1.Time `json:"lastUpdate,omitempty"`

	// TotalPVCs indicates the total number of PVCs managed by this PVA
	TotalPVCs int `json:"totalPVCs"`

	// ManagedPVCAs indicates the number of PVCA resources created for PVCs
	ManagedPVCAs int `json:"managedPVCAs"`

	// Conditions specifies the status conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pva
// +kubebuilder:printcolumn:name="Target Kind",type=string,JSONPath=`.spec.targetRef.kind`
// +kubebuilder:printcolumn:name="Target Name",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PVCs",type=integer,JSONPath=`.status.totalPVCs`
// +kubebuilder:printcolumn:name="PVCAs",type=integer,JSONPath=`.status.managedPVCAs`

// PersistentVolumeAutoscaler is the Schema for the persistentvolumeautoscalers API
type PersistentVolumeAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersistentVolumeAutoscalerSpec   `json:"spec,omitempty"`
	Status PersistentVolumeAutoscalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PersistentVolumeAutoscalerList contains a list of PersistentVolumeAutoscaler
type PersistentVolumeAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PersistentVolumeAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PersistentVolumeAutoscaler{}, &PersistentVolumeAutoscalerList{})
}
