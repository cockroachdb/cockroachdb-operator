package testenv

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube/kuberecord"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/logging"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/tenant"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CreateCrdbTenant constructs a tenant matching the given spec on the CRDB host
// cluster contained in the namespace referenced by "ctl". It returns the CRD
// for the new tenant.
// NOTE: The given name is *not* set as as the CrdbTenant resource name.
// Instead, the name is set as a ClusterName label. This allows for tenant
// cluster rename support in the future.
func CreateCrdbTenant(
	t *testing.T, ctl *kube.Ctl, name string, spec *crdbv1beta1.CrdbTenantSpec,
) *crdbv1beta1.CrdbTenant {
	ctx := context.Background()

	kubecrdbtest.InstallTenantCerts(t, ctl, spec.TenantID)

	// If kuberecord is active, then hook the tenantctrl.SendSqlStarterRequest
	// method in order to intercept calls that activate the sqlstarter process
	// that starts up a tenant pod.
	var hookSqlStarter func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error)
	switch kuberecord.Mode() {
	case kuberecord.ModeRecording:
		// When recording, use the pod dialer to make the HTTP call to the
		// sqlstarter process.
		hookSqlStarter = func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
			// Remove any kuberecord wrapper, since kuberecord does not support
			// the pod dialer protocol.
			restConfig := *ctl.RestConfig()
			restConfig.WrapTransport = nil
			dialer, err := kube.NewPodDialer(&restConfig, ctl.Namespace())
			require.NoError(t, err)
			client.Transport.(*http.Transport).DialContext = dialer.DialContext

			// Remap the request to use the pod name rather than the pod IP.
			req.URL.Host = fmt.Sprintf("%s:%s", pod.Name, req.URL.Port())
			return client.Do(req)
		}

	case kuberecord.ModeReplaying:
		// When playing back, simply return EOF, which is what the sqlstarter
		// process returns when it's successfully replaced itself with CRDB.
		hookSqlStarter = func(client *http.Client, req *http.Request, pod *corev1.Pod) (*http.Response, error) {
			return nil, &url.Error{Err: io.EOF}
		}
	}

	if hookSqlStarter != nil {
		t.Cleanup(testutils.HookGlobal(&tenant.SendSqlStarterRequest, hookSqlStarter))
	}

	// Name of tenant is always tenant-N.
	tenant := &crdbv1beta1.CrdbTenant{
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.TenantID.Tag(),
			Labels: map[string]string{
				crdbv1beta1.TenantClusterNameLabel: name,
			},
		},
		Spec: *spec,
	}

	// Register artifact capture before ctl.Apply, whose readiness wait fails via
	// t.FailNow on timeout and would skip a later registration.
	writeArtifactsOnFailed(t)

	require.NoError(t, ctl.Apply(ctx, tenant))

	// If debugEnv is on - set a root password to be 'root'. This allows
	// inspecting the tenant cluster after the test is over.
	if *debugEnv {
		db, err := openKubeSql(ctl, kubenet.Options{TenantID: spec.TenantID})
		if err != nil {
			logging.Warnf(ctx, "failed to open db for altering root's password for debug env %v", err)
			return tenant
		}
		defer db.Close()
		_, _ = db.Exec("alter user root with password 'root'")
	}
	return tenant
}
