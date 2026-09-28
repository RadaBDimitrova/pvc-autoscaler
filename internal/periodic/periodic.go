// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package periodic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/healthcheck"
	"github.com/gardener/pvc-autoscaler/internal/metrics"
	metricssource "github.com/gardener/pvc-autoscaler/internal/metrics/source"
	"github.com/gardener/pvc-autoscaler/internal/recommender"
	"github.com/gardener/pvc-autoscaler/internal/resizer"
	"github.com/gardener/pvc-autoscaler/internal/status"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
	"github.com/gardener/pvc-autoscaler/internal/target/pvcfetcher"
	"github.com/gardener/pvc-autoscaler/internal/utils"
)

// UnknownUtilizationValue is the value which will be used when the free
// space/inodes utilization is unknown.
const UnknownUtilizationValue = "unknown"

// ErrNoMetricsSource is returned when the [Runner] is configured without a
// metrics source.
var ErrNoMetricsSource = errors.New("no metrics source provided")

// ErrVolumeModeIsNotFilesystem is an error which is returned if a target PVC
// for resizing is not using the Filesystem VolumeMode.
var ErrVolumeModeIsNotFilesystem = errors.New("volume mode is not filesystem")

// ErrStorageClassNotFound is an error which is returned when the storage class
// for a PVC is not found.
var ErrStorageClassNotFound = errors.New("no storage class found")

// ErrStorageClassDoesNotSupportExpansion is an error which is returned when an
// annotated PVC uses a storage class that does not support volume expansion.
var ErrStorageClassDoesNotSupportExpansion = errors.New("storage class does not support expansion")

// ErrNoClient is an error which is returned when the periodic [Runner] was
// configured without a Kubernetes API client.
var ErrNoClient = errors.New("no client provided")

// ErrNoPVCFetcher is an error which is returned when the periodic [Runner] was
// configured without a [pvcfetcher.Fetcher].
var ErrNoPVCFetcher = errors.New("no PersistentVolumeClaim fetcher provided")

// ErrPVCNotBound is returned when the PVC is not in the Bound phase.
var ErrPVCNotBound = errors.New("PersistentVolumeClaim is not bound")

// Runner is a [sigs.k8s.io/controller-runtime/pkg/manager.Runnable], which
// processes [v1alpha1.PersistentVolumeClaimAutoscaler] items on a regular basis
// and performs PVC resizing when thresholds are reached.
type Runner struct {
	client         client.Client
	interval       time.Duration
	metricsSource  metricssource.Source
	eventRecorder  record.EventRecorder
	pvcFetcher     pvcfetcher.Fetcher
	heartbeat      *healthcheck.Heartbeat
	autoscalerName string
}

var _ manager.Runnable = &Runner{}

// Option is a function which configures the [Runner].
type Option func(r *Runner)

// New creates a new [Runner] with the given options.
func New(opts ...Option) (*Runner, error) {
	r := &Runner{}
	for _, opt := range opts {
		opt(r)
	}

	if r.metricsSource == nil {
		return nil, ErrNoMetricsSource
	}

	if r.eventRecorder == nil {
		return nil, common.ErrNoEventRecorder
	}

	if r.client == nil {
		return nil, ErrNoClient
	}

	if r.pvcFetcher == nil {
		return nil, ErrNoPVCFetcher
	}

	return r, nil
}

// WithClient configures the [Runner] with the given client.
func WithClient(c client.Client) Option {
	opt := func(r *Runner) {
		r.client = c
	}

	return opt
}

// WithInterval configures the [Runner] with the given interval.
func WithInterval(interval time.Duration) Option {
	opt := func(r *Runner) {
		r.interval = interval
	}

	return opt
}

// WithMetricsSource configures the [Runner] to use the given source of metrics.
func WithMetricsSource(src metricssource.Source) Option {
	opt := func(r *Runner) {
		r.metricsSource = src
	}

	return opt
}

// WithEventRecorder configures the [Runner] to use the given event recorder.
func WithEventRecorder(recorder record.EventRecorder) Option {
	opt := func(r *Runner) {
		r.eventRecorder = recorder
	}

	return opt
}

// WithPVCFetcher configures the [Runner] to use the given [pvcfetcher.Fetcher].
func WithPVCFetcher(pvcFetcher pvcfetcher.Fetcher) Option {
	opt := func(r *Runner) {
		r.pvcFetcher = pvcFetcher
	}

	return opt
}

// WithHeartbeat configures the [Runner] to report activity for health checks.
func WithHeartbeat(h *healthcheck.Heartbeat) Option {
	opt := func(r *Runner) {
		r.heartbeat = h
	}

	return opt
}

