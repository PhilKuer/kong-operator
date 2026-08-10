package resources

import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	pkgapisappsv1 "k8s.io/kubernetes/pkg/apis/apps/v1"
)

// DaemonSetFromDeployment converts a generated DataPlane Deployment into the
// equivalent DaemonSet.
//
// DataPlane workloads are always generated as Deployments first: that way the
// image resolution, user PodTemplateSpec patches, environment defaulting,
// certificate mounting and all the DeploymentOpt transforms are shared between
// both workload types and can't drift apart. Only the fields that make up the
// workload's own identity (selector, Pod template, metadata) carry over here,
// the replica and rollout related ones have no DaemonSet counterpart: a
// DaemonSet's size follows the number of eligible nodes.
func DaemonSetFromDeployment(d *appsv1.Deployment) *DaemonSet {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: *d.ObjectMeta.DeepCopy(),
		Spec: appsv1.DaemonSetSpec{
			Selector:        d.Spec.Selector.DeepCopy(),
			Template:        *d.Spec.Template.DeepCopy(),
			MinReadySeconds: d.Spec.MinReadySeconds,
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 1,
					},
				},
			},
		},
	}

	// Set defaults for the DaemonSet so that we don't get a diff when we compare
	// it with what's in the cluster.
	pkgapisappsv1.SetDefaults_DaemonSet(daemonSet)

	wrapped := DaemonSet(*daemonSet)
	return &wrapped
}

// DaemonSet is a wrapper for appsv1.DaemonSet, mirroring the Deployment wrapper
// in this package.
type DaemonSet appsv1.DaemonSet

// Unwrap returns the underlying appsv1.DaemonSet.
func (d *DaemonSet) Unwrap() *appsv1.DaemonSet {
	return (*appsv1.DaemonSet)(d)
}
