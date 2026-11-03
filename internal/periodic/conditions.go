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

// recommendationsConditionAggregator is a condition aggregator for the RecommendationAvailable condition of the PVCA.
type recommendationsConditionAggregator struct {
	conditions []metav1.Condition
}

// recommendationsUnavailableCondition returns a RecommendationAvailable
// condition with a False status and the given reason and message.
func recommendationsUnavailableCondition(reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
}

// configUnsupportedProblemCondition returns a ConfigUnsupported condition with a
// True status and the given reason and message.
func configUnsupportedProblemCondition(reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeConfigUnsupported),
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	}
}

// emptyResizingCondition returns a Resizing condition with only its type set.
// Its empty message is a sentinel understood by setStatus as "remove this
// condition from the status".
func emptyResizingCondition() metav1.Condition {
	return metav1.Condition{Type: string(v1alpha1.ConditionTypeResizing)}
}

// emptyRecommendationCondition returns a RecommendationAvailable condition with
// only its type set. Its empty message is a sentinel understood by setStatus as
// "remove this condition from the status". It is used when the state is already
// fully explained by a dedicated condition (NoPVCsMatched or ConfigUnsupported)
// or the target is scaled to zero, so RecommendationAvailable carries no
// additional signal.
func emptyRecommendationCondition() metav1.Condition {
	return metav1.Condition{Type: string(v1alpha1.ConditionTypeRecommendationAvailable)}
}

// addCondition adds a condition to the aggregator. Only conditions with false status are aggregated.
func (c *recommendationsConditionAggregator) addCondition(condition metav1.Condition) {
	if condition.Status == metav1.ConditionFalse {
		c.conditions = append(c.conditions, condition)
	}
}

// getAggregatedCondition aggregates all conditions into one. If there are no false conditions, it
// returns a true condition indicating that recommendations have been provided
func (c *recommendationsConditionAggregator) getAggregatedCondition() metav1.Condition {
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

	return recommendationsUnavailableCondition(reason, message)
}

// resizingConditionAggregator is a condition aggregator for the Resizing condition of the PVCA.
type resizingConditionAggregator struct {
	conditions []metav1.Condition
}

// addCondition adds a condition to the aggregator
func (c *resizingConditionAggregator) addCondition(condition metav1.Condition) {
	c.conditions = append(c.conditions, condition)
}

// getAggregatedCondition aggregates all conditions into one. If there are no conditions, it returns an empty condition with the
// Resizing type. If there is one condition with status true, the aggregated condition's status is also true to indicate that there
// is a resize in progress. If there are only conditions with Unknown status, the aggregated condition will also have Unknown status.
func (c *resizingConditionAggregator) getAggregatedCondition() metav1.Condition {
	// When there are 0 conditions, return a condition with empty fields, except for the Resizing type.
	// This can be used in calling functions to determine whether the condition can be removed from the status of the PVCA.
	if len(c.conditions) == 0 {
		return emptyResizingCondition()
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

// statusConditions is the full set of conditions a single reconcile produces for
// a PersistentVolumeClaimAutoscaler. Build it with newStatusConditions, which
// initializes every condition to its healthy default, then override only the
// ones a problem applies to. Persisting the whole set via list() at once
// guarantees no condition is accidentally left in a stale state.
type statusConditions struct {
	recommendationAvailable metav1.Condition
	resizing                metav1.Condition
	noPVCsMatched           metav1.Condition
	configUnsupported       metav1.Condition
}

// newStatusConditions returns a statusConditions with every condition set to its
// healthy default: recommendations provided, no resize in progress, PVCs
// matched and a supported configuration.
func newStatusConditions() statusConditions {
	return statusConditions{
		recommendationAvailable: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonRecommendationsProvided,
			Message: "Recommendations have been provided",
		},
		resizing: emptyResizingCondition(),
		noPVCsMatched: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeNoPVCsMatched),
			Status:  metav1.ConditionFalse,
			Reason:  ReasonPVCsMatched,
			Message: "PersistentVolumeClaims were matched correctly or the target workload is scaled to zero",
		},
		configUnsupported: metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeConfigUnsupported),
			Status:  metav1.ConditionFalse,
			Reason:  ReasonConfigSupported,
			Message: "PersistentVolumeClaimAutoscaler configuration is supported",
		},
	}
}

// list returns all conditions in a stable order, ready to be passed to
// setStatus.
func (c statusConditions) list() []metav1.Condition {
	return []metav1.Condition{
		c.recommendationAvailable,
		c.resizing,
		c.noPVCsMatched,
		c.configUnsupported,
	}
}

// fetchErrorConditions classifies an error returned by the [pvcfetcher.Fetcher]
// into the RecommendationAvailable, NoPVCsMatched and ConfigUnsupported
// conditions. The returned set starts from the healthy defaults and flips only
// the conditions relevant to the error.
func fetchErrorConditions(err error) statusConditions {
	conditions := newStatusConditions()

	switch {
	case errors.Is(err, pvcfetcher.ErrTargetScaledToZero):
		// The target workload is scaled to zero, so having no PersistentVolumeClaims
		// is expected.
		conditions.recommendationAvailable = emptyRecommendationCondition()
	case errors.Is(err, pvcfetcher.ErrNoPodsFound), errors.Is(err, pvcfetcher.ErrNoPVCsFound):
		message := fmt.Sprintf("No PersistentVolumeClaims found for PersistentVolumeClaimAutoscaler: %s", err.Error())
		conditions.noPVCsMatched = metav1.Condition{
			Type:    string(v1alpha1.ConditionTypeNoPVCsMatched),
			Status:  metav1.ConditionTrue,
			Reason:  ReasonNoPVCsMatched,
			Message: message,
		}
		conditions.recommendationAvailable = emptyRecommendationCondition()
	case errors.Is(err, selectorfetcher.ErrNoScaleSubresource):
		// ConfigUnsupported fully explains the state, so RecommendationAvailable is
		// removed rather than redundantly set to False.
		message := fmt.Sprintf("Target of PersistentVolumeClaimAutoscaler does not expose a scale subresource: %s", err.Error())
		conditions.configUnsupported = configUnsupportedProblemCondition(ReasonNoScaleSubresource, message)
		conditions.recommendationAvailable = emptyRecommendationCondition()
	default:
		// A generic fetch error is not explained by any dedicated condition, so
		// RecommendationAvailable is the only place to surface it.
		message := fmt.Sprintf("Failed to fetch PersistentVolumeClaims for PersistentVolumeClaimAutoscaler: %s", err.Error())
		conditions.recommendationAvailable = recommendationsUnavailableCondition(ReasonPVCFetchError, message)
	}

	return conditions
}

// unknownResizingCondition returns a Resizing condition with an Unknown status
// when a Resizing condition already exists in the given conditions, so that a
// previously reported resize state is marked stale rather than silently kept
// after the PersistentVolumeClaims could not be fetched.
func unknownResizingCondition(existingConditions []metav1.Condition) metav1.Condition {
	if meta.FindStatusCondition(existingConditions, string(v1alpha1.ConditionTypeResizing)) == nil {
		return emptyResizingCondition()
	}

	return metav1.Condition{
		Type:    string(v1alpha1.ConditionTypeResizing),
		Status:  metav1.ConditionUnknown,
		Reason:  ReasonReconcile,
		Message: "Resizing state is unknown: PersistentVolumeClaims could not be fetched",
	}
}
