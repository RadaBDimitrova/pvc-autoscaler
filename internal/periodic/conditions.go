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

// conditionAggregator collects the False conditions produced for one condition
// type during a reconcile and reduces them to a single aggregated condition.
type conditionAggregator struct {
	conditions       []metav1.Condition
	defaultCondition metav1.Condition
	problemPrefix    string
	problemReason    string
}

// newRecommendationAggregator returns an aggregator for the RecommendationAvailable
// condition.
func newRecommendationAggregator() *conditionAggregator {
	return &conditionAggregator{
		defaultCondition: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonRecommendationsProvided,
			Message: "Recommendations have been provided",
		},
		problemPrefix: "Recommendations could not be provided for some PersistentVolumeClaims:",
		problemReason: ReasonRecommendationsNotProvided,
	}
}

// newPVCsMatchedAggregator returns an aggregator for the PVCsMatched condition. It
// collects the cases where no PersistentVolumeClaims could be matched.
func newPVCsMatchedAggregator() *conditionAggregator {
	return &conditionAggregator{
		defaultCondition: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypePVCsMatched),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonPVCsMatched,
			Message: "PersistentVolumeClaims were matched correctly or the target workload is scaled to zero",
		},
		problemPrefix: "No PersistentVolumeClaims were matched:",
		problemReason: ReasonNoPVCsMatched,
	}
}

// newConfigSupportedAggregator returns an aggregator for the ConfigSupported
// condition, collecting the ways the configuration is unsupported.
func newConfigSupportedAggregator() *conditionAggregator {
	return &conditionAggregator{
		defaultCondition: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeConfigSupported),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonConfigSupported,
			Message: "PersistentVolumeClaimAutoscaler configuration is supported",
		},
		problemPrefix: "PersistentVolumeClaimAutoscaler configuration is unsupported:",
		problemReason: ReasonAmbiguousPVCA,
	}
}

// addCondition adds a condition to the aggregator. Only False conditions are
// aggregated, as these are the only ones that make a "problem" condition fire.
func (c *conditionAggregator) addCondition(condition metav1.Condition) {
	if condition.Status == metav1.ConditionFalse {
		c.conditions = append(c.conditions, condition)
	}
}

// getAggregatedCondition reduces all aggregated failures into a single False
// condition. If no conditions were aggregated it returns defaultCondition.
func (c *conditionAggregator) getAggregatedCondition() metav1.Condition {
	if len(c.conditions) == 0 {
		return c.defaultCondition
	}

	return buildAggregatedCondition(c.defaultCondition.Type, metav1.ConditionFalse, c.problemPrefix, c.problemReason, c.conditions)
}

// resizingConditionAggregator collects the per-PVC Resizing conditions (any status) and
// reduces them to a tri-state: True if any is resizing, else False if any failed, else Unknown.
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

// getAggregatedCondition reduces the conditions to one: the empty sentinel if none,
// else True if any is resizing, else False if any failed, else Unknown.
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

// buildAggregatedCondition builds one condition of the given type and status, heading
// the sorted messages with prefix and using their shared reason, or defaultReason if they differ.
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

// newStatusConditions returns a statusConditions with every aggregator at its healthy
// default.
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

// determineErrorStatusConditions classifies a [pvcfetcher.Fetcher] error into the condition
// set and returns the reason and message explaining why the fetch failed.
func determineErrorStatusConditions(err error) (conditions statusConditions, reason, message string) {
	conditions = newStatusConditions()
	reason, message = ReasonReconcile, "PersistentVolumeClaims could not be fetched"

	switch {
	case errors.Is(err, pvcfetcher.ErrTargetScaledToZero):
		// The target workload is scaled to zero, so having no PersistentVolumeClaims
		// is expected.
		conditions.recommendationAvailable.defaultCondition = emptyCondition(v1alpha1.ConditionTypeRecommendationAvailable)
	case errors.Is(err, pvcfetcher.ErrNoPodsFound), errors.Is(err, pvcfetcher.ErrNoPVCsFound):
		reason, message = ReasonNoPVCsMatched, fmt.Sprintf("No PersistentVolumeClaims found for PersistentVolumeClaimAutoscaler: %s", err.Error())
		conditions.pvcsMatched.defaultCondition = newFalseCondition(v1alpha1.ConditionTypePVCsMatched, reason, message)
		conditions.recommendationAvailable.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeRecommendationAvailable, reason, message)
	case errors.Is(err, selectorfetcher.ErrNoScaleSubresource):
		reason, message = ReasonNoScaleSubresource, fmt.Sprintf("Target of PersistentVolumeClaimAutoscaler does not expose a scale subresource: %s", err.Error())
		conditions.configSupported.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeConfigSupported, reason, message)
		conditions.recommendationAvailable.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeRecommendationAvailable, reason, message)
	default:
		reason, message = ReasonPVCFetchError, fmt.Sprintf("Failed to fetch PersistentVolumeClaims for PersistentVolumeClaimAutoscaler: %s", err.Error())
		conditions.recommendationAvailable.defaultCondition = newFalseCondition(v1alpha1.ConditionTypeRecommendationAvailable, reason, message)
	}

	return conditions, reason, message
}

// markResizingUnknown adds an Unknown Resizing condition with the caller's reason and
// message, when the PVCA already reports a Resizing condition to mark stale.
func (c statusConditions) markResizingUnknown(existingConditions []metav1.Condition, reason, message string) {
	if meta.FindStatusCondition(existingConditions, string(v1alpha1.ConditionTypeResizing)) == nil {
		return
	}

	c.resizing.addCondition(metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeResizing),
		Status:  metav1.ConditionUnknown,
		Reason:  reason,
		Message: fmt.Sprintf("Resizing state is unknown: %s", message),
	})
}
