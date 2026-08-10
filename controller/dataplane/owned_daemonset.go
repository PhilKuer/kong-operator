package dataplane

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/kong/kong-operator/v2/api/common/v1alpha1"
	operatorv1beta1 "github.com/kong/kong-operator/v2/api/gateway-operator/v1beta1"
	dataplanepkg "github.com/kong/kong-operator/v2/controller/pkg/dataplane"
	"github.com/kong/kong-operator/v2/controller/pkg/log"
	"github.com/kong/kong-operator/v2/controller/pkg/op"
	"github.com/kong/kong-operator/v2/controller/pkg/patch"
	"github.com/kong/kong-operator/v2/controller/pkg/utils"
	"github.com/kong/kong-operator/v2/pkg/consts"
	k8sutils "github.com/kong/kong-operator/v2/pkg/utils/kubernetes"
	k8sreduce "github.com/kong/kong-operator/v2/pkg/utils/kubernetes/reduce"
	k8sresources "github.com/kong/kong-operator/v2/pkg/utils/kubernetes/resources"
)

// dataPlaneWorkloadType returns the workload type configured for the DataPlane,
// defaulting to Deployment for DataPlanes created before the field existed (the
// CRD default only applies to objects written after the operator was upgraded).
func dataPlaneWorkloadType(dataplane *operatorv1beta1.DataPlane) commonv1alpha1.WorkloadType {
	if dataplane.Spec.Deployment.WorkloadType == commonv1alpha1.WorkloadTypeDaemonSet {
		return commonv1alpha1.WorkloadTypeDaemonSet
	}
	return commonv1alpha1.WorkloadTypeDeployment
}

// BuildAndDeployDaemonSet builds and deploys a DataPlane DaemonSet, or reduces DaemonSets if there is more
// than one. It returns the DaemonSet if it created or updated one, or nil if it needed to reduce.
//
// The DaemonSet is derived from the very same Deployment the Deployment workload type would get, so that
// both workload types share their image resolution, user patches, environment defaulting and certificates.
func (d *DeploymentBuilder) BuildAndDeployDaemonSet(
	ctx context.Context,
	dataplane *operatorv1beta1.DataPlane,
	enforceConfig bool,
	validateDataPlaneImage bool,
) (*appsv1.DaemonSet, op.Result, error) {
	if err := d.ensureKonnectCert(ctx, dataplane); err != nil {
		return nil, op.Noop, err
	}

	// if there is more than one DaemonSet, delete the extras
	reduced, existingDaemonSet, err := listOrReduceDataPlaneDaemonSets(ctx, d.client, dataplane, d.additionalLabels)
	if err != nil {
		return nil, op.Noop, fmt.Errorf("failed listing existing DaemonSets: %w", err)
	}
	if reduced {
		return nil, op.Noop, nil
	}

	desiredDeployment, err := d.buildDesiredDeployment(ctx, dataplane, validateDataPlaneImage)
	if err != nil {
		return nil, op.Noop, err
	}
	desiredDaemonSet := k8sresources.DaemonSetFromDeployment(desiredDeployment.Unwrap())

	// push the complete DaemonSet to Kubernetes
	return reconcileDataPlaneDaemonSet(ctx, d.client, d.logger, enforceConfig,
		dataplane, existingDaemonSet, desiredDaemonSet.Unwrap())
}

