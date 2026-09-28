// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package status_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/status"
	testutils "github.com/gardener/pvc-autoscaler/test/utils"
)

// matchAllPolicy builds a VolumePolicy that matches every PVC name.
func matchAllPolicy() v1alpha1.VolumePolicy {
	return v1alpha1.VolumePolicy{
		Match:       v1alpha1.Match{Name: "*"},
		MaxCapacity: resource.MustParse("10Gi"),
	}
}

var _ = Describe("Status", func() {
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

	Describe("#New", func() {
		It("retains recommendations whose PVC exists and has a matching policy", func() {
			existing := []v1alpha1.VolumeRecommendation{{Name: "pvc-a"}, {Name: "pvc-b"}}
			pvcA, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			pvcB, err := testutils.CreatePVC(ctx, k8sClient, "pvc-b", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			pvcs := []*corev1.PersistentVolumeClaim{pvcA, pvcB}

			st := status.New(existing, pvcs, []v1alpha1.VolumePolicy{matchAllPolicy()})

			Expect(st.Recommendations).To(ConsistOf(
				v1alpha1.VolumeRecommendation{Name: "pvc-a"},
				v1alpha1.VolumeRecommendation{Name: "pvc-b"},
			))
		})

		It("drops recommendations whose PVC is no longer present", func() {
			existing := []v1alpha1.VolumeRecommendation{{Name: "pvc-a"}, {Name: "gone"}}
			pvcA, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			pvcs := []*corev1.PersistentVolumeClaim{pvcA}

			st := status.New(existing, pvcs, []v1alpha1.VolumePolicy{matchAllPolicy()})

			Expect(st.Recommendations).To(ConsistOf(v1alpha1.VolumeRecommendation{Name: "pvc-a"}))
		})

		It("drops recommendations that have no matching policy", func() {
			existing := []v1alpha1.VolumeRecommendation{{Name: "pvc-a"}}
			pvcA, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			pvcs := []*corev1.PersistentVolumeClaim{pvcA}
			policy := matchAllPolicy()
			policy.Match.Name = "other-*"

			st := status.New(existing, pvcs, []v1alpha1.VolumePolicy{policy})

			Expect(st.Recommendations).To(BeEmpty())
		})
	})

	Describe("#GetOrCreate", func() {
		It("returns the existing recommendation when present", func() {
			st := &status.Status{Recommendations: []v1alpha1.VolumeRecommendation{
				{Name: "pvc-a", Current: v1alpha1.CurrentVolumeStatus{UsedSpacePercent: ptr.To(42)}},
			}}

			Expect(st.GetOrCreate("pvc-a").Current.UsedSpacePercent).To(HaveValue(Equal(42)))
		})

		It("returns a fresh, named recommendation when absent", func() {
			st := &status.Status{}

			Expect(st.GetOrCreate("pvc-a")).To(Equal(v1alpha1.VolumeRecommendation{Name: "pvc-a"}))
			Expect(st.Recommendations).To(BeEmpty(), "GetOrCreate must not store the new recommendation")
		})
	})

	Describe("#Set", func() {
		It("replaces an existing recommendation", func() {
			st := &status.Status{Recommendations: []v1alpha1.VolumeRecommendation{{Name: "pvc-a"}}}

			st.Set("pvc-a", v1alpha1.VolumeRecommendation{Name: "pvc-a", Current: v1alpha1.CurrentVolumeStatus{UsedSpacePercent: ptr.To(7)}})

			Expect(st.Recommendations).To(HaveLen(1))
			Expect(st.Recommendations[0].Current.UsedSpacePercent).To(HaveValue(Equal(7)))
		})

		It("appends a new recommendation", func() {
			st := &status.Status{Recommendations: []v1alpha1.VolumeRecommendation{{Name: "pvc-a"}}}

			st.Set("pvc-b", v1alpha1.VolumeRecommendation{Name: "pvc-b"})

			Expect(st.Recommendations).To(HaveLen(2))
		})
	})

	Describe("#Sorted", func() {
		It("sorts recommendations by name", func() {
			st := &status.Status{Recommendations: []v1alpha1.VolumeRecommendation{{Name: "pvc-c"}, {Name: "pvc-a"}, {Name: "pvc-b"}}}

			sorted := st.Sorted()

			Expect(sorted).To(HaveExactElements(
				v1alpha1.VolumeRecommendation{Name: "pvc-a"},
				v1alpha1.VolumeRecommendation{Name: "pvc-b"},
				v1alpha1.VolumeRecommendation{Name: "pvc-c"},
			))
		})
	})

	Describe("#Observe", func() {
		It("records the observed current state and defaults the target size from spec", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())

			recommendation, err := status.Observe(v1alpha1.VolumeRecommendation{Name: "pvc-a"}, pvc, 60, 40, 1024*1024*1024)

			Expect(err).NotTo(HaveOccurred())
			Expect(recommendation).To(Equal(v1alpha1.VolumeRecommendation{
				Name: "pvc-a",
				Current: v1alpha1.CurrentVolumeStatus{
					Size:              pvc.Status.Capacity.Storage(),
					UsedSpacePercent:  ptr.To(60),
					UsedInodesPercent: ptr.To(40),
				},
				Target: v1alpha1.TargetRecommendation{
					Size: pvc.Spec.Resources.Requests.Storage(),
				},
			}))
		})

		It("preserves an already-recommended target size", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())
			existingTarget := resource.MustParse("2Gi")

			recommendation, err := status.Observe(
				v1alpha1.VolumeRecommendation{Name: "pvc-a", Target: v1alpha1.TargetRecommendation{Size: &existingTarget}},
				pvc, 60, 40, 1024*1024*1024,
			)

			Expect(err).NotTo(HaveOccurred())
			Expect(recommendation.Target.Size).To(HaveValue(Equal(existingTarget)))
		})

		It("returns ErrStaleMetrics when capacity deviates beyond the 0.5Gi floor on a small PVC", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "1Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())

			// status size is 1Gi; report ~200MiB capacity => delta ~824MiB > 0.5Gi floor.
			_, err = status.Observe(v1alpha1.VolumeRecommendation{Name: "pvc-a"}, pvc, 5, 0, 200*1024*1024)

			Expect(err).To(MatchError(common.ErrStaleMetrics))
		})

		It("applies the deviation ratio tolerance on a large PVC", func() {
			pvc, err := testutils.CreatePVC(ctx, k8sClient, "pvc-a", "100Gi", nil, nil)
			Expect(err).NotTo(HaveOccurred())

			By("deviating beyond the ratio tolerance")
			_, errStale := status.Observe(v1alpha1.VolumeRecommendation{Name: "pvc-a"}, pvc, 5, 0, 95*1024*1024*1024)
			Expect(errStale).To(MatchError(common.ErrStaleMetrics))

			By("deviating within the ratio tolerance")
			_, errOK := status.Observe(v1alpha1.VolumeRecommendation{Name: "pvc-a"}, pvc, 5, 0, 98*1024*1024*1024)
			Expect(errOK).NotTo(HaveOccurred())
		})
	})
})
