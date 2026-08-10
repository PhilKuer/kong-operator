package dataplane

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakectrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/kong/kong-operator/v2/api/common/v1alpha1"
	operatorv1beta1 "github.com/kong/kong-operator/v2/api/gateway-operator/v1beta1"
	"github.com/kong/kong-operator/v2/pkg/consts"
)

func TestDataPlaneWorkloadType(t *testing.T) {
	testCases := []struct {
		name     string
		spec     commonv1alpha1.WorkloadType
		expected commonv1alpha1.WorkloadType
	}{
		{
			name:     "unset defaults to Deployment",
			spec:     "",
			expected: commonv1alpha1.WorkloadTypeDeployment,
		},
		{
			name:     "explicit Deployment",
			spec:     commonv1alpha1.WorkloadTypeDeployment,
			expected: commonv1alpha1.WorkloadTypeDeployment,
		},
		{
			name:     "explicit DaemonSet",
			spec:     commonv1alpha1.WorkloadTypeDaemonSet,
			expected: commonv1alpha1.WorkloadTypeDaemonSet,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dataplane := &operatorv1beta1.DataPlane{
				Spec: operatorv1beta1.DataPlaneSpec{
					DataPlaneOptions: operatorv1beta1.DataPlaneOptions{
						Deployment: operatorv1beta1.DataPlaneDeploymentOptions{
							WorkloadType: tc.spec,
						},
					},
				},
			}
			assert.Equal(t, tc.expected, dataPlaneWorkloadType(dataplane))
		})
	}
}

func TestIsDaemonSetReady(t *testing.T) {
	testCases := []struct {
		name     string
		status   appsv1.DaemonSetStatus
		expected bool
	}{
		{
			name:     "no node wants a Pod",
			status:   appsv1.DaemonSetStatus{},
			expected: false,
		},
		{
			name: "a Pod is missing on one node",
			status: appsv1.DaemonSetStatus{
				DesiredNumberScheduled: 3,
				NumberReady:            2,
				NumberAvailable:        2,
			},
			expected: false,
		},
		{
			name: "a Pod is available on every node",
			status: appsv1.DaemonSetStatus{
				DesiredNumberScheduled: 3,
				NumberReady:            3,
				NumberAvailable:        3,
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, isDaemonSetReady(tc.status))
		})
	}
}

func TestDeleteDataPlaneWorkloadsOfOtherType(t *testing.T) {
	const (
		namespace = "default"
		dpUID     = "test-uid"
	)

	dataPlaneWith := func(workloadType commonv1alpha1.WorkloadType) *operatorv1beta1.DataPlane {
		return &operatorv1beta1.DataPlane{
			ObjectMeta: metav1.ObjectMeta{
				UID:       dpUID,
				Name:      "test",
				Namespace: namespace,
			},
			Spec: operatorv1beta1.DataPlaneSpec{
				DataPlaneOptions: operatorv1beta1.DataPlaneOptions{
					Deployment: operatorv1beta1.DataPlaneDeploymentOptions{
						WorkloadType: workloadType,
					},
				},
			},
		}
	}

	ownedMeta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				consts.GatewayOperatorManagedByLabel: consts.DataPlaneManagedLabelValue,
			},
			// The operator sets this finalizer on every workload it owns; it must be
			// removed before the object can actually go away.
			Finalizers: []string{consts.DataPlaneOwnedWaitForOwnerFinalizer},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "gateway-operator.konghq.com/v1beta1",
					Kind:       "DataPlane",
					Name:       "test",
					UID:        dpUID,
				},
			},
		}
	}

	deployment := &appsv1.Deployment{ObjectMeta: ownedMeta("dataplane-deployment-1")}
	daemonSet := &appsv1.DaemonSet{ObjectMeta: ownedMeta("dataplane-daemonset-1")}

	testCases := []struct {
		name                 string
		dataPlane            *operatorv1beta1.DataPlane
		objects              []client.Object
		expectedDeleted      bool
		expectDeploymentGone bool
		expectDaemonSetGone  bool
	}{
		{
			name:                 "switching to DaemonSet removes the leftover Deployment",
			dataPlane:            dataPlaneWith(commonv1alpha1.WorkloadTypeDaemonSet),
			objects:              []client.Object{deployment.DeepCopy(), daemonSet.DeepCopy()},
			expectedDeleted:      true,
			expectDeploymentGone: true,
		},
		{
			name:                "switching back to Deployment removes the leftover DaemonSet",
			dataPlane:           dataPlaneWith(commonv1alpha1.WorkloadTypeDeployment),
			objects:             []client.Object{deployment.DeepCopy(), daemonSet.DeepCopy()},
			expectedDeleted:     true,
			expectDaemonSetGone: true,
		},
		{
			name:      "an unset workload type is treated as Deployment",
			dataPlane: dataPlaneWith(""),
			objects:   []client.Object{deployment.DeepCopy(), daemonSet.DeepCopy()},

			expectedDeleted:     true,
			expectDaemonSetGone: true,
		},
		{
			name:            "nothing to do when only the configured workload exists",
			dataPlane:       dataPlaneWith(commonv1alpha1.WorkloadTypeDaemonSet),
			objects:         []client.Object{daemonSet.DeepCopy()},
			expectedDeleted: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, operatorv1beta1.AddToScheme(scheme))

			fakeClient := fakectrlruntimeclient.
				NewClientBuilder().
				WithScheme(scheme).
				WithObjects(append(tc.objects, tc.dataPlane)...).
				Build()

			deleted, err := deleteDataPlaneWorkloadsOfOtherType(t.Context(), fakeClient, logr.Discard(), tc.dataPlane, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedDeleted, deleted)

			var deployments appsv1.DeploymentList
			require.NoError(t, fakeClient.List(t.Context(), &deployments, client.InNamespace(namespace)))
			var daemonSets appsv1.DaemonSetList
			require.NoError(t, fakeClient.List(t.Context(), &daemonSets, client.InNamespace(namespace)))

			if tc.expectDeploymentGone {
				assert.Empty(t, deployments.Items, "the Deployment should have been deleted")
			}
			if tc.expectDaemonSetGone {
				assert.Empty(t, daemonSets.Items, "the DaemonSet should have been deleted")
			}
			// The workload matching the configured type is always left alone.
			if dataPlaneWorkloadType(tc.dataPlane) == commonv1alpha1.WorkloadTypeDaemonSet {
				assert.Len(t, daemonSets.Items, 1)
			} else {
				assert.Len(t, deployments.Items, 1)
			}
		})
	}
}
