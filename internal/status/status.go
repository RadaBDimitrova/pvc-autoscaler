package status

import "github.com/gardener/pvc-autoscaler/api/autoscaling/v1alpha1"

type Status struct {
	Recommendations []v1alpha1.VolumeRecommendation
}