// listOrReduceDataPlaneDaemonSets lists existing DataPlane DaemonSets. If only one is present, it returns it.
// If multiple are present, it reduces them to one and notifies the caller it reduced, so that the caller can
// try its operation again once there's only a single DaemonSet to work with.
func listOrReduceDataPlaneDaemonSets(
	ctx context.Context,
	cl client.Client,
	dataplane *operatorv1beta1.DataPlane,
	additionalDaemonSetLabels client.MatchingLabels,
) (reduced bool, daemonSet *appsv1.DaemonSet, err error) {
	matchingLabels := k8sresources.GetManagedLabelForOwner(dataplane)
	maps.Copy(matchingLabels, additionalDaemonSetLabels)

	daemonSets, err := k8sutils.ListDaemonSetsForOwner(
		ctx,
		cl,
		dataplane.Namespace,
		dataplane.UID,
		matchingLabels,
	)
	if err != nil {
		return false, nil, fmt.Errorf("failed listing DaemonSets for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
	}

	count := len(daemonSets)
	if count > 1 {
		if err := k8sreduce.ReduceDaemonSets(ctx, cl, daemonSets, dataplanepkg.OwnedObjectPreDeleteHook); err != nil {
			return false, nil, err
		}
		return true, nil, errors.New("number of daemonsets reduced")
	}
	if count == 0 {
		return false, nil, nil
	}

	return false, &daemonSets[0], nil
}

// reconcileDataPlaneDaemonSet takes any existing DataPlane DaemonSet and a desired DataPlane DaemonSet and
// reconciles the existing state to the desired state by either updating an existing DaemonSet, creating a new
// one, or doing nothing.
func reconcileDataPlaneDaemonSet(
	ctx context.Context,
	cl client.Client,
	logger logr.Logger,
	enforceConfig bool,
	dataplane *operatorv1beta1.DataPlane,
	existing *appsv1.DaemonSet,
	desired *appsv1.DaemonSet,
) (daemonSet *appsv1.DaemonSet, res op.Result, err error) {
	if existing != nil {
		// If the enforceConfig flag is not set, we compare the spec hash of the
		// existing DaemonSet with the spec hash of the desired DaemonSet. If the
		// hashes match, we skip the update.
		if !enforceConfig {
			match, err := k8sresources.SpecHashMatchesAnnotation(deploymentRelevantDataPlaneSpec(dataplane), existing)
			if err != nil {
				return nil, op.Noop, err
			}
			if match {
				log.Debug(logger, "DataPlane DaemonSet spec hash matches existing DaemonSet, skipping update")
				return existing, op.Noop, nil
			}
			// If the spec hash does not match, we need to enforce the configuration
			// so fall through to the update logic.
		}

		var updated bool
		original := existing.DeepCopy()

		k8sresources.SetDefaultsPodTemplateSpec(&desired.Spec.Template)

		// Save the original last-applied-annotations before EnsureObjectMetaIsUpdated
		// merges generated annotations into existing ones. EnsureObjectMetaIsUpdated
		// overwrites AnnotationLastAppliedAnnotations with the new value before the
		// option function runs, which prevents detecting removed annotations.
		originalLastApplied := existing.Annotations[consts.AnnotationLastAppliedAnnotations]

		// ensure that object metadata is up to date
		updated, existing.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existing.ObjectMeta, desired.ObjectMeta,
			// enforce all the annotations provided through the dataplane API, removing
			// any that were previously set by the operator but are no longer present
			// in the DataPlane spec.
			func(existingMeta metav1.ObjectMeta, generatedMeta metav1.ObjectMeta) (bool, metav1.ObjectMeta) {
				if existingMeta.Annotations != nil && originalLastApplied != "" {
					existingMeta.Annotations[consts.AnnotationLastAppliedAnnotations] = originalLastApplied
				}
				metaToUpdate, updatedAnnotations, err := ensureDataPlaneDeploymentAnnotationsUpdated(
					dataplane, existingMeta.Annotations, generatedMeta.Annotations,
				)
				if err != nil {
					log.Error(logger, err, "failed to update annotations of existing DaemonSet for DataPlane",
						"dataplane", fmt.Sprintf("%s/%s", dataplane.Namespace, dataplane.Name),
						"daemonset", fmt.Sprintf("%s/%s", existing.Namespace, existing.Name))
					return true, existingMeta
				}
				existingMeta.Annotations = updatedAnnotations
				return metaToUpdate, existingMeta
			},
		)

		// some custom comparison rules are needed for some PodTemplateSpec sub-attributes
		opts := []cmp.Option{
			cmp.Comparer(k8sresources.ResourceRequirementsEqual),
			utils.IgnoreAnnotationKeysComparer(restartAnnotationKey),
		}

		if !cmp.Equal(existing.Spec.Template, desired.Spec.Template, opts...) {
			restartTimeStr, isRestartOperation := isRecentDeploymentRestart(&existing.Spec.Template, logger)
			if isRestartOperation {
				log.Debug(logger, "found restart annotation", "timestamp", restartTimeStr)
				// Preserve the restart annotation
				if desired.Spec.Template.Annotations == nil {
					desired.Spec.Template.Annotations = make(map[string]string)
				}
				desired.Spec.Template.Annotations[restartAnnotationKey] = restartTimeStr
			}

			existing.Spec.Template = desired.Spec.Template
			updated = true
		}

		// ensure that the update strategy is up to date
		if !cmp.Equal(existing.Spec.UpdateStrategy, desired.Spec.UpdateStrategy) {
			existing.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
			updated = true
		}

		if updated {
			diff := cmp.Diff(original.Spec.Template, desired.Spec.Template, opts...)
			log.Trace(logger, "DataPlane DaemonSet diff detected", "diff", diff)
		}

		res, patched, err := patch.ApplyPatchIfNotEmpty(ctx, cl, logger, existing, original, updated)
		return patched, res, err
	}

	if err = cl.Create(ctx, desired); err != nil {
		return nil, op.Noop, fmt.Errorf("failed creating DaemonSet for DataPlane %s: %w", dataplane.Name, err)
	}

	log.Debug(logger, "daemonset for DataPlane created", "daemonset", desired.Name)
	return desired, op.Created, nil
}

