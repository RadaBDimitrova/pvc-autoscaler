// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package periodic

import (
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/target/pvcfetcher"
	"github.com/gardener/pvc-autoscaler/internal/target/selectorfetcher"
)

// conditionAggregator collects the failures produced for a single recommendation
// condition type during one reconcile and reduces them into a single aggregated condition.
type conditionAggregator struct {
	conditionType string
	conditions    []metav1.Condition

	// defaultCondition is returned by getAggregatedCondition when no conditions were
	// aggregated. determineErrorStatusConditions can override it to report one specific
	// condition per type.
	defaultCondition metav1.Condition

	// problemPrefix heads the aggregated message.
	problemPrefix string

	// defaultReason is used as the aggregated reason when the aggregated
	// conditions carry more than one distinct reason.
	defaultReason string
}

// newRecommendationAggregator returns an aggregator for the RecommendationAvailable
// condition.
func newRecommendationAggregator() *conditionAggregator {
	return &conditionAggregator{
		conditionType: string(v1alpha1.ConditionTypeRecommendationAvailable),
		defaultCondition: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonRecommendationsProvided,
			Message: "Recommendations have been provided",
		},
		problemPrefix: "Recommendations could not be provided for some PersistentVolumeClaims:",
		defaultReason: ReasonRecommendationsNotProvided,
	}
}

// newPVCsMatchedAggregator returns an aggregator for the PVCsMatched condition. It
// collects the cases where no PersistentVolumeClaims could be matched.
func newPVCsMatchedAggregator() *conditionAggregator {
	return &conditionAggregator{
		conditionType: string(v1alpha1.ConditionTypePVCsMatched),
		defaultCondition: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypePVCsMatched),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonPVCsMatched,
			Message: "PersistentVolumeClaims were matched correctly or the target workload is scaled to zero",
		},
		problemPrefix: "No PersistentVolumeClaims were matched:",
		defaultReason: ReasonNoPVCsMatched,
	}
}

// newConfigSupportedAggregator returns an aggregator for the ConfigSupported
// condition. It collects the ways in which the PersistentVolumeClaimAutoscaler
// configuration is unsupported.
func newConfigSupportedAggregator() *conditionAggregator {
	return &conditionAggregator{
		conditionType: string(v1alpha1.ConditionTypeConfigSupported),
		defaultCondition: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeConfigSupported),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonConfigSupported,
			Message: "PersistentVolumeClaimAutoscaler configuration is supported",
		},
		problemPrefix: "PersistentVolumeClaimAutoscaler configuration is unsupported:",
		defaultReason: ReasonAmbiguousPVCA,
	}
}

// addCondition adds a condition to the aggregator. Only False conditions are
// aggregated, as these are the only ones that make a "problem" condition fire.
func (c *conditionAggregator) addCondition(condition metav1.Condition) {
	if condition.Status != metav1.ConditionFalse {
		return
	}
	c.conditions = append(c.conditions, condition)
}

// getAggregatedCondition reduces all aggregated failures into a single False
// condition. If no conditions were aggregated it returns defaultCondition.
func (c *conditionAggregator) getAggregatedCondition() metav1.Condition {
	if len(c.conditions) == 0 {
		return c.defaultCondition
	}

	return buildAggregatedCondition(c.conditionType, metav1.ConditionFalse, c.problemPrefix, c.defaultReason, c.conditions)
}

// resizingConditionAggregator collects the per-PVC Resizing conditions produced
// during one reconcile. Unlike the "problem" conditions it keeps conditions of
// any status and reduces them to a tri-state result: True when any resize is in
// progress, False when some could not be resized, Unknown otherwise.
type resizingConditionAggregator struct {
	conditions []metav1.Condition
}

// newResizingAggregator returns an aggregator for the Resizing condition.
func newResizingAggregator() *resizingConditionAggregator {
	return &resizingConditionAggregator{}
}

// addCondition adds a condition of any status to the aggregator.
func (c *resizingConditionAggregator) addCondition(condition metav1.Condition) {
	c.conditions = append(c.conditions, condition)
}

// getAggregatedCondition reduces all aggregated conditions into a single one. If
// no conditions were aggregated it returns the empty sentinel (no resize in
// progress, remove the condition). Otherwise the status is True if any resize is
// in progress, else False if any could not be resized, else Unknown.
func (c *resizingConditionAggregator) getAggregatedCondition() metav1.Condition {
	// When there are 0 conditions, return a condition with empty fields, except for the Resizing type.
	// This can be used in calling functions to determine whether the condition can be removed from the status of the PVCA.
	if len(c.conditions) == 0 {
		return emptyCondition(v1alpha1.ConditionTypeResizing)
	}

	var (
		prefix = "PersistentVolumeClaims cannot be resized:"
		status = metav1.ConditionUnknown
	)

	for _, condition := range c.conditions {
		switch {
		case condition.Status == metav1.ConditionTrue:
			status = metav1.ConditionTrue
			prefix = "PersistentVolumeClaims are being resized:"
		case status == metav1.ConditionUnknown && condition.Status == metav1.ConditionFalse:
			// Only set aggregated condition to False if it was previously Unknown and not yet set to True
			status = metav1.ConditionFalse
		}
	}

	return buildAggregatedCondition(string(v1alpha1.ConditionTypeResizing), status, prefix, ReasonReconcile, c.conditions)
}

