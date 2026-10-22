// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package pvc_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/pvc-autoscaler/internal/common"
	"github.com/gardener/pvc-autoscaler/internal/pvc"
)

const image = "europe-docker.pkg.dev/gardener-project/releases/gardener/pvc-copy:latest"

var _ = Describe("Copier", func() {
	var (
		ctx        context.Context
		fakeClient client.Client

		source      *corev1.PersistentVolumeClaim
		destination *corev1.PersistentVolumeClaim
	)

	BeforeEach(func() {
		ctx = context.Background()

		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(batchv1.AddToScheme(scheme)).To(Succeed())

		fakeClient = fakeclient.NewClientBuilder().WithScheme(scheme).Build()

		source = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: "default"},
		}
		destination = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "destination", Namespace: "default"},
		}
	})

	Describe("#NewRsyncCopier", func() {
		It("should return an error when no client is provided", func() {
			_, err := pvc.NewRsyncCopier(pvc.WithImage(image))
			Expect(err).To(Equal(pvc.ErrNoClient))
		})

		It("should return an error when no image is provided", func() {
			_, err := pvc.NewRsyncCopier(pvc.WithClient(fakeClient))
			Expect(err).To(Equal(pvc.ErrNoImage))
		})

		It("should return a copier when all options are provided", func() {
			copier, err := pvc.NewRsyncCopier(pvc.WithClient(fakeClient), pvc.WithImage(image))
			Expect(err).NotTo(HaveOccurred())
			Expect(copier).NotTo(BeNil())
		})
	})

	Describe("#EnsureCopy", func() {
		var copier *pvc.RsyncCopier

		BeforeEach(func() {
			var err error
			copier, err = pvc.NewRsyncCopier(pvc.WithClient(fakeClient), pvc.WithImage(image))
			Expect(err).NotTo(HaveOccurred())
		})

		// jobForPVCs returns the single copy Job present in the destination namespace.
		jobForPVCs := func() *batchv1.Job {
			GinkgoHelper()
			jobList := &batchv1.JobList{}
			Expect(fakeClient.List(ctx, jobList, client.InNamespace(destination.Namespace))).To(Succeed())
			Expect(jobList.Items).To(HaveLen(1))

			return &jobList.Items[0]
		}

		When("the copy Job does not exist yet", func() {
			It("should create the Job and report the copy as running", func() {
				status, err := copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())
				Expect(status).To(Equal(pvc.StatusRunning))

				job := jobForPVCs()
				Expect(job.Labels).To(HaveKeyWithValue(common.LabelManagedBy, common.LabelManagedByValue))
			})

			It("should mount the source read-only and the destination writable", func() {
				_, err := copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())

				podSpec := jobForPVCs().Spec.Template.Spec
				Expect(podSpec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
				Expect(podSpec.Containers).To(HaveLen(1))

				container := podSpec.Containers[0]
				Expect(container.Image).To(Equal(image))
				Expect(container.VolumeMounts).To(ContainElements(
					HaveField("ReadOnly", BeTrue()),
					HaveField("ReadOnly", BeFalse()),
				))

				var sourceClaim, destinationClaim *corev1.PersistentVolumeClaimVolumeSource
				for _, volume := range podSpec.Volumes {
					switch volume.PersistentVolumeClaim.ClaimName {
					case source.Name:
						sourceClaim = volume.PersistentVolumeClaim
					case destination.Name:
						destinationClaim = volume.PersistentVolumeClaim
					}
				}
				Expect(sourceClaim).NotTo(BeNil())
				Expect(sourceClaim.ReadOnly).To(BeTrue())
				Expect(destinationClaim).NotTo(BeNil())
				Expect(destinationClaim.ReadOnly).To(BeFalse())
			})
		})

		When("the copy Job already exists", func() {
			It("should not create a second Job", func() {
				_, err := copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())
				firstJob := jobForPVCs()

				_, err = copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())

				secondJob := jobForPVCs() // jobForPVCs asserts exactly one Job exists.
				Expect(secondJob.UID).To(Equal(firstJob.UID))
			})

			DescribeTable("should report the status derived from the Job conditions",
				func(conditions []batchv1.JobCondition, expected pvc.Status) {
					_, err := copier.EnsureCopy(ctx, source, destination)
					Expect(err).NotTo(HaveOccurred())

					job := jobForPVCs()
					patch := client.MergeFrom(job.DeepCopy())
					job.Status.Conditions = conditions
					Expect(fakeClient.Status().Patch(ctx, job, patch)).To(Succeed())

					status, err := copier.EnsureCopy(ctx, source, destination)
					Expect(err).NotTo(HaveOccurred())
					Expect(status).To(Equal(expected))
				},
				Entry("no conditions", nil, pvc.StatusRunning),
				Entry("complete",
					[]batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
					pvc.StatusSucceeded),
				Entry("failed",
					[]batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}},
					pvc.StatusFailed),
				Entry("complete condition present but false",
					[]batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}},
					pvc.StatusRunning),
			)
		})
	})

	Describe("#DeleteCopy", func() {
		var copier *pvc.RsyncCopier

		BeforeEach(func() {
			var err error
			copier, err = pvc.NewRsyncCopier(pvc.WithClient(fakeClient), pvc.WithImage(image))
			Expect(err).NotTo(HaveOccurred())
		})

		jobList := func() []batchv1.Job {
			GinkgoHelper()
			list := &batchv1.JobList{}
			Expect(fakeClient.List(ctx, list, client.InNamespace(destination.Namespace))).To(Succeed())

			return list.Items
		}

		When("the copy Job does not exist", func() {
			It("should not return an error", func() {
				Expect(copier.DeleteCopy(ctx, source, destination)).To(Succeed())
			})
		})

		When("the copy Job exists", func() {
			BeforeEach(func() {
				_, err := copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())
				Expect(jobList()).To(HaveLen(1))
			})

			It("should delete the Job", func() {
				Expect(copier.DeleteCopy(ctx, source, destination)).To(Succeed())
				Expect(jobList()).To(BeEmpty())
			})

			It("should let EnsureCopy start a fresh copy afterwards (retry)", func() {
				// Fail the copy, delete it, then re-run: the fresh Job has no failed
				// condition and the copy is reported as running again.
				job := &batchv1.Job{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(&jobList()[0]), job)).To(Succeed())
				patch := client.MergeFrom(job.DeepCopy())
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
				Expect(fakeClient.Status().Patch(ctx, job, patch)).To(Succeed())

				status, err := copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())
				Expect(status).To(Equal(pvc.StatusFailed))

				Expect(copier.DeleteCopy(ctx, source, destination)).To(Succeed())

				status, err = copier.EnsureCopy(ctx, source, destination)
				Expect(err).NotTo(HaveOccurred())
				Expect(status).To(Equal(pvc.StatusRunning))

				jobs := jobList()
				Expect(jobs).To(HaveLen(1))
				Expect(jobs[0].Status.Conditions).To(BeEmpty())
			})
		})
	})
})