// WithAutoscalerName configures the [Runner] to reconcile only
// [v1alpha1.PersistentVolumeClaimAutoscaler] objects whose spec.autoscalerName
// matches the given value. An empty string (the default) reconciles only PVCAs
// with an empty autoscalerName.
func WithAutoscalerName(name string) Option {
	opt := func(r *Runner) {
		r.autoscalerName = name
	}

	return opt
}

// Start implements the
// [sigs.k8s.io/controller-runtime/pkg/manager.Runnable] interface.
func (r *Runner) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	logger := log.FromContext(ctx, "controller", common.ControllerName)
	defer ticker.Stop()

	if r.heartbeat != nil {
		r.heartbeat.StartMonitoring()
	}

	for {
		select {
		case <-ticker.C:
			if err := r.reconcileAll(ctx); err != nil {
				logger.Error(err, "failed to reconcile persistentvolumeclaimautoscalers")
			}

			if r.heartbeat != nil {
				r.heartbeat.UpdateLastActivity()
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// reconcileAll processes all [v1alpha1.PersistentVolumeClaimAutoscaler]
// resources.
func (r *Runner) reconcileAll(ctx context.Context) error {
	var (
		logger   = log.FromContext(ctx, "controller", common.ControllerName)
		pvcaList v1alpha1.PersistentVolumeClaimAutoscalerList
	)

	if err := r.client.List(ctx, &pvcaList, client.MatchingFields{v1alpha1.AutoscalerNameIndexKey: r.autoscalerName}); err != nil {
		return err
	}

	// Nothing to do for now
	if len(pvcaList.Items) == 0 {
		return nil
	}

	metricsData, err := r.metricsSource.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get metrics: %w", err)
	}

	pvcaToPVCsMap, pvcToOwnersMap := r.fetchPVCsForPVCAs(ctx, logger, pvcaList.Items)

	for pvca, pvcs := range pvcaToPVCsMap {
		r.reconcilePVCA(ctx, logger, pvca, pvcs, pvcToOwnersMap, metricsData)
	}

	return nil
}

// fetchPVCsForPVCAs iterates over all [v1alpha1.PersistentVolumeClaimAutoscaler] items and retrieves all [corev1.PersistentVolumeClaim]
// that should be scaled by them. It returns a map of [v1alpha1.PersistentVolumeClaimAutoscaler] to [corev1.PersistentVolumeClaim] objects,
// and a "reverse" map of [corev1.PersistentVolumeClaim] object keys to the list of [v1alpha1.PersistentVolumeClaimAutoscaler]
// object keys that manage them, which is used to detect [corev1.PersistentVolumeClaim] claimed by more than one [v1alpha1.PersistentVolumeClaimAutoscaler].
func (r *Runner) fetchPVCsForPVCAs(ctx context.Context, logger logr.Logger, persistentVolumeClaimAutoscalers []v1alpha1.PersistentVolumeClaimAutoscaler) (
	map[*v1alpha1.PersistentVolumeClaimAutoscaler][]*corev1.PersistentVolumeClaim,
	map[string][]string,
) {
	var (
		pvcaToPVCsMap  = make(map[*v1alpha1.PersistentVolumeClaimAutoscaler][]*corev1.PersistentVolumeClaim, len(persistentVolumeClaimAutoscalers))
		pvcToOwnersMap = map[string][]string{}
	)

	for _, pvca := range persistentVolumeClaimAutoscalers {
		pvcaKey := client.ObjectKeyFromObject(&pvca)
		logger.V(2).Info("fetching persistentvolumeclaims for persistentvolumeclaimautoscaler", "autoscalerName", r.autoscalerName, "pvca", pvcaKey)

		persistentVolumeClaims, err := r.pvcFetcher.Fetch(ctx, &pvca)
		if err != nil {
			reason := conditions.ReasonPVCFetchError
			message := fmt.Sprintf("Failed to fetch PersistentVolumeClaims for PersistentVolumeClaimAutoscaler: %s", err.Error())

			if errors.Is(err, pvcfetcher.ErrNoPodsFound) || errors.Is(err, pvcfetcher.ErrNoPVCsFound) {
				logger.V(2).Info("no persistentvolumeclaims found for persistentvolumeclaimautoscaler", "pvca", pvcaKey, "reason", err.Error())
				reason = conditions.ReasonNoPVCsMatched
				message = fmt.Sprintf("No PersistentVolumeClaims found for PersistentVolumeClaimAutoscaler: %s", err.Error())
			} else {
				logger.Error(err, "failed to fetch persistentvolumeclaims for persistentvolumeclaimautoscaler", "pvca", pvcaKey)
			}

			recommendationsCondition := metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  reason,
				Message: message,
			}

			resizingCondition := metav1.Condition{Type: string(v1alpha1.ConditionTypeResizing)}
			if existing := meta.FindStatusCondition(pvca.Status.Conditions, resizingCondition.Type); existing != nil {
				resizingCondition = metav1.Condition{
					Type:    string(v1alpha1.ConditionTypeResizing),
					Status:  metav1.ConditionUnknown,
					Reason:  reason,
					Message: fmt.Sprintf("Resizing state is unknown: %s", message),
				}
			}

			if err := r.setStatus(ctx, &pvca, recommendationsCondition, resizingCondition, []v1alpha1.VolumeRecommendation{}); err != nil {
				logger.Error(err, "failed to update PVCA status", "pvca", pvcaKey)
			}

			continue
		}

		pvcaToPVCsMap[&pvca] = persistentVolumeClaims

		for _, pvc := range persistentVolumeClaims {
			key := client.ObjectKeyFromObject(pvc).String()
			pvcToOwnersMap[key] = append(pvcToOwnersMap[key], pvcaKey.String())
		}
	}

	return pvcaToPVCsMap, pvcToOwnersMap
}

// reconcilePVCA reconciles one [v1alpha1.PersistentVolumeClaimAutoscaler]
// and resizes [corev1.PersistentVolumeClaim] managed by it when thresholds are reached.
func (r *Runner) reconcilePVCA(
	ctx context.Context,
	logger logr.Logger,
	pvca *v1alpha1.PersistentVolumeClaimAutoscaler,
	pvcs []*corev1.PersistentVolumeClaim,
	pvcToOwnersMap map[string][]string,
	metricsData metricssource.Metrics,
) {
	logger = logger.WithValues("pvca", client.ObjectKeyFromObject(pvca))

	resizingConditions := &conditions.ResizingConditionAggregator{}
	recommendationConditions := &conditions.RecommendationsConditionAggregator{}

	pvcaStatus := status.New(pvca.Status.VolumeRecommendations, pvcs, pvca.Spec.VolumePolicies)

	for _, pvc := range pvcs {
		pvcObjKey := client.ObjectKeyFromObject(pvc)
		logger := logger.WithValues("pvc", pvcObjKey)

		if owners, ok := pvcToOwnersMap[pvcObjKey.String()]; ok && len(owners) > 1 {
			logger.Info("skipping persistentvolumeclaim because it is scaled by multiple persistentvolumeclaimautoscalers", "pvcas", strings.Join(owners, ", "))
			recommendationConditions.AddCondition(metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  conditions.ReasonAmbiguousPVCA,
				Message: fmt.Sprintf("PersistentVolumeClaim %s is scaled by multiple PersistentVolumeClaimAutoscalers: %s", pvcObjKey, strings.Join(owners, ", ")),
			})

			continue
		}

		// Get a fresh copy of the pvc object.
		if err := r.client.Get(ctx, pvcObjKey, pvc); err != nil {
			logger.Info("failed to get persistentvolumeclaim", "reason", err.Error())
			recommendationConditions.AddCondition(metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  conditions.ReasonPVCFetchError,
				Message: fmt.Sprintf("Failed to get PersistentVolumeClaim %s: %s", pvcObjKey, err.Error()),
			})

			continue
		}

		policy, err := utils.GetVolumePolicy(pvc.Name, pvca.Spec.VolumePolicies)
		if err != nil {
			logger.Info("skipping persistentvolumeclaim", "reason", err.Error())
			recommendationConditions.AddCondition(metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  conditions.ReasonRecommendationError,
				Message: fmt.Sprintf("%s: %s", pvcObjKey.Name, err.Error()),
			})

			continue
		}

		if policy == nil {
			logger.Info("skipping persistentvolumeclaim", "reason", "no matching volume policy")

			continue
		}

		if err := r.validatePVC(ctx, pvc, *policy); err != nil {
			logger.Info("skipping persistentvolumeclaim", "reason", err.Error())
			recommendationConditions.AddCondition(metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  conditions.ReasonRecommendationError,
				Message: fmt.Sprintf("%s: %s", pvcObjKey.Name, err.Error()),
			})

			continue
		}

		volumeRecommendation := pvcaStatus.GetOrCreate(pvc.Name)
		volumeRecommendation, err = observeVolumeRecommendation(volumeRecommendation, pvc, metricsData[pvcObjKey])
		if err != nil {
			logger.Info("skipping persistentvolumeclaim", "reason", err.Error())
			metrics.SkippedTotal.WithLabelValues(pvca.Namespace, pvca.Name, err.Error()).Inc()
			recommendationConditions.AddCondition(metav1.Condition{
				Type:    string(v1alpha1.ConditionTypeRecommendationAvailable),
				Status:  metav1.ConditionFalse,
				Reason:  conditions.ReasonMetricsFetchError,
				Message: fmt.Sprintf("%s: %s", pvcObjKey.Name, err.Error()),
			})

			continue
		}

		recommendation := recommender.RecommendResize(logger, r.eventRecorder, pvc, *policy, volumeRecommendation, resizingConditions)
		if recommendation.TargetSize != nil {
			volumeRecommendation, err = resizer.ResizePVC(ctx, logger, r.client, r.eventRecorder, pvc, recommendation, volumeRecommendation, resizingConditions)
			if err != nil {
				logger.Error(err, "failed to resize pvc")
			}
		}

		pvcaStatus.Set(pvc.Name, volumeRecommendation)
	}

	if err := r.setStatus(ctx, pvca, recommendationConditions.GetAggregatedCondition(), resizingConditions.GetAggregatedCondition(), pvcaStatus.Sorted()); err != nil {
		logger.Error(err, "failed to update PVCA status")
	}
}

