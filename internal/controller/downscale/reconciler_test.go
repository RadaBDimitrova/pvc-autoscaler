// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package downscale

import (
	"context"
	"testing"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	scaleclient "k8s.io/client-go/scale"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"
	"github.com/gardener/pvc-autoscaler/internal/pvc"
)

// fakeScales is an in-memory scaleclient.ScalesGetter that tracks replica counts keyed by
// "namespace/name", so the downscale controller's scale up/down can be exercised without a
// real API server.
type fakeScales struct {
	replicas map[string]int32
}

func (f *fakeScales) Scales(namespace string) scaleclient.ScaleInterface {
	return &fakeScaleClient{parent: f, namespace: namespace}
}

type fakeScaleClient struct {
	parent    *fakeScales
	namespace string
}

func (s *fakeScaleClient) Get(_ context.Context, _ schema.GroupResource, name string, _ metav1.GetOptions) (*autoscalingv1.Scale, error) {
	return &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: s.parent.replicas[s.namespace+"/"+name]},
	}, nil
}

func (s *fakeScaleClient) Update(_ context.Context, _ schema.GroupResource, scale *autoscalingv1.Scale, _ metav1.UpdateOptions) (*autoscalingv1.Scale, error) {
	s.parent.replicas[s.namespace+"/"+scale.Name] = scale.Spec.Replicas

	return scale, nil
}

func (s *fakeScaleClient) Patch(_ context.Context, _ schema.GroupVersionResource, _ string, _ types.PatchType, _ []byte, _ metav1.PatchOptions) (*autoscalingv1.Scale, error) {
	return nil, nil
}

// deploymentRESTMapper returns a REST mapper that can resolve apps/v1 Deployments, which is
// what getScale needs before talking to the (fake) scale client.
func deploymentRESTMapper() apimeta.RESTMapper {
	rm := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}})
	rm.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, apimeta.RESTScopeNamespace)

	return rm
}

// fakeCopier is a pvc.Copier whose status the test controls, standing in for the real
// rsync-Job-based copier.
type fakeCopier struct {
	status  pvc.Status
	deleted bool
}

func (f *fakeCopier) EnsureCopy(_ context.Context, _, _ *corev1.PersistentVolumeClaim) (pvc.Status, error) {
	return f.status, nil
}

func (f *fakeCopier) DeleteCopy(_ context.Context, _, _ *corev1.PersistentVolumeClaim) error {
	f.deleted = true

	return nil
}

