// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package resizer_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/recommender"
	"github.com/gardener/pvc-autoscaler/internal/resizer"
	"github.com/gardener/pvc-autoscaler/internal/status/conditions"
)

// makePVC builds an in-memory PVC with the given spec and status storage sizes.
func makePVC(specSize, statusSize string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(specSize)},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(statusSize)},
		},
	}
}

var _ = Describe("Resizer", func() {
	var (
		ctx           context.Context
		eventRecorder *record.FakeRecorder
		resizingConds *conditions.ResizingConditionAggregator
	)

	BeforeEach(func() {
		ctx = context.Background()
		eventRecorder = record.NewFakeRecorder(128)
		resizingConds = &conditions.ResizingConditionAggregator{}
	})

	// newClient builds a fake client seeded with the given objects.
	newClient := func(objects ...client.Object) client.Client {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1.AddToScheme(scheme)).To(Succeed())

		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	}

	Describe("#ResizePVC", func() {
		It("does not patch the PVC when the resize strategy is Off", func() {
			pvc := makePVC("1Gi", "1Gi")
			fakeClient := newClient(pvc)
			targetSize := resource.MustParse("2Gi")
			recommendation := recommender.Recommendation{
				TargetSize:     &targetSize,
				ScalingReason:  common.ScalingReasonStorageThreshold,
				ResizeStrategy: v1alpha1.OffVolumeResizeStrategy,
			}

			volumeRecommendation, err := resizer.ResizePVC(ctx, logr.Discard(), fakeClient, eventRecorder, pvc, recommendation, v1alpha1.VolumeRecommendation{Name: "test-pvc"}, resizingConds)
			Expect(err).NotTo(HaveOccurred())

			By("surfacing the recommended target size in the status")
			Expect(volumeRecommendation.Target.Size).To(HaveValue(Equal(targetSize)))
			Expect(volumeRecommendation.LastResizeTime).To(BeNil())

			By("leaving the PVC spec untouched")
			var persisted corev1.PersistentVolumeClaim
			Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(pvc), &persisted)).To(Succeed())
			Expect(persisted.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))
			Expect(persisted.Annotations).NotTo(HaveKey(common.AnnotationPreviousSize))

			By("not recording a resizing condition")
			Expect(resizingConds.GetAggregatedCondition().Message).To(BeEmpty())
		})

		It("patches the PVC and records the resizing condition when the strategy applies", func() {
			pvc := makePVC("1Gi", "1Gi")
			fakeClient := newClient(pvc)
			targetSize := resource.MustParse("2Gi")
			recommendation := recommender.Recommendation{
				TargetSize:     &targetSize,
				ScalingReason:  common.ScalingReasonStorageThreshold,
				ResizeStrategy: v1alpha1.InPlaceVolumeResizeStrategy,
			}

			volumeRecommendation, err := resizer.ResizePVC(ctx, logr.Discard(), fakeClient, eventRecorder, pvc, recommendation, v1alpha1.VolumeRecommendation{Name: "test-pvc"}, resizingConds)
			Expect(err).NotTo(HaveOccurred())

			By("recording the target size and the last resize time")
			Expect(volumeRecommendation.Target.Size).To(HaveValue(Equal(targetSize)))
			Expect(volumeRecommendation.LastResizeTime).NotTo(BeNil())

			By("patching the PVC spec and the previous-size annotation")
			var persisted corev1.PersistentVolumeClaim
			Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(pvc), &persisted)).To(Succeed())
			Expect(persisted.Spec.Resources.Requests.Storage().String()).To(Equal("2Gi"))
			Expect(persisted.Annotations).To(HaveKeyWithValue(common.AnnotationPreviousSize, "1Gi"))

			By("recording the ResizingStorage event")
			Expect(<-eventRecorder.Events).To(Equal("Normal ResizingStorage resizing storage from 1Gi to 2Gi"))

			By("recording a Resizing=True condition")
			Expect(resizingConds.GetAggregatedCondition()).To(And(
				HaveField("Type", string(v1alpha1.ConditionTypeResizing)),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Message", ContainSubstring("resizing from 1Gi to 2Gi due to "+common.ScalingReasonStorageThreshold)),
			))
		})
	})

	Describe("#ValidatePVC", func() {
		// storageClass builds a StorageClass with the given expansion support.
		storageClass := func(name string, allowExpansion bool) *storagev1.StorageClass {
			return &storagev1.StorageClass{
				ObjectMeta:           metav1.ObjectMeta{Name: name},
				Provisioner:          "no-provisioner",
				AllowVolumeExpansion: ptr.To(allowExpansion),
			}
		}

		// eligiblePVC builds a bound, filesystem PVC referencing the given storage class.
		eligiblePVC := func(scName string) *corev1.PersistentVolumeClaim {
			pvc := makePVC("1Gi", "1Gi")
			pvc.Spec.StorageClassName = ptr.To(scName)
			pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
			pvc.Status.Phase = corev1.ClaimBound

			return pvc
		}

		policy := v1alpha1.VolumePolicy{MaxCapacity: resource.MustParse("10Gi")}

		It("returns no error for an eligible PVC", func() {
			pvc := eligiblePVC("expandable")
			fakeClient := newClient(storageClass("expandable", true))

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, policy)).To(Succeed())
		})

		It("errors when the current status capacity is invalid", func() {
			pvc := eligiblePVC("expandable")
			pvc.Status.Capacity = corev1.ResourceList{}
			fakeClient := newClient(storageClass("expandable", true))

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, policy)).To(MatchError(ContainSubstring(".status.capacity.storage is invalid")))
		})

		It("errors when max capacity is less than the current size", func() {
			pvc := eligiblePVC("expandable")
			fakeClient := newClient(storageClass("expandable", true))
			smallPolicy := v1alpha1.VolumePolicy{MaxCapacity: resource.MustParse("512Mi")}

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, smallPolicy)).To(MatchError(ContainSubstring("max capacity")))
		})

		It("returns ErrStorageClassNotFound when the PVC has no storage class", func() {
			pvc := eligiblePVC("expandable")
			pvc.Spec.StorageClassName = nil
			fakeClient := newClient()

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, policy)).To(MatchError(resizer.ErrStorageClassNotFound))
		})

		It("returns ErrStorageClassDoesNotSupportExpansion when the storage class forbids expansion", func() {
			pvc := eligiblePVC("no-expansion")
			fakeClient := newClient(storageClass("no-expansion", false))

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, policy)).To(MatchError(resizer.ErrStorageClassDoesNotSupportExpansion))
		})

		It("returns ErrVolumeModeIsNotFilesystem for a block volume", func() {
			pvc := eligiblePVC("expandable")
			pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
			fakeClient := newClient(storageClass("expandable", true))

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, policy)).To(MatchError(resizer.ErrVolumeModeIsNotFilesystem))
		})

		It("returns ErrPVCNotBound when the PVC is not bound", func() {
			pvc := eligiblePVC("expandable")
			pvc.Status.Phase = corev1.ClaimLost
			fakeClient := newClient(storageClass("expandable", true))

			Expect(resizer.ValidatePVC(ctx, fakeClient, pvc, policy)).To(MatchError(resizer.ErrPVCNotBound))
		})
	})
})