// observeVolumeRecommendation extracts the observed volume metrics for the
// [corev1.PersistentVolumeClaim] and records them into the recommendation via
// [status.Observe]. It returns [common.ErrNoMetrics] when no metrics are
// available for the PVC yet.
func observeVolumeRecommendation(volumeRecommendation v1alpha1.VolumeRecommendation, pvc *corev1.PersistentVolumeClaim, volInfo *metricssource.VolumeInfo) (v1alpha1.VolumeRecommendation, error) {
	// No metrics found, nothing to do for now
	if volInfo == nil {
		return v1alpha1.VolumeRecommendation{}, common.ErrNoMetrics
	}

	usedSpace, err := volInfo.UsedSpacePercentage()
	if err != nil {
		return v1alpha1.VolumeRecommendation{}, fmt.Errorf("failed to get used space percentage: %w", err)
	}

	usedInodes, err := volInfo.UsedInodesPercentage()
	if err != nil {
		return v1alpha1.VolumeRecommendation{}, fmt.Errorf("failed to get used inodes percentage: %w", err)
	}

	return status.Observe(volumeRecommendation, pvc, usedSpace, usedInodes, volInfo.CapacityBytes)
}

// validatePVC checks whether the [corev1.PersistentVolumeClaim] is valid for
// reconciliation based on its current state and the associated volume policy.
func (r *Runner) validatePVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, policy v1alpha1.VolumePolicy) error {
	currStatusSize := pvc.Status.Capacity.Storage()
	if currStatusSize.IsZero() {
		return fmt.Errorf(".status.capacity.storage is invalid: %s", currStatusSize.String())
	}

	if policy.MaxCapacity.Value() < currStatusSize.Value() {
		return fmt.Errorf("max capacity (%s) cannot be less than current size (%s)", policy.MaxCapacity.String(), currStatusSize.String())
	}

	// We need a StorageClass with expansion support
	scName := ptr.Deref(pvc.Spec.StorageClassName, "")
	if scName == "" {
		return ErrStorageClassNotFound
	}

	var sc storagev1.StorageClass
	scKey := types.NamespacedName{Name: scName}
	if err := r.client.Get(ctx, scKey, &sc); err != nil {
		return err
	}

	if !ptr.Deref(sc.AllowVolumeExpansion, false) {
		return ErrStorageClassDoesNotSupportExpansion
	}

	// VolumeMode should be Filesystem
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return ErrVolumeModeIsNotFilesystem
	}

	// The PVC should be bound
	if pvc.Status.Phase != corev1.ClaimBound {
		return ErrPVCNotBound
	}

	return nil
}

