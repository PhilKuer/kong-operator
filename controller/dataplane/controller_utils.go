package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1alpha1 "github.com/kong/kong-operator/v2/api/common/v1alpha1"
	kcfgdataplane "github.com/kong/kong-operator/v2/api/gateway-operator/dataplane"
	operatorv1beta1 "github.com/kong/kong-operator/v2/api/gateway-operator/v1beta1"
	"github.com/kong/kong-operator/v2/controller/pkg/log"
	"github.com/kong/kong-operator/v2/controller/pkg/reservedkeys"
	"github.com/kong/kong-operator/v2/internal/versions"
	"github.com/kong/kong-operator/v2/pkg/consts"
	k8sutils "github.com/kong/kong-operator/v2/pkg/utils/kubernetes"
)

// -----------------------------------------------------------------------------
// DataPlane - Private Functions - Generators
// -----------------------------------------------------------------------------

func generateDataPlaneImage(dataplane *operatorv1beta1.DataPlane, defaultImage string, validators ...versions.VersionValidationOption) (string, error) {
	if dataplane.Spec.Deployment.PodTemplateSpec == nil {
		return defaultImage, nil // TODO: https://github.com/kong/kong-operator-archive/issues/20
	}

	container := k8sutils.GetPodContainerByName(&dataplane.Spec.Deployment.PodTemplateSpec.Spec, consts.DataPlaneProxyContainerName)
	if container != nil && container.Image != "" {
		for _, v := range validators {
			supported, err := v(container.Image)
			if err != nil {
				return "", err
			}
			if !supported {
				return "", fmt.Errorf("unsupported DataPlane image %s", container.Image)
			}
		}
		return container.Image, nil
	}

	if relatedKongImage := os.Getenv("RELATED_IMAGE_KONG"); relatedKongImage != "" {
		// RELATED_IMAGE_KONG is set by the operator-sdk when building the operator bundle.
		// https://github.com/kong/kong-operator-archive/issues/261
		return relatedKongImage, nil
	}

	return defaultImage, nil // TODO: https://github.com/kong/kong-operator-archive/issues/20
}

// -----------------------------------------------------------------------------
// DataPlane - Private Functions - Kubernetes Object Labels and Annotations
// -----------------------------------------------------------------------------

func addAnnotationsForDataPlaneIngressService(svc *corev1.Service, dataplane operatorv1beta1.DataPlane) {
	specAnnotations := extractDataPlaneIngressServiceAnnotations(&dataplane)
	if specAnnotations == nil {
		return
	}
	annotations := svc.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	maps.Copy(annotations, specAnnotations)
	encodedSpecAnnotations, err := json.Marshal(specAnnotations)
	if err == nil {
		annotations[consts.AnnotationLastAppliedAnnotations] = string(encodedSpecAnnotations)
	}
	svc.SetAnnotations(annotations)
}

func extractDataPlaneIngressServiceAnnotations(dataplane *operatorv1beta1.DataPlane) map[string]string {
	if dataplane.Spec.Network.Services == nil ||
		dataplane.Spec.Network.Services.Ingress == nil ||
		dataplane.Spec.Network.Services.Ingress.Annotations == nil {
		return nil
	}

	anns := dataplane.Spec.Network.Services.Ingress.Annotations
	return anns
}

func addLabelsForDataPlaneIngressService(svc *corev1.Service, dataplane operatorv1beta1.DataPlane) {
	specLabels := extractDataPlaneIngressServiceLabels(&dataplane)
	if specLabels == nil {
		return
	}
	lbls := svc.GetLabels()
	if lbls == nil {
		lbls = make(map[string]string)
	}
	maps.Copy(lbls, specLabels)
	svc.SetLabels(lbls)
}

func extractDataPlaneIngressServiceLabels(dataplane *operatorv1beta1.DataPlane) map[string]string {
	if dataplane.Spec.Network.Services == nil ||
		dataplane.Spec.Network.Services.Ingress == nil ||
		dataplane.Spec.Network.Services.Ingress.Labels == nil {
		return nil
	}
	result := make(map[string]string, len(dataplane.Spec.Network.Services.Ingress.Labels))
	for k, v := range dataplane.Spec.Network.Services.Ingress.Labels {
		result[string(k)] = string(v)
	}
	return result
}

