// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package conditions

import (
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
)

// Condition reasons for the RecommendationAvailable condition
const (
	// ReasonMetricsFetched indicates that metrics were successfully fetched and computed.
	ReasonMetricsFetched = "MetricsFetched"
	// ReasonMetricsFetchError indicates an error occurred while fetching metrics.
	ReasonMetricsFetchError = "MetricsFetchError"
	// ReasonPVCFetchError indicates an error occurred during fetching of PVCs.
	ReasonPVCFetchError = "PersistentVolumeClaimFetchError"
	// ReasonNoPVCsMatched indicates that pods were found but none had PVC volumes matching the policy.
	ReasonNoPVCsMatched = "NoPersistentVolumeClaimsMatched"
	// ReasonAmbiguousPVCA indicates that a PVC is autoscaled by multiple PVCAs.
	ReasonAmbiguousPVCA = "AmbiguousPersistentVolumeClaimAutoscaler"
	// ReasonRecommendationError indicates an error occurred during recommendation computation.
	ReasonRecommendationError = "RecommendationError"
	// ReasonRecommendationsProvided indicates that all recommendations have been computed and added to the status.
	ReasonRecommendationsProvided = "RecommendationsProvided"
	// ReasonRecommendationsNotProvided is a generic reason that not all recommendations have been computed and added to the status.
	ReasonRecommendationsNotProvided = "RecommendationsNotProvided"
	// ReasonReconcile condition reason for the Resizing condition.
	ReasonReconcile = "Reconcile"
	// ReasonPVCResizeCooldown indicates that the PVC resize is in cooldown period.
	ReasonPVCResizeCooldown = "PersistentVolumeClaimResizeCooldown"
)

// RecommendationsConditionAggregator is a condition aggregator for the RecommendationAvailable condition of the PVCA.
type RecommendationsConditionAggregator struct {
	conditions []metav1.Condition
}

// addCondition adds a condition to the aggregator. Only conditions with false status are aggregated.
func (c *RecommendationsConditionAggregator) AddCondition(condition metav1.Condition) {
	if condition.Status == metav1.ConditionFalse {
		c.conditions = append(c.conditions, condition)
	}
}

// getAggregatedCondition aggregates all conditions into one. If there are no false conditions, it
// returns a true condition indicating that recommendations have been provided
func (c *RecommendationsConditionAggregator) GetAggregatedCondition() metav1.Condition {
	var (
		status          = metav1.ConditionTrue
		failureReasons  = sets.New[string]()
		failureMessages = make([]string, 0, len(c.conditions))
	)

	for _, condition := range c.conditions {
		if condition.Status == metav1.ConditionFalse {
			status = metav1.ConditionFalse
			failureReasons.Insert(condition.Reason)
			failureMessages = append(failureMessages, condition.Message)
		}
	}

	if status == metav1.ConditionTrue {
		return metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonRecommendationsProvided,
			Message: "Recommendations have been provided",
		}
	}

	slices.Sort(failureMessages)
	message := "Recommendations could not be provided for some PersistentVolumeClaims:"
	for _, failure := range failureMessages {
		message = message + "\n- " + failure
	}

	reason := ReasonRecommendationsNotProvided
	if failureReasons.Len() == 1 {
		// The boolean return value is ignored as we are sure that there is at least one item
		// in the set
		reason, _ = failureReasons.PopAny()
	}

	return metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
		Reason:  reason,
		Message: message,
		Status:  status,
	}
}

// ResizingConditionAggregator is a condition aggregator for the Resizing condition of the PVCA.
type ResizingConditionAggregator struct {
	conditions []metav1.Condition
}

// AddCondition adds a condition to the aggregator
func (c *ResizingConditionAggregator) AddCondition(condition metav1.Condition) {
	c.conditions = append(c.conditions, condition)
}

// GetAggregatedCondition aggregates all conditions into one. If there are no conditions, it returns an empty condition with the
// Resizing type. If there is one condition with status true, the aggregated condition's status is also true to indicate that there
// is a resize in progress. If there are only conditions with Unknown status, the aggregated condition will also have Unknown status.
func (c *ResizingConditionAggregator) GetAggregatedCondition() metav1.Condition {
	// When there are 0 conditions, return a condition with empty fields, except for the Resizing type.
	// This can be used in calling functions to determine whether the condition can be removed from the status of the PVCA.
	if len(c.conditions) == 0 {
		return metav1.Condition{Type: string(v1alpha1.ConditionTypeResizing)}
	}

	var (
		message           = "PersistentVolumeClaims cannot be resized:"
		status            = metav1.ConditionUnknown
		reasons           = sets.New[string]()
		conditionMessages = make([]string, 0, len(c.conditions))
	)

	for _, condition := range c.conditions {
		conditionMessages = append(conditionMessages, condition.Message)
		reasons.Insert(condition.Reason)
		if condition.Status == metav1.ConditionTrue {
			message = "PersistentVolumeClaims are being resized:"
			status = metav1.ConditionTrue
		} else if status == metav1.ConditionUnknown && condition.Status == metav1.ConditionFalse {
			// Only set aggregated condition to False if it was previously Unknown and not yet set to True
			status = metav1.ConditionFalse
		}
	}

	slices.Sort(conditionMessages)
	for _, conditionMessage := range conditionMessages {
		message = message + "\n- " + conditionMessage
	}

	reason := ReasonReconcile
	if reasons.Len() == 1 {
		// The boolean return value is ignored as we are sure that there is at least one item
		// in the set
		reason, _ = reasons.PopAny()
	}

	return metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeResizing),
		Message: message,
		Reason:  reason,
		Status:  status,
	}
}
