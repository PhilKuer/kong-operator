package v1alpha1

// WorkloadType selects the Kubernetes workload resource that the operator
// creates to run a DataPlane's Pods.
//
// +kubebuilder:validation:Enum=Deployment;DaemonSet
type WorkloadType string

const (
	// WorkloadTypeDeployment makes the operator run the DataPlane's Pods with a
	// Deployment. This is the default.
	WorkloadTypeDeployment WorkloadType = "Deployment"

	// WorkloadTypeDaemonSet makes the operator run the DataPlane's Pods with a
	// DaemonSet, which schedules exactly one Pod on every eligible node.
	WorkloadTypeDaemonSet WorkloadType = "DaemonSet"
)