// extractOutdatedDataPlaneIngressServiceAnnotations returns the last applied annotations
// of ingress service from `DataPlane` spec but disappeared in current `DataPlane` spec.
func extractOutdatedDataPlaneIngressServiceAnnotations(
	dataplane *operatorv1beta1.DataPlane, existingAnnotations map[string]string,
) (map[string]string, error) {
	if existingAnnotations == nil {
		return nil, nil
	}
	lastAppliedAnnotationsEncoded, ok := existingAnnotations[consts.AnnotationLastAppliedAnnotations]
	if !ok {
		return nil, nil
	}
	outdatedAnnotations := map[string]string{}
	err := json.Unmarshal([]byte(lastAppliedAnnotationsEncoded), &outdatedAnnotations)
	if err != nil {
		return nil, fmt.Errorf("failed to decode last applied annotations: %w", err)
	}
	// If an annotation is present in last applied annotations but not in current spec of annotations,
	// the annotation is outdated and should be removed.
	// So we remove the annotations present in current spec in last applied annotations,
	// the remaining annotations are outdated and should be removed.
	currentSpecifiedAnnotations := extractDataPlaneIngressServiceAnnotations(dataplane)
	for k := range currentSpecifiedAnnotations {
		delete(outdatedAnnotations, k)
	}
	return outdatedAnnotations, nil
}

// dataPlaneDeploymentReservedKeys reports whether a label/annotation key is reserved
// for internal operator or Kubernetes use and must be dropped from any
// spec.deployment.labels/annotations provided by the user.
var dataPlaneDeploymentReservedKeys = reservedkeys.NewChecker("app", "deployment.kubernetes.io/revision")

func addAnnotationsForDataPlaneDeployment(logger logr.Logger, deployment *appsv1.Deployment, dataplane operatorv1beta1.DataPlane) {
	specAnnotations := extractDataPlaneDeploymentAnnotations(&dataplane)
	if specAnnotations == nil {
		return
	}

	specAnnotations = reservedkeys.Filter(logger, reservedkeys.MetadataTypeAnnotation, specAnnotations, dataPlaneDeploymentReservedKeys)
	deployment.Annotations = reservedkeys.MergeAnnotationsTracked(deployment.Annotations, specAnnotations)
}

func extractDataPlaneDeploymentAnnotations(dataplane *operatorv1beta1.DataPlane) map[string]string {
	return dataplane.Spec.Deployment.Annotations
}

func addLabelsForDataPlaneDeployment(logger logr.Logger, deployment *appsv1.Deployment, dataplane operatorv1beta1.DataPlane) {
	specLabels := dataplane.Spec.Deployment.Labels
	if specLabels == nil {
		return
	}
	specLabels = reservedkeys.Filter(logger, reservedkeys.MetadataTypeLabel, specLabels, dataPlaneDeploymentReservedKeys)
	deployment.Labels = reservedkeys.Merge(deployment.Labels, specLabels)
}

// extractOutdatedDataPlaneDeploymentAnnotations returns the last applied annotations
// of the DataPlane Deployment from `DataPlane` spec but disappeared in current `DataPlane` spec.
func extractOutdatedDataPlaneDeploymentAnnotations(
	dataplane *operatorv1beta1.DataPlane, existingAnnotations map[string]string,
) (map[string]string, error) {
	outdatedAnnotations, err := reservedkeys.ExtractOutdated(extractDataPlaneDeploymentAnnotations(dataplane), existingAnnotations)
	if err != nil {
		return nil, err
	}
	return outdatedAnnotations, nil
}