// deleteDataPlaneWorkloadsOfOtherType deletes the workloads that don't match the DataPlane's currently
// configured workload type. It is what makes switching spec.deployment.workloadType back and forth
// converge instead of leaving both a Deployment and a DaemonSet serving traffic.
//
// It returns true if it deleted anything, in which case the caller should let the resulting watch event
// trigger another reconciliation.
func deleteDataPlaneWorkloadsOfOtherType(
	ctx context.Context,
	cl client.Client,
	logger logr.Logger,
	dataplane *operatorv1beta1.DataPlane,
	additionalLabels client.MatchingLabels,
) (bool, error) {
	matchingLabels := k8sresources.GetManagedLabelForOwner(dataplane)
	maps.Copy(matchingLabels, additionalLabels)

	var objs []client.Object
	if dataPlaneWorkloadType(dataplane) == commonv1alpha1.WorkloadTypeDaemonSet {
		deployments, err := k8sutils.ListDeploymentsForOwner(ctx, cl, dataplane.Namespace, dataplane.UID, matchingLabels)
		if err != nil {
			return false, fmt.Errorf("failed listing Deployments for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
		}
		for i := range deployments {
			objs = append(objs, &deployments[i])
		}
	} else {
		daemonSets, err := k8sutils.ListDaemonSetsForOwner(ctx, cl, dataplane.Namespace, dataplane.UID, matchingLabels)
		if err != nil {
			return false, fmt.Errorf("failed listing DaemonSets for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
		}
		for i := range daemonSets {
			objs = append(objs, &daemonSets[i])
		}
	}

	var deleted bool
	for _, obj := range objs {
		// The workload carries a finalizer that's only removed once its owner is gone, so it has to be
		// dropped explicitly here: the DataPlane itself is very much alive, it just changed its mind
		// about which workload should run its Pods.
		if err := dataplanepkg.OwnedObjectPreDeleteHook(ctx, cl, obj); err != nil {
			return deleted, err
		}
		if err := cl.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return deleted, fmt.Errorf("failed deleting %T %s/%s for DataPlane %s/%s: %w",
				obj, obj.GetNamespace(), obj.GetName(), dataplane.Namespace, dataplane.Name, err)
		}
		log.Debug(logger, "deleted DataPlane workload that no longer matches the configured workloadType",
			"workload", fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName()),
			"workloadType", dataPlaneWorkloadType(dataplane),
		)
		deleted = true
	}

	return deleted, nil
}
