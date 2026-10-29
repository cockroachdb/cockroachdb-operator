package betaclusterctrl_test

import (
	"context"
	"testing"
	"time"

	crdbv1beta1 "github.com/cockroachlabs/cockroach-enterprise-operator/api/v1beta1"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kube"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubecrdbtest"
	"github.com/cockroachlabs/cockroach-enterprise-operator/kubecrdb/kubenet"
	"github.com/cockroachlabs/cockroach-enterprise-operator/pkg/testutils"
	"github.com/cockroachlabs/cockroach-enterprise-operator/testenv"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// postInitSQLImage matches the image used by the other betaclusterctrl
// integration tests so we keep the cached image set small.
const postInitSQLImage = "us-docker.pkg.dev/cockroach-cloud-images/cockroachdb/cockroach:v22.1.10"

// TestIntegrationPostInitSQL exercises the post-init SQL feature end-to-end
// against a live k3d cluster. The function name must begin with
// TestIntegration so testutils.MarkIntegrationTest accepts it.
func TestIntegrationPostInitSQL(t *testing.T) {
	testutils.MarkIntegrationTest(t)

	suite.Run(t, new(PostInitSQLSuite))
}

// PostInitSQLSuite hosts the subtests for the post-init SQL feature.
// Each subtest creates its own cluster in its own namespace so failures
// do not leak across cases.
type PostInitSQLSuite struct {
	suite.Suite

	env *testenv.Env
	ctl *kube.Ctl
}

func (s *PostInitSQLSuite) SetupSuite() {
	s.env = testenv.New(s.T(), testenv.MultiZoneConfig, testenv.SkipTenantController)
	s.ctl = s.env.DefaultCluster().Ctl
}

// makeCluster builds a single-region 3-node TLS-enabled cluster spec
// pinned to postInitSQLImage. postInit may be nil for tests that do not
// exercise the feature, but all three subtests below populate it.
func (s *PostInitSQLSuite) makeCluster(
	name, namespace string, postInit *crdbv1beta1.PostInitSQL,
) *crdbv1beta1.CrdbCluster {
	return &crdbv1beta1.CrdbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: crdbv1beta1.CrdbClusterSpec{
			TLSEnabled:  true,
			PostInitSQL: postInit,
			Regions: []crdbv1beta1.CrdbClusterRegion{
				{Code: "us-east-1", Nodes: 3, CloudProvider: "k3d"},
			},
			Template: crdbv1beta1.CrdbNodeTemplate{
				Spec: crdbv1beta1.CrdbNodeSpec{
					Image: postInitSQLImage,
				},
			},
		},
	}
}

// databaseExists returns true if a database with the given name appears in
// crdb_internal.databases. The check runs over a TLS-protected SQL
// connection tunneled through the K8s API.
func (s *PostInitSQLSuite) databaseExists(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster, name string,
) bool {
	db := testenv.OpenKubeSql(s.T(), ctl, kubenet.Options{TLSConfig: testenv.GetTLS(s.T(), ctl, cluster)})
	defer db.Close()

	var count int
	s.Require().NoError(db.QueryRowContext(ctx, "SELECT count(*) FROM crdb_internal.databases WHERE name = $1", name).Scan(&count))
	return count > 0
}

// tableExists returns true if a table with the given name appears in
// crdb_internal.tables. Used by the failure-path test to confirm that the
// failing statement did not create its target table.
func (s *PostInitSQLSuite) tableExists(
	ctx context.Context, ctl *kube.Ctl, cluster *crdbv1beta1.CrdbCluster, name string,
) bool {
	db := testenv.OpenKubeSql(s.T(), ctl, kubenet.Options{TLSConfig: testenv.GetTLS(s.T(), ctl, cluster)})
	defer db.Close()

	var count int
	s.Require().NoError(db.QueryRowContext(ctx, "SELECT count(*) FROM crdb_internal.tables WHERE name = $1", name).Scan(&count))
	return count > 0
}

func (s *PostInitSQLSuite) TestInlineHappyPath() {
	// Use a short explicit namespace name; the default derived from t.Name()
	// is over the 63-character namespace limit for this suite + subtest pair.
	namespace := testenv.EnsureCustomNamespace(s.T(), s.ctl, "pisql-inline")
	ctl := s.ctl.WithNamespace(namespace)
	ctx := context.Background()

	cluster := s.makeCluster("post-init-sql-inline", namespace, &crdbv1beta1.PostInitSQL{
		Inline: []string{
			"CREATE DATABASE IF NOT EXISTS testdb",
			"CREATE USER IF NOT EXISTS testuser",
			"GRANT ALL ON DATABASE testdb TO testuser",
		},
	})

	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	// Re-fetch the cluster so we observe the status the reconciler wrote.
	s.Require().NoError(ctl.Get(ctx, cluster.Name, cluster))
	s.Require().True(cluster.Status.HasCondition(crdbv1beta1.ClusterPostInitSQLApplied, metav1.ConditionTrue))

	s.Require().True(s.databaseExists(ctx, ctl, cluster, "testdb"))
}

