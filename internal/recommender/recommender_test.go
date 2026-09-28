// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package recommender

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
	testutils "github.com/gardener/pvc-autoscaler/test/utils"
)

// createPolicy builds a VolumePolicy with the default scale-up rules and the given max capacity.
func createPolicy(maxCapacity string) v1alpha1.VolumePolicy {
	return v1alpha1.VolumePolicy{
		MaxCapacity: resource.MustParse(maxCapacity),
		ScaleUp: ptr.To(v1alpha1.ScalingRules{
			UtilizationThresholdPercent: ptr.To(common.DefaultThresholdPercent),
			StepPercent:                 ptr.To(common.DefaultStepPercent),
			MinStepAbsolute:             ptr.To(resource.MustParse("1Gi")),
		}),
	}
}

// makeRecommendation builds a VolumeRecommendation with the given utilization percentages.
func makeRecommendation(usedSpacePercent, usedInodesPercent int) v1alpha1.VolumeRecommendation {
	return v1alpha1.VolumeRecommendation{
		Name: "test-pvc",
		Current: v1alpha1.CurrentVolumeStatus{
			UsedSpacePercent:  ptr.To(usedSpacePercent),
			UsedInodesPercent: ptr.To(usedInodesPercent),
		},
	}
}

var _ = Describe("Recommender", func() {
	var (
		ctx       context.Context
		k8sClient client.Client
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()
	})

	Describe("#ScalingReason", func() {
		DescribeTable("determines whether and why a PVC should be resized",
			func(specSize, maxCapacity string, usedSpacePercent, usedInodesPercent int, expected string) {
				pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", specSize, nil, nil)
				Expect(err).NotTo(HaveOccurred())
				policy := createPolicy(maxCapacity)
				volumeRecommendation := makeRecommendation(usedSpacePercent, usedInodesPercent)

				Expect(ScalingReason(pvc, policy, volumeRecommendation)).To(Equal(expected))
			},
			Entry("no resize when neither threshold is reached", "1Gi", "10Gi", 50, 50, ""),
			Entry("storage threshold reached with headroom", "1Gi", "10Gi", 92, 0, common.ScalingReasonStorageThreshold),
			Entry("inodes threshold reached with headroom", "1Gi", "10Gi", 0, 91, common.ScalingReasonInodesThreshold),
			Entry("space takes precedence over inodes", "1Gi", "10Gi", 92, 91, common.ScalingReasonStorageThreshold),
			Entry("max capacity reached when already at the limit", "3Gi", "3Gi", 92, 0, common.ScalingReasonMaxCapacity),
			Entry("max capacity reached when within one resolution of the limit", "3Gi", "3500Mi", 92, 0, common.ScalingReasonMaxCapacity),
			Entry("threshold (not max) when more than one resolution of headroom remains", "1Gi", "3Gi", 92, 0, common.ScalingReasonStorageThreshold),
		)
	})

	Describe("#RecommendResize", func() {
		var (
			eventRecorder *record.FakeRecorder
			resizingConds *conditions.ResizingConditionAggregator
		)

		BeforeEach(func() {
			eventRecorder = record.NewFakeRecorder(128)
			resizingConds = &conditions.ResizingConditionAggregator{}
		})

		It("recommends a target size and records the used-space event", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			policy := createPolicy("10Gi")
			volumeRecommendation := makeRecommendation(92, 0)

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).NotTo(BeNil())
			Expect(recommendation.TargetSize.String()).To(Equal("2Gi"))
			Expect(recommendation.ClampedToMaxCapacity).To(BeFalse())
			Expect(recommendation.ScalingReason).To(Equal(common.ScalingReasonStorageThreshold))

			event := <-eventRecorder.Events
			Expect(event).To(Equal(`Warning UsedSpaceThresholdReached used space (92%) exceeds the configured threshold (80%)`))
		})

		It("records the used-inodes event when only inodes exceed the threshold", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			policy := createPolicy("10Gi")
			volumeRecommendation := makeRecommendation(0, 91)

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).NotTo(BeNil())
			Expect(recommendation.ScalingReason).To(Equal(common.ScalingReasonInodesThreshold))

			event := <-eventRecorder.Events
			Expect(event).To(Equal(`Warning UsedInodesThresholdReached used inodes (91%) exceeds the configured threshold (80%)`))
		})

		It("does not recommend when no threshold is reached", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			policy := createPolicy("10Gi")
			volumeRecommendation := makeRecommendation(50, 50)

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).To(BeNil())
			Expect(eventRecorder.Events).NotTo(Receive())
		})

		It("clamps the target size to the max capacity", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "2Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			policy := createPolicy("3Gi")
			volumeRecommendation := makeRecommendation(92, 0)

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).NotTo(BeNil())
			Expect(recommendation.TargetSize.String()).To(Equal("3Gi"))
			Expect(recommendation.ClampedToMaxCapacity).To(BeTrue())
		})

		It("does not recommend and records the max-capacity event when at max capacity", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "3Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			policy := createPolicy("3Gi")
			volumeRecommendation := makeRecommendation(92, 0)

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).To(BeNil())
			Expect(<-eventRecorder.Events).To(ContainSubstring("MaxCapacityReached"))
			Expect(resizingConds.GetAggregatedCondition().Message).To(ContainSubstring("max capacity reached"))
		})

		It("does not recommend while the cooldown period has not elapsed", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			policy := createPolicy("10Gi")
			policy.ScaleUp.CooldownDuration = ptr.To(metav1.Duration{Duration: time.Hour})
			volumeRecommendation := makeRecommendation(92, 0)
			volumeRecommendation.LastResizeTime = ptr.To(metav1.Now())

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).To(BeNil())
			Expect(resizingConds.GetAggregatedCondition().Reason).To(Equal(conditions.ReasonPVCResizeCooldown))
		})

		It("does not recommend while a resize is already in progress", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{
				{Type: corev1.PersistentVolumeClaimResizing, Status: corev1.ConditionTrue},
			}
			policy := createPolicy("10Gi")
			volumeRecommendation := makeRecommendation(92, 0)

			recommendation := RecommendResize(logr.Discard(), eventRecorder, pvc, policy, volumeRecommendation, resizingConds)

			Expect(recommendation.TargetSize).To(BeNil())
			Expect(resizingConds.GetAggregatedCondition().Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Describe("#isResizeInProgress", func() {
		DescribeTable("detects whether a resize is in progress",
			func(
				pvcConditionType *corev1.PersistentVolumeClaimConditionType,
				prevSizeAnnotation *string,
				reason string,
				expectedLogSubstring string,
				expectedMessageRegex string,
				expectInProgress bool,
				expectedConditionStatus metav1.ConditionStatus,
			) {
				pvc, err := testutils.CreatePVC(ctx, k8sClient, "test-pvc", "1Gi", nil, nil)
				Expect(err).NotTo(HaveOccurred())
				if pvcConditionType != nil {
					pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{
						{Type: *pvcConditionType, Status: corev1.ConditionTrue},
					}
				}
				if prevSizeAnnotation != nil {
					pvc.Annotations = map[string]string{common.AnnotationPreviousSize: *prevSizeAnnotation}
				}

				var buf strings.Builder
				w := io.MultiWriter(GinkgoWriter, &buf)
				logger := zap.New(zap.WriteTo(w))

				aggregator := &conditions.ResizingConditionAggregator{}
				inProgress := isResizeInProgress(logger, pvc, reason, aggregator)
				Expect(inProgress).To(Equal(expectInProgress))

				if !expectInProgress {
					Expect(aggregator.GetAggregatedCondition().Message).To(BeEmpty())

					return
				}

				if expectedLogSubstring != "" {
					Expect(buf.String()).To(ContainSubstring(expectedLogSubstring))
				}
				Expect(aggregator.GetAggregatedCondition()).To(And(
					HaveField("Type", string(v1alpha1.ConditionTypeResizing)),
					HaveField("Status", expectedConditionStatus),
					HaveField("Reason", conditions.ReasonReconcile),
					HaveField("Message", MatchRegexp(expectedMessageRegex)),
				))
			},
			Entry("should detect resize has been started",
				ptr.To(corev1.PersistentVolumeClaimResizing),
				nil,
				common.ScalingReasonStorageThreshold,
				"resize has been started",
				`storage threshold.*resize has been started`,
				true,
				metav1.ConditionTrue,
			),
			Entry("should detect filesystem resize is pending",
				ptr.To(corev1.PersistentVolumeClaimFileSystemResizePending),
				nil,
				common.ScalingReasonInodesThreshold,
				"filesystem resize is pending",
				`passing inodes threshold.*file system resize is pending`,
				true,
				metav1.ConditionTrue,
			),
			Entry("should detect volume is being modified",
				ptr.To(corev1.PersistentVolumeClaimVolumeModifyingVolume),
				nil,
				common.ScalingReasonStorageThreshold,
				"volume is being modified",
				`storage threshold.*volume is being modified`,
				true,
				metav1.ConditionTrue,
			),
			Entry("should detect pvc is still being resized when annotation matches status",
				nil,
				ptr.To("1Gi"),
				common.ScalingReasonInodesThreshold,
				"persistent volume claim is still being resized",
				`passing inodes threshold.*persistent volume claim is still being resized`,
				true,
				metav1.ConditionTrue,
			),
			Entry("should return false when annotation is missing",
				nil,
				nil,
				common.ScalingReasonStorageThreshold,
				"",
				"",
				false,
				metav1.ConditionTrue,
			),
			Entry("should return false when annotation no longer matches status (resize completed)",
				nil,
				ptr.To("512Mi"),
				common.ScalingReasonStorageThreshold,
				"",
				"",
				false,
				metav1.ConditionTrue,
			),
			Entry("should return true on unparseable annotation and surface the parse error in the aggregated condition",
				nil,
				ptr.To("not-a-quantity"),
				common.ScalingReasonStorageThreshold,
				"",
				`could not parse pvc.autoscaling.gardener.cloud/prev-size annotation with value not-a-quantity`,
				true,
				metav1.ConditionUnknown,
			),
		)
	})
})
