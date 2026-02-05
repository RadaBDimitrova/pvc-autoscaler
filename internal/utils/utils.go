// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"errors"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Condition types
const (
	// ConditionTypeRecommendationAvailable represents the type of condition
	// indicating whether metrics have been successfully fetched and computed.
	ConditionTypeRecommendationAvailable = "RecommendationAvailable"

	// ConditionTypeResizing represents the type of condition indicating the
	// status of the resize operation.
	ConditionTypeResizing = "Resizing"
)

// Condition reasons
const (
	// ReasonMetricsFetched indicates that metrics were successfully fetched and computed.
	ReasonMetricsFetched = "MetricsFetched"

	// ReasonReconcile indicates that a reconcile is in progress.
	ReasonReconcile = "Reconcile"

	// ReasonStaleMetrics indicates that stale metrics were detected.
	ReasonStaleMetrics = "StaleMetrics"

	// ReasonMetricsFetchError indicates an error occurred while fetching metrics.
	ReasonMetricsFetchError = "MetricsFetchError"
)

// ErrBadPercentageValue is an error which is returned when attempting to parse
// a bad percentage value.
var ErrBadPercentageValue = errors.New("bad percentage value")

// ParsePercentage parses a string value, which represents percentage, e.g. 10%.
func ParsePercentage(s string) (float64, error) {
	s = strings.TrimSpace(s)

	if !strings.HasSuffix(s, "%") {
		return 0.0, ErrBadPercentageValue
	}
	s = strings.TrimRight(s, "%")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return val, ErrBadPercentageValue
	}

	if val < 0.0 || val > 100.0 {
		return val, ErrBadPercentageValue
	}

	return val, nil
}

// IsPersistentVolumeClaimConditionTrue is a predicate which tests whether the
// given PersistentVolumeClaim object's status condition is set to [corev1.ConditionTrue].
func IsPersistentVolumeClaimConditionTrue(obj *corev1.PersistentVolumeClaim, conditionType corev1.PersistentVolumeClaimConditionType) bool {
	return IsPersistentVolumeClaimConditionPresentAndEqual(obj, conditionType, corev1.ConditionTrue)
}

// IsPersistentVolumeClaimConditionPresentAndEqual is a predicate which returns
// whether the condition of the given type is equal to the given status.
func IsPersistentVolumeClaimConditionPresentAndEqual(obj *corev1.PersistentVolumeClaim, conditionType corev1.PersistentVolumeClaimConditionType, status corev1.ConditionStatus) bool {
	for _, condition := range obj.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}

	return false
}