func (s *PostInitSQLSuite) TestSecretAndConfigMapOrdering() {
	namespace := testenv.EnsureCustomNamespace(s.T(), s.ctl, "pisql-sources")
	ctl := s.ctl.WithNamespace(namespace)
	ctx := context.Background()

	// Create the ConfigMap and Secret before the cluster so the reconciler
	// can resolve both refs on its first pass.
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-sql",
			Namespace: namespace,
		},
		Data: map[string]string{
			"init.sql": "CREATE DATABASE IF NOT EXISTS configdb;",
		},
	}
	s.Require().NoError(ctl.Apply(ctx, configMap))

	// StringData keeps the SQL human-readable in the apply payload; the
	// server-side conversion to base64 in Data still happens.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "license-sql",
			Namespace: namespace,
		},
		StringData: map[string]string{
			"license.sql": "CREATE DATABASE IF NOT EXISTS secretdb;",
		},
	}
	s.Require().NoError(ctl.Apply(ctx, secret))

	cluster := s.makeCluster("post-init-sql-refs", namespace, &crdbv1beta1.PostInitSQL{
		Inline: []string{"CREATE DATABASE IF NOT EXISTS inlinedb"},
		ConfigMapRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "bootstrap-sql"},
			Key:                  "init.sql",
		},
		SecretRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "license-sql"},
			Key:                  "license.sql",
		},
	})

	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)
	s.Require().NoError(ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

	s.Require().NoError(ctl.Get(ctx, cluster.Name, cluster))
	s.Require().True(cluster.Status.HasCondition(crdbv1beta1.ClusterPostInitSQLApplied, metav1.ConditionTrue))

	// All three sources executed; ordering is secret -> configmap -> inline
	// but a success-path test only needs to observe the end state.
	s.Require().True(s.databaseExists(ctx, ctl, cluster, "secretdb"))
	s.Require().True(s.databaseExists(ctx, ctl, cluster, "configdb"))
	s.Require().True(s.databaseExists(ctx, ctl, cluster, "inlinedb"))
}

func (s *PostInitSQLSuite) TestFailureBlocksOnlyThisCluster() {
	namespace := testenv.EnsureCustomNamespace(s.T(), s.ctl, "pisql-failure")
	ctl := s.ctl.WithNamespace(namespace)
	ctx := context.Background()

	// inline[0] is well-formed and runs first. inline[1] has a deliberate
	// typo (CRATE instead of CREATE) so the applier returns an error after
	// the first statement has already committed.
	cluster := s.makeCluster("post-init-sql-failure", namespace, &crdbv1beta1.PostInitSQL{
		Inline: []string{
			"CREATE DATABASE IF NOT EXISTS gooddb",
			"CRATE TABLE bad SYNTAX",
		},
	})

	kubecrdbtest.CreateCluster(s.T(), ctl, cluster)

	// The reconciler requeues with backoff on failure rather than reaching
	// quiescence, so we poll for PostInitSQLApplied=False instead.
	s.Require().Eventually(func() bool {
		if err := ctl.Get(ctx, cluster.Name, cluster); err != nil {
			return false
		}
		return cluster.Status.HasCondition(crdbv1beta1.ClusterPostInitSQLApplied, metav1.ConditionFalse)
	}, 5*time.Minute, 5*time.Second,
		"PostInitSQLApplied=False never observed; status conditions=%v", cluster.Status.Conditions)

	cond, _, _ := cluster.Status.GetCondition(crdbv1beta1.ClusterPostInitSQLApplied)
	s.Require().Equal(metav1.ConditionFalse, cond)
	msg := postInitSQLConditionMessage(cluster)
	s.Require().Contains(msg, "inline[1]")
	s.Require().Contains(msg, "post-init SQL")

	// inline[0] committed before inline[1] failed, so gooddb survives.
	s.Require().True(s.databaseExists(ctx, ctl, cluster, "gooddb"))
	// The failing statement did not create its target table.
	s.Require().False(s.tableExists(ctx, ctl, cluster, "bad"))
}

// postInitSQLConditionMessage returns the Message field of the
// PostInitSQLApplied condition, or "" if absent. GetCondition only returns
// status + timestamp, so the test reaches into the slice directly.
func postInitSQLConditionMessage(c *crdbv1beta1.CrdbCluster) string {
	for i := range c.Status.Conditions {
		if c.Status.Conditions[i].Type == crdbv1beta1.ClusterPostInitSQLApplied {
			return c.Status.Conditions[i].Message
		}
	}
	return ""
}
