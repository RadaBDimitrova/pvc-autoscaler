// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package pvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gardener/pvc-autoscaler/internal/common"
)

// Status describes the state of a data copy between two PersistentVolumeClaims.
type Status string

const (
	// StatusRunning indicates that the copy is in progress or has just been started.
	StatusRunning Status = "Running"

	// StatusSucceeded indicates that the copy completed successfully.
	StatusSucceeded Status = "Succeeded"

	// StatusFailed indicates that the copy failed.
	StatusFailed Status = "Failed"
)

// Copier ensures data is copied from a source PersistentVolumeClaim to a
// destination PersistentVolumeClaim and reports the progress of the operation.
type Copier interface {
	// EnsureCopy is idempotent: it starts the copy if it has not been started
	// yet and returns the current [Status]. It is safe to call on every
	// reconciliation.
	EnsureCopy(ctx context.Context, source, destination *corev1.PersistentVolumeClaim) (Status, error)

	// DeleteCopy removes the copy for the given source and destination PVCs,
	// including the Pods it started. It is idempotent: deleting a copy that
	// does not exist is not an error. A subsequent [Copier.EnsureCopy] starts a
	// fresh copy, which is how a failed copy is retried.
	DeleteCopy(ctx context.Context, source, destination *corev1.PersistentVolumeClaim) error
}

// rsyncArgs are the arguments passed to the rsync entrypoint of the pvc-copy
// image. The source and destination paths match the mount paths below.
var rsyncArgs = []string{"-a", "--delete", sourceMountPath + "/", destinationMountPath + "/"}

const (
	// jobNamePrefix prefixes the names of the data-copy Jobs.
	jobNamePrefix = "pvc-copy"

	// containerName is the name of the rsync container in the copy Job's Pod.
	containerName = "rsync"

	// sourceMountPath is where the source PVC is mounted in the copy container.
	sourceMountPath = "/src"

	// destinationMountPath is where the destination PVC is mounted in the copy container.
	destinationMountPath = "/dst"

	// nonRootUID is the UID the copy container runs as, matching the pvc-copy image.
	nonRootUID int64 = 65532

	// backoffLimit is the number of retries before the copy Job is marked failed.
	backoffLimit int32 = 3
)

// ErrNoClient is returned when the [RsyncCopier] is configured without a client.
var ErrNoClient = errors.New("no client provided")

// ErrNoImage is returned when the [RsyncCopier] is configured without an image.
var ErrNoImage = errors.New("no image provided")

// RsyncCopier is a [Copier] which copies data between PersistentVolumeClaims by
// running an rsync container in a Kubernetes Job.
type RsyncCopier struct {
	client client.Client
	image  string
}

var _ Copier = (*RsyncCopier)(nil)

// Option is a function which configures the [RsyncCopier].
type Option func(*RsyncCopier)

// NewRsyncCopier creates a new [RsyncCopier] with the given options.
func NewRsyncCopier(opts ...Option) (*RsyncCopier, error) {
	c := &RsyncCopier{}
	for _, opt := range opts {
		opt(c)
	}

	if c.client == nil {
		return nil, ErrNoClient
	}

	if c.image == "" {
		return nil, ErrNoImage
	}

	return c, nil
}

// WithClient configures the [RsyncCopier] with the given client.
func WithClient(cl client.Client) Option {
	return func(c *RsyncCopier) {
		c.client = cl
	}
}

// WithImage configures the [RsyncCopier] with the pvc-copy image reference.
func WithImage(image string) Option {
	return func(c *RsyncCopier) {
		c.image = image
	}
}

// EnsureCopy implements the [Copier] interface.
func (c *RsyncCopier) EnsureCopy(ctx context.Context, source, destination *corev1.PersistentVolumeClaim) (Status, error) {
	logger := log.FromContext(ctx)

	jobKey := client.ObjectKey{Namespace: destination.Namespace, Name: jobName(source.Name, destination.Name)}
	job := &batchv1.Job{}
	if err := c.client.Get(ctx, jobKey, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("failed to get copy Job %s: %w", jobKey, err)
		}

		job = c.buildJob(source, destination)
		if err := c.client.Create(ctx, job); err != nil {
			return "", fmt.Errorf("failed to create copy Job %s: %w", jobKey, err)
		}
		logger.Info("Created copy Job", "job", jobKey, "source", source.Name, "destination", destination.Name)

		return StatusRunning, nil
	}

	return statusFromJob(job), nil
}

// DeleteCopy implements the [Copier] interface.
func (c *RsyncCopier) DeleteCopy(ctx context.Context, source, destination *corev1.PersistentVolumeClaim) error {
	logger := log.FromContext(ctx)

	jobKey := client.ObjectKey{Namespace: destination.Namespace, Name: jobName(source.Name, destination.Name)}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: jobKey.Namespace},
	}
	// Background propagation so the Pods the Job started are deleted along with it.
	if err := c.client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to delete copy Job %s: %w", jobKey, err)
	}
	logger.Info("Deleted copy Job", "job", jobKey, "source", source.Name, "destination", destination.Name)

	return nil
}

// buildJob builds the copy Job for the given source and destination PVCs.
func (c *RsyncCopier) buildJob(source, destination *corev1.PersistentVolumeClaim) *batchv1.Job {
	const sourceVolumeName, destinationVolumeName = "source", "destination"

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(source.Name, destination.Name),
			Namespace: destination.Namespace,
			Labels:    map[string]string{common.LabelManagedBy: common.LabelManagedByValue},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(backoffLimit),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{common.LabelManagedBy: common.LabelManagedByValue},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  containerName,
							Image: c.image,
							Args:  rsyncArgs,
							VolumeMounts: []corev1.VolumeMount{
								{Name: sourceVolumeName, MountPath: sourceMountPath, ReadOnly: true},
								{Name: destinationVolumeName, MountPath: destinationMountPath},
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot:             ptr.To(true),
								RunAsUser:                ptr.To(nonRootUID),
								AllowPrivilegeEscalation: ptr.To(false),
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: sourceVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: source.Name,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: destinationVolumeName,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: destination.Name,
								},
							},
						},
					},
				},
			},
		},
	}
}

// statusFromJob maps the conditions of the given Job to a [Status].
func statusFromJob(job *batchv1.Job) Status {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return StatusSucceeded
		case batchv1.JobFailed:
			return StatusFailed
		}
	}

	return StatusRunning
}

// jobName returns a deterministic, DNS-label-safe (<=63 chars) name for the copy
// Job of the given source and destination PVCs. A hash of both names is appended
// to keep the name unique and bounded even when the PVC names are long.
func jobName(source, destination string) string {
	hash := sha256.Sum256([]byte(source + "/" + destination))
	suffix := hex.EncodeToString(hash[:])[:8]

	name := fmt.Sprintf("%s-%s-%s-%s", jobNamePrefix, source, destination, suffix)
	if len(name) > 63 {
		// Keep the hash suffix, which guarantees uniqueness, and truncate the rest.
		name = name[:63]
	}

	return name
}