// ensureDataPlaneReadyStatus ensures that the provided DataPlane gets an up to
// date Ready status condition.
// It sets the condition based on the readiness of DataPlane's Deployment and
// its ingress Service receiving an address.
func ensureDataPlaneReadyStatus(
	ctx context.Context,
	cl client.Client,
	logger logr.Logger,
	dataplane *operatorv1beta1.DataPlane,
	generation int64,
) (ctrl.Result, error) {
	// retrieve a fresh copy of the dataplane to reduce the number of times we have to error on update
	// due to new changes when the `DataPlane` resource is very active.
	if err := cl.Get(ctx, client.ObjectKeyFromObject(dataplane), dataplane); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed getting DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
	}

	workloadKind := dataPlaneWorkloadKind(dataplane)
	workloads, err := listDataPlaneLiveWorkloads(ctx, cl, dataplane)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed listing %ss for DataPlane %s/%s: %w", workloadKind, dataplane.Namespace, dataplane.Name, err)
	}

	switch len(workloads) {
	case 0:
		log.Debug(logger, fmt.Sprintf("%s for DataPlane not present yet", workloadKind))

		// Set Ready to false for dataplane as the underlying workload is not ready.
		k8sutils.SetCondition(
			k8sutils.NewConditionWithGeneration(
				kcfgdataplane.ReadyType,
				metav1.ConditionFalse,
				kcfgdataplane.WaitingToBecomeReadyReason,
				kcfgdataplane.WaitingToBecomeReadyMessage,
				generation,
			),
			dataplane,
		)
		ensureDataPlaneReadinessStatus(dataplane, dataPlaneWorkloadStatus{})
		res, err := patchDataPlaneStatus(ctx, cl, logger, dataplane)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed patching status (%s not present) for DataPlane %s/%s: %w", workloadKind, dataplane.Namespace, dataplane.Name, err)
		}
		if res {
			return ctrl.Result{}, nil
		}

	case 1: // Expect just 1.

	default: // More than 1.
		log.Info(logger, fmt.Sprintf("expected only 1 %s for DataPlane", workloadKind))
		return ctrl.Result{Requeue: true}, nil
	}

	workload := workloads[0]
	if !workload.Ready {
		log.Debug(logger, fmt.Sprintf("%s for DataPlane not ready yet", workloadKind))

		// Set Ready to false for dataplane as the underlying workload is not ready.
		k8sutils.SetCondition(
			k8sutils.NewConditionWithGeneration(
				kcfgdataplane.ReadyType,
				metav1.ConditionFalse,
				kcfgdataplane.WaitingToBecomeReadyReason,
				fmt.Sprintf("%s: %s %s is not ready yet", kcfgdataplane.WaitingToBecomeReadyMessage, workloadKind, workload.Name),
				generation,
			),
			dataplane,
		)
		ensureDataPlaneReadinessStatus(dataplane, workload)
		if _, err := patchDataPlaneStatus(ctx, cl, logger, dataplane); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed patching status (%s not ready) for DataPlane %s/%s: %w", workloadKind, dataplane.Namespace, dataplane.Name, err)
		}
		return ctrl.Result{}, nil
	}

	services, err := listDataPlaneLiveServices(ctx, cl, dataplane)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed listing ingress services for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
	}

	switch len(services) {
	case 0:
		log.Debug(logger, "Ingress Service for DataPlane not present")

		// Set Ready to false for dataplane as the Service is not ready yet.
		k8sutils.SetCondition(
			k8sutils.NewConditionWithGeneration(
				kcfgdataplane.ReadyType,
				metav1.ConditionFalse,
				kcfgdataplane.WaitingToBecomeReadyReason,
				kcfgdataplane.WaitingToBecomeReadyMessage,
				generation,
			),
			dataplane,
		)
		ensureDataPlaneReadinessStatus(dataplane, workload)
		_, err := patchDataPlaneStatus(ctx, cl, logger, dataplane)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed patching status (ingress Service not present) for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
		}
		return ctrl.Result{}, nil

	case 1: // Expect just 1.

	default: // More than 1.
		log.Info(logger, "expected only 1 ingress Service for DataPlane")
		return ctrl.Result{Requeue: true}, nil
	}

	ingressService := services[0]
	if !dataPlaneIngressServiceIsReady(&ingressService) {
		log.Debug(logger, "Ingress Service for DataPlane not ready yet")

		// Set Ready to false for dataplane as the Service is not ready yet.
		k8sutils.SetCondition(
			k8sutils.NewConditionWithGeneration(
				kcfgdataplane.ReadyType,
				metav1.ConditionFalse,
				kcfgdataplane.WaitingToBecomeReadyReason,
				fmt.Sprintf("%s: ingress Service %s is not ready yet", kcfgdataplane.WaitingToBecomeReadyMessage, ingressService.Name),
				generation,
			),
			dataplane,
		)
		ensureDataPlaneReadinessStatus(dataplane, workload)
		_, err := patchDataPlaneStatus(ctx, cl, logger, dataplane)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed patching status (ingress Service not ready) for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
		}
		return ctrl.Result{}, nil
	}

	k8sutils.SetReadyWithGeneration(dataplane, generation)
	ensureDataPlaneReadinessStatus(dataplane, workload)

	if _, err := patchDataPlaneStatus(ctx, cl, logger, dataplane); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed patching status for DataPlane %s/%s: %w", dataplane.Namespace, dataplane.Name, err)
	}

	return ctrl.Result{}, nil
}

