package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	operatorv1beta1 "github.com/kong/kong-operator/v2/api/gateway-operator/v1beta1"
	"github.com/kong/kong-operator/v2/pkg/consts"
)

func TestDaemonSetFromDeployment(t *testing.T) {
	dataplane := &operatorv1beta1.DataPlane{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "gateway-operator.konghq.com/v1beta1",
			Kind:       "DataPlane",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dp-1",
			Namespace: "test-namespace",
		},
		Spec: operatorv1beta1.DataPlaneSpec{
			DataPlaneOptions: operatorv1beta1.DataPlaneOptions{
				Deployment: operatorv1beta1.DataPlaneDeploymentOptions{
					DeploymentOptions: operatorv1beta1.DeploymentOptions{
						Replicas: new(int32(3)),
					},
				},
			},
		},
	}

	deployment, err := GenerateNewDeploymentForDataPlane(dataplane, "kong:3.9")
	require.NoError(t, err)
	// Something that only lives on the ObjectMeta, to verify metadata carries over.
	deployment.Annotations = map[string]string{"example.com/annotation": "value"}

	daemonSet := DaemonSetFromDeployment(deployment.Unwrap()).Unwrap()

	t.Run("identity and metadata are carried over", func(t *testing.T) {
		assert.Equal(t, deployment.Namespace, daemonSet.Namespace)
		assert.Equal(t, deployment.GenerateName, daemonSet.GenerateName)
		assert.Equal(t, deployment.Labels, daemonSet.Labels)
		assert.Equal(t, deployment.Annotations, daemonSet.Annotations)
		assert.Equal(t, deployment.OwnerReferences, daemonSet.OwnerReferences)
		assert.Contains(t, daemonSet.Finalizers, consts.DataPlaneOwnedWaitForOwnerFinalizer)
	})

	t.Run("selector and Pod template are shared with the Deployment", func(t *testing.T) {
		assert.Equal(t, deployment.Spec.Selector, daemonSet.Spec.Selector)
		assert.Equal(t, deployment.Spec.Template, daemonSet.Spec.Template)
	})

	t.Run("rolling update strategy is set", func(t *testing.T) {
		assert.Equal(t, appsv1.RollingUpdateDaemonSetStrategyType, daemonSet.Spec.UpdateStrategy.Type)
		require.NotNil(t, daemonSet.Spec.UpdateStrategy.RollingUpdate)
		assert.Equal(t,
			&intstr.IntOrString{Type: intstr.Int, IntVal: 1},
			daemonSet.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable,
		)
	})

	t.Run("mutating the DaemonSet does not affect the source Deployment", func(t *testing.T) {
		daemonSet.Spec.Template.Spec.Containers[0].Image = "kong:changed"
		assert.Equal(t, "kong:3.9", deployment.Spec.Template.Spec.Containers[0].Image)
	})
}