// setStatus updates the status of the [v1alpha1.PersistentVolumeClaimAutoscaler]
// with the given conditions and the latest volume recommendations. For each
// condition, an empty Message is treated as a sentinel value: the condition is
// removed from the status by Type rather than set. The status is only patched
// if the recommendations, resizing conditions, or current stats have changed
// compared to the existing status.
func (r *Runner) setStatus(ctx context.Context, pvca *v1alpha1.PersistentVolumeClaimAutoscaler, recommendationsCondition metav1.Condition, resizingCondition metav1.Condition, volumeRecommendations []v1alpha1.VolumeRecommendation) error {
	original := pvca.DeepCopy()
	conditions := pvca.Status.Conditions
	if len(conditions) == 0 {
		conditions = make([]metav1.Condition, 0)
	}

	for _, condition := range []metav1.Condition{resizingCondition, recommendationsCondition} {
		if condition.Message == "" {
			meta.RemoveStatusCondition(&conditions, condition.Type)
		} else {
			meta.SetStatusCondition(&conditions, condition)
		}
	}

	pvca.Status.Conditions = conditions

	pvca.Status.VolumeRecommendations = volumeRecommendations

	if apiequality.Semantic.DeepEqual(original.Status, pvca.Status) {
		return nil
	}

	return r.client.Status().Patch(ctx, pvca, client.MergeFrom(original))
}