// TestReconcileDrivesTaskToCompleted walks a downscale from an untouched PVC all the way to
// a completed task. Each reconcile observes the live state and does as much as it can;
// between reconciles the test simulates the external controllers the real cluster would run
// (the PV binder and the data copy), which the controller otherwise waits on.
func TestReconcileDrivesTaskToCompleted(t *testing.T) {
	g := newGomega(t)
	ctx := context.Background()

	scheme := runtime.NewScheme()
	g.noErr(corev1.AddToScheme(scheme))
	g.noErr(v1alpha1.AddToScheme(scheme))

	const (
		ns       = "default"
		pvcName  = "data-web-0"
		target   = "web"
		tempName = pvcName + TempPVCSuffix
	)

	original := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr.To("my-storage-class"),
			VolumeMode:       ptr.To(corev1.PersistentVolumeFilesystem),
			VolumeName:       "pv-old",
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}

	task := &v1alpha1.PVCDownscaleTask{
		ObjectMeta: metav1.ObjectMeta{Name: "downscale-data-web-0", Namespace: ns, UID: types.UID("uid12345")},
		Spec: v1alpha1.PVCDownscaleTaskSpec{
			PVCName:    pvcName,
			TargetSize: resource.MustParse("1Gi"),
			CopyType:   CopyTypeRsync,
			TargetRef:  autoscalingv1.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: target},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(original, task).
		WithStatusSubresource(&v1alpha1.PVCDownscaleTask{}, &corev1.PersistentVolume{}).
		Build()

	scales := &fakeScales{replicas: map[string]int32{ns + "/" + target: 3}}
	copier := &fakeCopier{status: pvc.StatusRunning}

	r, err := New(
		WithClient(c),
		WithEventRecorder(record.NewFakeRecorder(1024)),
		WithScaleClient(scales),
		WithRESTMapper(deploymentRESTMapper()),
		WithCopier(copier),
	)
	g.noErr(err)

	taskKey := client.ObjectKeyFromObject(task)
	reconcileOnce := func() {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: taskKey})
		g.noErr(err)
	}
	readyReason := func() string {
		cur := &v1alpha1.PVCDownscaleTask{}
		g.noErr(c.Get(ctx, taskKey, cur))
		if cond := apimeta.FindStatusCondition(cur.Status.Conditions, ConditionTypeReady); cond != nil {
			return cond.Reason
		}

		return ""
	}
	getPVCt := func(name string) *corev1.PersistentVolumeClaim {
		claim := &corev1.PersistentVolumeClaim{}
		g.noErr(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, claim))

		return claim
	}
	getPVt := func() *corev1.PersistentVolume {
		pv := &corev1.PersistentVolume{}
		g.noErr(c.Get(ctx, types.NamespacedName{Name: "pv-new"}, pv))

		return pv
	}

	// Pass 1: record replicas, scale target to 0, create the temp PVC, wait for it to bind.
	reconcileOnce()
	g.equal(readyReason(), ReasonProvisioning, "provisioning new volume")
	g.equal(scales.replicas[ns+"/"+target], int32(0), "target scaled to zero")
	cur := &v1alpha1.PVCDownscaleTask{}
	g.noErr(c.Get(ctx, taskKey, cur))
	if or := cur.Status.OriginalReplicas; or == nil || *or != 3 {
		t.Fatalf("OriginalReplicas = %v, want 3", or)
	}
	g.equal(getPVCt(tempName).Spec.Resources.Requests.Storage().String(), "1Gi", "temp PVC size")

	// Simulate the PV binder: provision and bind a new volume to the temp PVC.
	temp := getPVCt(tempName)
	pvNew := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-new"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName:              "my-storage-class",
			VolumeMode:                    ptr.To(corev1.PersistentVolumeFilesystem),
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource:        corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/pv-new"}},
			ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: ns, Name: tempName},
		},
	}
	g.noErr(c.Create(ctx, pvNew))
	tempPatch := client.MergeFrom(temp.DeepCopy())
	temp.Spec.VolumeName = "pv-new"
	g.noErr(c.Patch(ctx, temp, tempPatch))

	// Pass 2: retain the new PV, record it, start the copy (Running) and wait.
	reconcileOnce()
	g.equal(readyReason(), ReasonCopying, "copying data")
	g.equal(string(getPVt().Spec.PersistentVolumeReclaimPolicy), string(corev1.PersistentVolumeReclaimRetain), "reclaim policy retained")
	g.noErr(c.Get(ctx, taskKey, cur))
	g.equal(cur.Status.NewVolumeName, "pv-new", "recorded new volume")

	// Simulate the copy completing.
	copier.status = pvc.StatusSucceeded

	// Pass 3: copy done -> delete the temp PVC.
	reconcileOnce()
	g.equal(readyReason(), ReasonSwapping, "swapping volume (temp delete)")
	assertGone(t, ctx, c, types.NamespacedName{Namespace: ns, Name: tempName}, &corev1.PersistentVolumeClaim{})

	// Pass 4: delete the original PVC.
	reconcileOnce()
	assertGone(t, ctx, c, types.NamespacedName{Namespace: ns, Name: pvcName}, &corev1.PersistentVolumeClaim{})

	// Pass 5: recreate the original-named PVC bound to the new volume.
	reconcileOnce()
	recreated := getPVCt(pvcName)
	g.equal(recreated.Spec.VolumeName, "pv-new", "recreated PVC bound to new volume")
	g.equal(recreated.Spec.Resources.Requests.Storage().String(), "1Gi", "recreated PVC size")

	// Pass 6: final stretch - the stale claimRef is cleared so the PV can rebind.
	reconcileOnce()
	g.equal(readyReason(), ReasonRebinding, "rebinding volume")
	if getPVt().Spec.ClaimRef != nil {
		t.Fatalf("expected claimRef to be cleared, got %+v", getPVt().Spec.ClaimRef)
	}

	// Simulate the binder binding the PV to the recreated PVC.
	pvNew = getPVt()
	specPatch := client.MergeFrom(pvNew.DeepCopy())
	pvNew.Spec.ClaimRef = &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: ns, Name: pvcName}
	g.noErr(c.Patch(ctx, pvNew, specPatch))
	pvNew = getPVt()
	pvNew.Status.Phase = corev1.VolumeBound
	g.noErr(c.Status().Update(ctx, pvNew))

	// Pass 7: bound -> scale back up, clean up the copy, complete.
	reconcileOnce()
	g.equal(readyReason(), ReasonCompleted, "completed")
	g.equal(scales.replicas[ns+"/"+target], int32(3), "target scaled back up")
	if !copier.deleted {
		t.Fatal("expected the copy to be deleted on completion")
	}
	g.noErr(c.Get(ctx, taskKey, cur))
	if cond := apimeta.FindStatusCondition(cur.Status.Conditions, ConditionTypeReady); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %v, want True", cond)
	}

	// Terminal: further reconciles are a no-op.
	reconcileOnce()
	g.equal(readyReason(), ReasonCompleted, "stays completed")
}

// --- tiny assertion helpers (keep plain testing.T readable) ----------------

type gm struct{ t *testing.T }

func newGomega(t *testing.T) *gm { return &gm{t: t} }

func (g *gm) noErr(err error) {
	g.t.Helper()
	if err != nil {
		g.t.Fatalf("unexpected error: %v", err)
	}
}

func (g *gm) equal(got, want any, msg string) {
	g.t.Helper()
	if got != want {
		g.t.Fatalf("%s: got %v, want %v", msg, got, want)
	}
}

func assertGone(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName, obj client.Object) {
	t.Helper()
	err := c.Get(ctx, key, obj)
	if err == nil {
		t.Fatalf("expected %s to be deleted, but it still exists", key)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error getting %s: %v", key, err)
	}
}
