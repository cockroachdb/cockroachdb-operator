# cockroach-operator

cockroach-operator is a [Kubernetes operator](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
that extends Kubernetes (K8s) with CockroachDB (CRDB) resource types, so a CRDB
cluster can be deployed and operated with the usual `kubectl` workflow. A
`CrdbCluster` describes the cluster, and the operator manages one `CrdbNode` per
CockroachDB node, each owning a single Pod and its volumes, instead of using a
StatefulSet. It also supports multi-tenant clusters through `CrdbTenant`, and
can migrate existing clusters from the Helm chart or the
[public operator](https://github.com/cockroachdb/cockroach-operator).

A K8s operator consists of two major parts:

* Custom resource definitions (e.g. `CrdbCluster` or `CrdbTenant`) that the
  operator adds to the built-in set of K8s resources (e.g. `Pod` or
  `Deployment`). The familiar `create`, `replace`, `patch`, `get`, etc. K8s
  operations are now available for the new resource types.

* Custom controllers that watch for changes to the desired state of these custom
  resources and then update the cluster to match that state (a process called
  "reconciliation"). Custom controllers are implemented in Go code and are run
  by a K8s manager component which is itself part of a `Deployment` resource
  installed in the K8s cluster.

## Installing

The operator is installed with the `cockroachdb-operator` Helm chart in
[cockroachdb/helm-charts](https://github.com/cockroachdb/helm-charts/tree/master/cockroachdb-operator).
The chart's README covers installation, configuration, and migrating existing
clusters.

## Versioning and Releases

The operator uses semantic versioning. All three images (operator, init-container,
cert-reloader) share a single version from the `VERSION` file at the repo root.
Releases are tag-driven. Push a `v*` tag to build and publish. See
[docs/versioning-and-releases.md](docs/versioning-and-releases.md) for the full
release process and local testing instructions.

## Development

A rapid development cycle requires minimal turnaround between making a change to
configuration or code and then testing that change. The operator uses K3D and
the `testenv` test package to enable rapid development. K3D is a full-featured
version of K8s that runs in Docker containers on your local machine (see
[this README](pkg/infrastructure/k3d/README.md) for more details). The
`testenv` package can run the operator in the test process rather than in the
K3D cluster. This avoids the slow process of building the operator's Docker
image after every change. It also enables convenient debugging of the operator
code, since it's running in the test process.

Typical development workflow goes like this: find an existing test (e.g.
`TestIntegrationCluster/TestCreate`) that exercises the functionality you're
working on (or create a new test). Then, after each change you make, run that
test using:

    go test -run TestXXX -debug-env -record -v

The operator tests reuse K3D clusters so they don't need to be spun up by every
test. In addition, each test installs its K8s resources in a namespace that is
created specifically for that test. The `debug-env` flag instructs the test
framework to not tear down that namespace at the end of the test (as well as any
CRDB clusters in it), which in turn means you can more rapidly iterate. The `v`
flag shows the logs as part of the verbose test output. The `record` flag is
discussed below.

### Development using live cluster

Sometimes it's useful to run the operator from your machine against a live
cluster. A script for this is provided in `scripts/run-local.sh`:

1. Connect to the cluster
1. Scale down the existing operator deployment to 0
1. Download the certs from the namespaced Secrets
1. Run the operator locally via `go run` with the following envs and flags:
   ```
    NODE_NAME="$node_name" NAMESPACE="$namespace" go run . -metrics-addr=0.0.0.0:8080 -rpc-addr=0.0.0.0:9070 --local-mode --skip-tenant-controller=true --disable-leader-election=true --certs-dir="$certs_dir"
    ```
    - `--local-mode`: instructs leader election to use the passed in namespace,
      and skip the validating admission webhook
    - `NODE_NAME`: is used to figure out which cloud provider the cluster is
      on, just use the first result of `kubectl get nodes`
    - `NAMESPACE`: is the namespace the operator is supposed to be running in
    - `--disable-leader-election`: ensures the local process doesn't fight for leader lease, 
      allowing it to run as a standalone controller.
    - `$certs_dir` is where the downloaded certs are
   
### Running with Webhooks Enabled (Advanced)

Sometimes you need webhooks running while you're testing new controller changes.
Since the K8s API server can't reach your local machine, we can use a trick where
we leave one operator pod running in the cluster *just* to serve webhooks, while you run the controllers locally.

```bash
ENABLE_WEBHOOKS=true ./scripts/run-local.sh
```

This will:
- Patch the in-cluster operator to run in `--webhook-only` mode.
- Keep 1 replica running there.
- Run your local operator for controllers with leader election disabled so it doesn't fight the webhook pod.

### Testing Namespace Scoping

The operator supports watching specific namespaces instead of the entire cluster. To test this locally:

```bash
# Watch only test-ns-a and test-ns-b
WATCH_NAMESPACE="test-ns-a,test-ns-b" ./scripts/run-local.sh

# Or run manually
WATCH_NAMESPACE="test-ns-a,test-ns-b" NODE_NAME="node-1" NAMESPACE="default" \
  go run . --local-mode --certs-dir=./certs --disable-leader-election
```

The operator will only reconcile CrdbClusters/CrdbTenants in the watched namespaces. Leave `WATCH_NAMESPACE` unset for global mode (default).

### Testing Leader Election locally

By default, our script disables leader election to keep things simple. But if you're working on the election logic itself, you'll need to enable it.

**To test leader election:**
1.  Don't use the script (it forces the flag).
2.  Run `go run` manually.
3.  Omit the `--disable-leader-election` flag.

For example, to simulate two operators fighting for the lead, run this in two separate terminals:

```bash
# Terminal 1
NODE_NAME="node-1" NAMESPACE="default" go run . --local-mode --certs-dir=./certs

# Terminal 2
NODE_NAME="node-1" NAMESPACE="default" go run . --local-mode --certs-dir=./certs
```
*Note: You'll need to download the certs manually first if you aren't using the script.*


## Contributing

These rules apply to everyone, maintainers included. Everything in this
repository, including commit messages, PR descriptions and review comments, is
public.

### Scope

Bug fixes, improvements and features that benefit operator users in general are
welcome. Organization-specific customizations should stay in your fork.
Integrations with other platforms, such as service meshes, ingress controllers
or GitOps tools, need a GitHub issue to discuss them before you write code.
Maintainers may decline integrations that are outside the project's scope.

### Before you start

- For anything larger than a small fix, open a GitHub issue that describes the
  problem and your proposed approach. For a bug, include the steps to reproduce
  it: the operator version, the CrdbCluster spec, and what you expected versus
  what happened.
- We usually don't accept changes that only fix spelling or wording in comments,
  unless they're part of a larger change.

### Making a change

1. Add or update tests. The [Testing](#testing) section explains playback,
   record and integration modes. If you change API types, run `make generate`.
   If you change Kubernetes API calls or generated manifests, re-record the
   affected fixtures.
2. Run `make fmt`, `golangci-lint run ./...` and the tests for the packages you
   touched. CI fails if `make generate` or `make modernize` changes any files,
   and runs `make test`.
3. If the change affects users, add an entry to `CHANGELOG.md` for the next
   release.
4. Open a pull request against `main`.

### Commit messages

PRs are squash-merged, so the PR title and description become the commit
message. Write them in this format:

```
area: short description in the imperative

Explain what changed and why. Describe the behavior before and after
if that helps. The diff already shows how, so don't repeat it.

Fixes #123
```

- Start the subject with the area you changed, such as `nodectrl`,
  `betaclusterctrl`, `migrationctrl`, `manifests`, `api` or `docs`. Use `*:`
  when a change spans many areas.
- Use lowercase after the colon and the imperative mood ("fix", not "fixed" or
  "fixes"). Keep it under 72 characters, with no trailing period.
- Wrap the body at 72 characters.
- Reference GitHub issues with `Fixes #123` or `See #456`.
- Credit co-authors with a `Co-authored-by: Name <email>` trailer.

### Keep it public

- Put the reasoning in the commit or comment itself instead of linking to it.
  Readers can't open private documents, chat threads or issue trackers.
- Don't reference internal ticket keys, internal tools, internal system names,
  hostnames, cluster IDs or private repositories.
- Don't name customers or describe a specific customer's environment.
- Don't commit credentials, keys or tokens, even expired or test ones. Test key
  material lives in `crypto/` and must come from the deterministic test signer.
  Before pushing new recorded fixtures, run `scripts/scan-vcr-fixtures.sh` and
  `gitleaks detect --no-git`.

If you track this work in a private tracker, link the PR from there rather than
putting the tracker's key in the commit.

### Code review

- Every PR needs an approving review from a maintainer before it merges.
- Reviewers prefix optional style suggestions with `nit:`.
- Reply to each comment with a fix or with the reason you're leaving it. The
  reviewer resolves the thread.

### Code style

Follow the conventions in `.claude/rules/`, which apply to all contributors:

- [go-conventions.md](.claude/rules/go-conventions.md) covers import layering,
  error handling, logging and reconciler return helpers.
- [commenting-standards.md](.claude/rules/commenting-standards.md) covers how
  comments should read.
- [testing-patterns.md](.claude/rules/testing-patterns.md) covers the test modes,
  fixtures and golden files.

## Namespace Scoping

By default, the operator watches all namespaces (global mode). You can optionally scope it to watch specific namespaces.

**Why use namespace scoping?**
- Run multiple operator versions side-by-side (e.g., one version managing prod namespaces, a newer one managing staging)
- Limit the operator's reconciliation scope for security-conscious deployments (RBAC remains cluster-scoped)
- Test new operator versions in isolation before global rollout

**How to enable**: Set the `WATCH_NAMESPACE` environment variable (see Helm chart documentation for production deployments).

**Modes**:
- **Global** (default): `WATCH_NAMESPACE=""` or unset → watches all namespaces
- **Single namespace**: `WATCH_NAMESPACE="prod-a"` → watches only prod-a
- **Multi-namespace**: `WATCH_NAMESPACE="prod-a,prod-b,staging"` → watches comma-separated list

**Important**: Deploy multiple operators in separate namespaces to avoid conflicts. Don't run operators with overlapping `WATCH_NAMESPACE` values except during short migrations.

## Testing

K3D has minimal overhead for a K8s distribution, but it is still resource-hungry
and slow-running on a development machine. Every integration test that uses K3D
can take 30-60 seconds to run and cannot be run in parallel with other tests
because they are so resource intensive. The long run times make the K3D
integration tests unsuitable for running as part of our continuous integration
(CI) pipeline and tedious to run locally. To solve these problems, the operator
uses the `kuberecord` package to run the integration tests 100x faster, in
parallel, while still retaining most of their efficacy. `kuberecord` enables
this by recording and playing back calls to the K8s HTTP API made by the
integration tests.

When writing an integration test, consider running that test in two modes: first
as a full integration test that starts up a local K8s cluster (using K3D), and
second as a unit test that uses `kuberecord` to run quickly and cheaply. Doing
this is not difficult, as long as you use `testenv.Env` to create your K8s
clusters:

  ```go
  env := testenv.New(t, kubetest.SingleZoneConfig)
  ```

By default, `testenv.Env` automatically activates `kuberecord` for any K8s
configs it creates. You can record the test for later playback using this
command:

  ```bash
  go test -run TestType_Method -record -v
  ```

After a disruptive change has invalidated previous recordings, you can use the
`make test/record` command to re-record all failing tests, rather than
re-recording each one individually.

Sometimes you want to run tests in an environment that is as close to production
as possible. Here is the command to build the operator's Docker image and use it
to run all integration tests:

  ```bash
  make test/integration
  ```

This disables `kuberecord` and runs the operator within the K8s cluster rather
than in the test process. You can also run an individual test this way:

  ```bash
  go test -run TestType_Method -tags=integration -v
  ```

## Code Structure

Here is a brief overview of important directories within the project:

`api/` Defines new custom resources introduced by the operator.

`autoscaler/` The autoscaler monitors load on each SQL pod assigned to a tenant,
  and then dynamically scales the number of pods for that tenant up or down to
  match the load.

`controllers/` Contains the code for the operator's custom controllers. When a
  custom resource changes, K8s calls the corresponding controller, which is then
  responsible for reconciling the actual state of the resource with its desired
  state.

`controllers/migrationctrl/` Automates migration of CockroachDB clusters from
  Helm StatefulSets or Public Operator v1alpha1 CrdbClusters to CockroachDB
  Operator v1beta1 CrdbNodes. Enabled via `--enable-migration`. Triggered by
  applying `crdb.io/migrate=start` to the source resource (StatefulSet for Helm
  migrations, CrdbCluster for public operator migrations). Phases: Init →
  CertMigration → PodMigration → Finalization → Complete. Finalization sets
  `Mode=MutableOnly` on the v1beta1 CrdbCluster, at which point the user must
  manually delete the StatefulSet to trigger the final Phase=Complete transition.

`kube/` All changes to the K8s cluster are made through the `kube.Ctl`
  interface, which is a close analogue of the familiar `kubectl` tool. This
  package also contains a number of other utilities for working with K8s. It
  is intended to contain only CRDB-agnostic code and may be moved in the future
  to its own repository where other K8s projects can reuse its functionality.

`kubecrdb/` Contains utilities and helper classes for working with CRDB
  components hosted in K8s. Unlike the `kube` package, the code is specific to
  CRDB and are not generally useful to other projects.

`kubecrdb/crdbcerts/` Contains code to create, get, and delete K8s secrets used
  to store CRDB certificates.

`kubecrdb/kubenet/` Helper classes to establish SQL and DBConsole connections
  to pods in a CRDB cluster hosted in K8s (Proxy, SQL, and CRDB pods).

`manager/` This is the entry point to the operator, which starts up the
  controllers, health endpoint, webhook, autoscaler, and other operator
  components.

`manifests/` Contains K8s manifests (constructed from YAML config files or by
  custom Go code), both for the operator itself and for the resources the
  operator creates as part of doing its job.

`testenv/` Operator tests use `testenv.Env` to create local K3d
  clusters that match a desired topology.

## Licensing

This project is licensed under the Apache License, Version 2.0. See
[LICENSE](LICENSE).

## FAQ

### How was this code created?

The initial code was generated by the K8s operator SDK, based on this
[tutorial](https://sdk.operatorframework.io/docs/building-operators/golang/tutorial/).
The directory structure and file naming was influenced by that tool, but has
also undergone many changes.

### What about the public K8s operator?

The [public K8s operator](https://github.com/cockroachdb/cockroach-operator)
only handles the operation of sole-tenant CRDB clusters. By contrast, this
operator adds functionality to operate multi-tenant CRDB clusters and includes
a migration controller for moving clusters off the public operator and the
Helm chart. There is overlap between the two code bases.

### What version of K8s does the operator run require?

The operator requires a minimum version of Kubernetes 1.18.