// buildAggregatedCondition assembles a single condition of the given type and
// status. It heads the sorted per-condition messages with prefix and reuses the
// single shared reason when there is one, falling back to defaultReason otherwise.
func buildAggregatedCondition(conditionType string, status metav1.ConditionStatus, prefix, defaultReason string, conditions []metav1.Condition) metav1.Condition {
	var (
		reasons  = sets.New[string]()
		messages = make([]string, 0, len(conditions))
	)
	for _, condition := range conditions {
		messages = append(messages, condition.Message)
		reasons.Insert(condition.Reason)
	}

	slices.Sort(messages)
	message := prefix
	for _, conditionMessage := range messages {
		message = message + "\n- " + conditionMessage
	}

	reason := defaultReason
	if reasons.Len() == 1 {
		// The boolean return value is ignored as we are sure that there is at least one item
		// in the set
		reason, _ = reasons.PopAny()
	}

	return metav1.Condition{
		Type:    conditionType,
		Message: message,
		Reason:  reason,
		Status:  status,
	}
}

// newFalseCondition returns a condition with a False status and the given type,
// reason and message.
func newFalseCondition(conditionType v1alpha1.PersistentVolumeClaimAutoscalerConditionType, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    string(conditionType),
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
}

// emptyCondition returns a condition with only its type set. Its empty message is
// a sentinel understood by setStatus as "remove this condition from the status".
func emptyCondition(conditionType v1alpha1.PersistentVolumeClaimAutoscalerConditionType) metav1.Condition {
	return metav1.Condition{Type: string(conditionType)}
}

// statusConditions is the full set of conditions a single reconcile produces for
// a PersistentVolumeClaimAutoscaler.
type statusConditions struct {
	resizing                *resizingConditionAggregator
	pvcsMatched             *conditionAggregator
	configSupported         *conditionAggregator
	recommendationAvailable *conditionAggregator
}

// newStatusConditions returns a statusConditions whose aggregators start from
// their healthy defaults: recommendations provided, no resize in progress, PVCs
// matched and a supported configuration.
func newStatusConditions() statusConditions {
	return statusConditions{
		recommendationAvailable: newRecommendationAggregator(),
		resizing:                newResizingAggregator(),
		pvcsMatched:             newPVCsMatchedAggregator(),
		configSupported:         newConfigSupportedAggregator(),
	}
}

// list resolves every aggregator and returns the conditions in a stable order,
// ready to be passed to setStatus.
func (c statusConditions) list() []metav1.Condition {
	return []metav1.Condition{
		c.recommendationAvailable.getAggregatedCondition(),
		c.resizing.getAggregatedCondition(),
		c.pvcsMatched.getAggregatedCondition(),
		c.configSupported.getAggregatedCondition(),
	}
}

// determineErrorStatusConditions classifies an error returned by the [pvcfetcher.Fetcher]
// into the RecommendationAvailable, PVCsMatched and ConfigSupported conditions.
// It starts from the healthy defaults and overrides only the conditions relevant
// to the error.
func determineErrorStatusConditions(err error) statusConditions {
	conditions := newStatusConditions()

	switch {
	case errors.Is(err, pvcfetcher.ErrTargetScaledToZero):
		// The target workload is scaled to zero, so having no PersistentVolumeClaims
		// is expected.
		conditions.recommendationAvailable.defaultCondition = emptyCondition(v1alpha1.ConditionTypeRecommendationAvailable)
	case errors.Is(err, pvcfetcher.ErrNoPodsFound), errors.Is(err, pvcfetcher.ErrNoPVCsFound):
		message := fmt.Sprintf("No PersistentVolumeClaims found for PersistentVolumeClaimAutoscaler: %s", err.Error())
		conditions.pvcsMatched.defaultCondition = newFalseCondition(v1alpha1.ConditionTypePVCsMatched, ReasonNoPVCsMatched, message)
		conditions.recommendationAvailable.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeRecommendationAvailable, ReasonNoPVCsMatched, message)
	case errors.Is(err, selectorfetcher.ErrNoScaleSubresource):
		message := fmt.Sprintf("Target of PersistentVolumeClaimAutoscaler does not expose a scale subresource: %s", err.Error())
		conditions.configSupported.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeConfigSupported, ReasonNoScaleSubresource, message)
		conditions.recommendationAvailable.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeRecommendationAvailable, ReasonNoScaleSubresource, message)
	default:
		// A generic fetch error is not explained by any dedicated condition, so
		// RecommendationAvailable is the only place to surface it.
		message := fmt.Sprintf("Failed to fetch PersistentVolumeClaims for PersistentVolumeClaimAutoscaler: %s", err.Error())
		conditions.recommendationAvailable.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeRecommendationAvailable, ReasonPVCFetchError, message)
	}

	return conditions
}

// markResizingUnknown marks a previously reported Resizing condition as stale by
// adding an Unknown condition to the Resizing aggregator, but only when the PVCA
// already has a Resizing condition. The reason and message are taken from the
// first problem condition that explains why the PersistentVolumeClaims could not
// be fetched, so the stale marker echoes the underlying fetch error.
func (c statusConditions) markResizingUnknown(existingConditions []metav1.Condition) {
	if meta.FindStatusCondition(existingConditions, string(v1alpha1.ConditionTypeResizing)) == nil {
		return
	}

	reason, message := ReasonReconcile, "PersistentVolumeClaims could not be fetched"
	for _, aggregator := range []*conditionAggregator{c.recommendationAvailable, c.pvcsMatched, c.configSupported} {
		if condition := aggregator.getAggregatedCondition(); condition.Status == metav1.ConditionFalse {
			reason, message = condition.Reason, condition.Message

			break
		}
	}

	c.resizing.addCondition(metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeResizing),
		Status:  metav1.ConditionUnknown,
		Reason:  reason,
		Message: fmt.Sprintf("Resizing state is unknown: %s", message),
	})
}