// dataPlaneWorkloadStatus is the workload-type agnostic view of the readiness of the
// workload (a Deployment or a DaemonSet) that runs a DataPlane's Pods.
type dataPlaneWorkloadStatus struct {
	// Name is the name of the workload object.
	Name string
	// Replicas is the number of Pods the workload wants to run: a Deployment's
	// status.replicas, or a DaemonSet's status.desiredNumberScheduled.
	Replicas int32
	// ReadyReplicas is how many of those Pods are ready: a Deployment's
	// status.readyReplicas, or a DaemonSet's status.numberReady.
	ReadyReplicas int32
	// Ready reports whether all the Pods the workload wants to run are available.
	// It does not indicate that a rollout has completed: a workload can be ready
	// (all of its Pods are available) while a new spec is still rolling out.
	Ready bool
}

// dataPlaneWorkloadKind returns the Kubernetes kind of the workload running the
// DataPlane's Pods, for use in log and condition messages.
func dataPlaneWorkloadKind(dataplane *operatorv1beta1.DataPlane) string {
	return string(dataPlaneWorkloadType(dataplane))
}

// listDataPlaneLiveWorkloads lists the live workloads running the DataPlane's Pods,
// reading either Deployments or DaemonSets depending on the configured workload type.
func listDataPlaneLiveWorkloads(
	ctx context.Context,
	cl client.Client,
	dataplane *operatorv1beta1.DataPlane,
) ([]dataPlaneWorkloadStatus, error) {
	matchingLabels := client.MatchingLabels{
		"app":                                dataplane.Name,
		consts.DataPlaneDeploymentStateLabel: consts.DataPlaneStateLabelValueLive,
	}

	if dataPlaneWorkloadType(dataplane) == commonv1alpha1.WorkloadTypeDaemonSet {
		daemonSets, err := k8sutils.ListDaemonSetsForOwner(ctx, cl, dataplane.Namespace, dataplane.UID, matchingLabels)
		if err != nil {
			return nil, err
		}
		return lo.Map(daemonSets, func(ds appsv1.DaemonSet, _ int) dataPlaneWorkloadStatus {
			return dataPlaneWorkloadStatus{
				Name:          ds.Name,
				Replicas:      ds.Status.DesiredNumberScheduled,
				ReadyReplicas: ds.Status.NumberReady,
				Ready:         isDaemonSetReady(ds.Status),
			}
		}), nil
	}

	deployments, err := k8sutils.ListDeploymentsForOwner(ctx, cl, dataplane.Namespace, dataplane.UID, matchingLabels)
	if err != nil {
		return nil, err
	}
	return lo.Map(deployments, func(d appsv1.Deployment, _ int) dataPlaneWorkloadStatus {
		_, ready := isDeploymentReady(d.Status)
		return dataPlaneWorkloadStatus{
			Name:          d.Name,
			Replicas:      d.Status.Replicas,
			ReadyReplicas: d.Status.ReadyReplicas,
			Ready:         ready,
		}
	}), nil
}

func listDataPlaneLiveServices(
	ctx context.Context,
	cl client.Client,
	dataplane *operatorv1beta1.DataPlane,
) ([]corev1.Service, error) {
	return k8sutils.ListServicesForOwner(ctx,
		cl,
		dataplane.Namespace,
		dataplane.UID,
		client.MatchingLabels{
			"app":                             dataplane.Name,
			consts.DataPlaneServiceStateLabel: consts.DataPlaneStateLabelValueLive,
			consts.DataPlaneServiceTypeLabel:  string(consts.DataPlaneIngressServiceLabelValue),
		},
	)
}

// isDeploymentReady if the DataPlane's Deployment is ready.
// It does not indicate if the rollout has completed, that is a DataPlane can indicate
// that it's ready (e.g. all replicas are available) but not fully rolled out
// (e.g. new spec has not completely rolled out).
func isDeploymentReady(deploymentStatus appsv1.DeploymentStatus) (metav1.ConditionStatus, bool) {
	// We check if the Deployment is not Ready.
	// This is the case when status has replicas set to 0 or status.availableReplicas
	// in status is less than status.replicas.
	if deploymentStatus.Replicas == 0 ||
		deploymentStatus.AvailableReplicas < deploymentStatus.Replicas {
		return metav1.ConditionFalse, false
	}

	return metav1.ConditionTrue, true
}

// isDaemonSetReady reports whether the DataPlane's DaemonSet is ready, i.e. whether
// every node that should run a DataPlane Pod has an available one.
// Mirroring isDeploymentReady, a DaemonSet that doesn't want to schedule anything
// (no eligible node) is not considered ready, as it serves no traffic.
func isDaemonSetReady(daemonSetStatus appsv1.DaemonSetStatus) bool {
	return daemonSetStatus.DesiredNumberScheduled > 0 &&
		daemonSetStatus.NumberAvailable >= daemonSetStatus.DesiredNumberScheduled
}
