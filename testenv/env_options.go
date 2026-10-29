package testenv

import "github.com/cockroachlabs/cockroach-enterprise-operator/metrics"

// envOptions control the behavior of kubetest.Env.
type envOptions struct {
	skipLeakTest              bool
	metricsMgr                metrics.TenantMetricsReader
	skipTenantController      bool
	enableK8sNodeController   bool
	enableMigrationController bool
	skipOperatorDeploy        bool
	operatorImage             string
}

// EnvOption defines an option that can be passed to kubetest.Env in order to
// control its behavior.
type EnvOption func(opts *envOptions)

// SkipLeakTest skips the goroutine leak test that the Env otherwise would
// perform. This option works around known memory leaks, like those in the K8s
// controller runtime:
// https://github.com/kubernetes-sigs/controller-runtime/issues/1280
func SkipLeakTest(opts *envOptions) {
	opts.skipLeakTest = true
}

// SkipTenantController skips setting up the tenant controller when running
// the operator.
func SkipTenantController(opts *envOptions) {
	opts.skipTenantController = true
}

// EnableNodeController enables the K8s node controller when running
// the operator.
func EnableNodeController(opts *envOptions) {
	opts.enableK8sNodeController = true
}

// EnableMigrationController enables the migration controller when running
// the operator.
func EnableMigrationController(opts *envOptions) {
	opts.enableMigrationController = true
}

// SkipOperatorDeploy creates the k3d cluster and ctl without deploying the
// enterprise operator or installing CRDs. Use this when the test needs to
// control operator deployment order, for example to install a public operator
// first and deploy the enterprise operator later.
func SkipOperatorDeploy(opts *envOptions) {
	opts.skipOperatorDeploy = true
}

// TenantMetricsManager provides a manager of tenant metrics used by the
// operator to decide which pods to drain and terminate.
func TenantMetricsManager(metricsMgr metrics.TenantMetricsReader) EnvOption {
	return func(opts *envOptions) {
		opts.metricsMgr = metricsMgr
	}
}

// SetOperatorImage sets the operator container image.
func SetOperatorImage(image string) EnvOption {
	return func(opts *envOptions) {
		opts.operatorImage = image
	}
}
