# Skill: integration-test

Write an integration test for a controller or reconciler change, following the established
patterns in this codebase.

## Test file conventions

Integration tests live alongside the package they test, named `integration_test.go` or
`reconciler_integration_test.go`. They use a `_test` package suffix. Tests are guarded with
`testutils.MarkIntegrationTest(t)` as the very first line, which skips the test unless the
integration build tag or environment is active.

Suite-based tests use testify's suite package and are structured as follows. This is the
pattern for multi-test files where shared setup is expensive.

```go
func TestIntegration<Name>(t *testing.T) {
    testutils.MarkIntegrationTest(t)
    suite.Run(t, new(TestSuite))
}

type TestSuite struct {
    suite.Suite
    env *testenv.Env
    ctl *kube.Ctl
}

func (s *TestSuite) SetupSuite() {
    s.env = testenv.New(s.T(), testenv.MultiZoneConfig, <optional opts>)
    ctl := s.env.DefaultCluster().Ctl
    s.ctl = ctl.WithNamespace(testenv.EnsureNamespace(s.T(), ctl))
}
```

Standalone integration tests (no suite) look like:

```go
func TestIntegration<Name>(t *testing.T) {
    testutils.MarkIntegrationTest(t)
    env := testenv.New(t, testenv.MultiZoneConfig)
    ...
}
```

## Environment setup helpers

`testenv.New(t, config, opts...)` provisions the test environment. Common configs:
- `testenv.MultiZoneConfig` — single-region, multi-zone (most tests)
- Custom `testenv.EnvConfig{Name, Reuse, Regions}` — for multi-region tests

Optional opts:
- `testenv.SkipTenantController` — omit tenant controller when not under test

`testenv.EnsureNamespace(t, ctl)` creates a fresh isolated namespace for each test method.
Each test method should get its own namespace to avoid state leakage.

`env.DefaultCluster().Ctl` gives the controller client. Use `ctl.WithNamespace(ns)` before
operations that are namespace-scoped.

## Creating resources

```go
// Create a CrdbCluster and wait for it to be quiescent
kubecrdbtest.CreateCluster(t, ctl, cluster)
require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))

// Helper that creates a cluster with defaults applied
cluster := testenv.CreateCrdbCluster(t, env, "cluster-name", &crdbv1beta1.CrdbClusterSpec{...})

// Create a tenant
tenant := testenv.CreateCrdbTenant(t, ctl, "tenant-name", &crdbv1beta1.CrdbTenantSpec{...})

// List all CrdbNodes for a cluster
nodes := kubecrdbtest.ClusterNodes(t, ctl, cluster)

// Install certs for TLS clusters
kubecrdbtest.InstallClusterCerts(t, ctl, isHostCluster)
```

## Waiting and polling

```go
// Wait for a resource to reach a desired state
require.NoError(t, ctl.WaitFor(ctx, kube.IsQuiescent, cluster))
require.NoError(t, ctl.WaitFor(ctx, kube.IsReady, cluster))
require.NoError(t, ctl.WaitFor(ctx, kube.IsDeleted, node))

// Eventually for conditions that need polling
require.Eventually(t, func() bool {
    require.NoError(t, ctl.Get(ctx, cluster.Name, cluster))
    return cluster.Status.HasCondition(crdbv1beta1.ClusterInitialized, metav1.ConditionTrue)
}, 30*time.Second, time.Second)

// Never for asserting something does NOT happen
require.Never(t, func() bool {
    return len(kubecrdbtest.ClusterNodes(t, ctl, cluster).Items) > 0
}, 5*time.Second, time.Second)
```

## Status conditions

Check CrdbNode conditions directly:
```go
require.True(t, node.Status.HasCondition(crdbv1beta1.PodReady, metav1.ConditionTrue))
require.True(t, node.Status.HasCondition(crdbv1beta1.PodRunning, metav1.ConditionTrue))

status, ts, exists := node.Status.GetCondition(crdbv1beta1.PodRunning)
```

## Decommissioning in tests

Scale-down tests must disable replication first on the k3d single-node setup:
```go
testenv.DisableReplication(t, ctl, cluster)
```

Then mark a node for decommission:
```go
node.MarkForDecommission()
require.NoError(t, ctl.Apply(ctx, &node))
```

## Refreshing resources

Resources go stale. Always re-fetch before applying changes:
```go
require.NoError(t, ctl.Get(ctx, cluster.Name, cluster))
cluster.Spec.Template.Spec.Image = newImage
require.NoError(t, ctl.Apply(ctx, cluster))
```

A helper method makes this clean in suite tests:
```go
func (s *TestSuite) refreshCluster(cluster *crdbv1beta1.CrdbCluster) {
    require.NoError(s.T(), s.ctl.WithNamespace(cluster.Namespace).Get(context.Background(), cluster.Name, cluster))
}
```

## Multi-region tests

Mark flaky tests if they have known intermittent issues:
```go
testutils.MarkFlakyTest(t, "CC-XXXXX")
testutils.MarkIntegrationTest(t)
```

Use `testenv.EnvConfig` with explicit region maps and set `Reuse: true` to reuse an
expensive multi-region cluster across runs during development.

## SQL in tests

```go
row := testenv.QueryRowKubeSql(t, ctl, kubenet.Options{}, "SHOW CLUSTER SETTING VERSION")
require.NoError(t, row.Scan(&version))

testenv.ExecKubeSql(t, ctl, kubenet.Options{}, "SET CLUSTER SETTING ...")

// For tenant SQL
row := testenv.QueryRowKubeSql(t, ctl, kubenet.Options{TenantID: tenantID}, "SHOW CLUSTER SETTING VERSION")
```

## Workflow

1. Ask what controller behavior is being tested (scale-up, decommission, rolling update, TLS, etc.).
2. Read the existing test file for this package before writing anything. It is the authoritative
   example for which harness, wait conditions, and namespace setup pattern to use.
3. Check if a suite already exists for this package; prefer adding a method to it over creating a new file.
4. Identify which wait conditions, helpers, and cluster configuration are needed.
5. Generate the test using the patterns above.
6. Self-verify before handing back to the user:
   - The test starts with `testutils.MarkIntegrationTest(t)` as the very first line.
   - Every test method calls `testenv.EnsureNamespace` and does not share state with siblings.
   - Scale-down or decommission tests call `testenv.DisableReplication` before marking nodes.
   - No `time.Sleep` calls are present.
7. Confirm the k3d cluster is running (`k3d cluster list`) and run `make push/local` if the
   operator image changed.
8. Integration tests run with: `go test -tags=integration -run TestName -v` or `make test/integration`.

## Things to avoid

Do not use `time.Sleep` to wait for conditions — use `ctl.WaitFor` or `require.Eventually`.
Do not share namespaces between test methods — call `testenv.EnsureNamespace` per method.
Do not leave decommission without `testenv.DisableReplication` on k3d.
Do not use `ctl.Update` when you mean `ctl.Apply`; `Apply` is server-side apply and is
preferred for spec updates. Use `ctl.Update` only when you need to patch status or when
the existing code already uses it for that resource.
