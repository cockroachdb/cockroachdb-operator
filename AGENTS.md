# Agent Instructions

## Overview
This is the CockroachDB Enterprise Operator (cockroach-operator) - a Kubernetes operator that extends K8s
with CockroachDB-specific resource types like CrdbCluster, CrdbTenant, and CrdbNode to deploy and manage
CockroachDB clusters on Kubernetes.

## Local Environment & Tooling
- Prereqs (see `README.md#local-development`): Docker Desktop UI, k3d, kubectl, access to cached registries for quay.io, docker.io, and us.gcr.io.
- `make local-cluster` spins up a disposable k3d cluster plus the operator; `make clean` / `make clean/test` resets wedged clusters or registries.
- `./scripts/run-local.sh` runs the operator binary against an existing cluster, copies the operator certs from the `cockroach-operator-certs` Secret, and scales down the in-cluster operator; keep credentials outside the repo.
- To keep webhooks running in-cluster while debugging controllers locally, use `ENABLE_WEBHOOKS=true ./scripts/run-local.sh`.
- Running `go run .` manually? Pass `--local-mode`, `--certs-dir`, `--disable-leader-election=true`, and export `NODE_NAME`/`NAMESPACE` to mirror production behavior.
- **Testing namespace scoping locally**: Set `WATCH_NAMESPACE` to limit which namespaces the operator watches (e.g., `WATCH_NAMESPACE="test-ns-a,test-ns-b"`). Leave unset for global mode (watches all namespaces). See the Namespace Scoping section below for details.

## Build, Format & Lint
- `make build` compiles the operator and runs `go fmt`; failures usually mean Docker or registries are unavailable. The build injects version info from `VERSION` via ldflags.
- `make generate` refreshes CRDs/manifests/code and runs the `modernize` analyzer. Docker must be on because controller-gen images run under the hood.
- `make push/local` and `make push/dev` build + push images to the respective registries.
- Run `golangci-lint run ./...` plus `make fmt` before sending patches; CI expects both.

## Versioning & Releases
- All operator images share a single semantic version from the `VERSION` file at repo root.
- `buildinfo.OperatorVersion()` returns the version; `pkg/constants/images_ubi.go` uses it for sidecar image tags.
- `buildinfo` and `pkg/constants` each have their own `operatorVersion`. This is intentional due to import layering.
- Releases are tag-driven (push `v*` tag). See `docs/versioning-and-releases.md` for the full process.


## Code Entry Points & Repo Map (quick ref; see `README.md` for depth)
- `main.go` wires the controller manager (`manager/manager.go`).
- Reconcilers live under `controllers/` (`betaclusterctrl`, `nodectrl`, `tenantctrl`, `migrationctrl`, legacy `clusterctrl`).
- CRD helpers: `kube/` (K8s wrappers + kuberecord), `kubecrdb/` (CRDB-aware helpers), `manifests/` (resource builders + `.golden` snapshots).
- Migration utilities: `pkg/migrationutil/` (StatefulSet parser, `ParsedMigrationContext`, shared constants used by `migrationctrl`).
- Env + tooling: `pkg/infrastructure/k3d/` (local clusters/registries), `scripts/` (codegen, run-local, registry helpers).
- Tests: `testenv/`, `testmanifest/`, `util/` hold shared fixtures and harness code.

## Skills & Rules
- Available skills (invoke via `/skill-name`):
  - `/commit` — narrative commit messages in repo style
  - `/test` — recommends and runs the right test mode for what changed
  - `/run-operator` — runs the operator locally (four modes)
  - `/demystify-operations` — traces operator internals with code links
  - `/review` — reviews changed code for quality and correctness (local skill with operator-specific checks; shadows global)
  - `/codex-review` — independent second-opinion review via `roachdev codex`; run after `/review` for high-stakes controller or security changes
  - `/pr-comments` — reads and helps address open PR review comments
  - `/retro` — retrospective on the session; surfaces refactoring and tooling gaps
  - `/integration-test` — writes integration tests following the testenv/kubecrdbtest patterns
  - `/investigate` — analyzes logs, specs, and events to explain reconciliation and root-cause issues
  - `/design-doc` — converts a plan file or description into a structured design doc (big design, UX change, runbook, decision record); challenges assumptions and alternatives before drafting
  - `/rca` — writes a customer-facing Root Cause Analysis document; uses `/investigate` for the diagnostic phase
- Rules (auto-loaded each session): `.claude/rules/go-conventions.md`,
  `.claude/rules/commenting-standards.md`, `.claude/rules/testing-patterns.md`

## Code Style
- Import layering is enforced (`kube` ← `manifests` ← `controllers`); verify before any cross-package reference.
- Commit messages: narrative style (`area: subject`, prose body, co-authored-by footer), with no
  internal ticket keys or links. See the Contributing section of `README.md` and
  `.claude/skills/commit/SKILL.md` for the full format.
- Comments: block comments are full sentences with terminal punctuation; inline comments are lowercase with no terminal punctuation.
  See `.claude/rules/commenting-standards.md`.

## Where to Dig Deeper
- **Architecture & reconciliation flows** – `README.md` plus controller package docs/comments.
- **Local clusters & registries** – `pkg/infrastructure/k3d/README.md` for registry scripts, cleanup tips, troubleshooting.
- **Manifests & codegen** – `manifests/README.md`, `build/gen/README.md`, and Bazel files under `build/` for controller-gen/gomock/proto workflows.
- **Deployment & monitoring** – `deploy/README.md` and Helm chart READMEs (RBAC, metrics, promotion steps); update anytime integration-facing behavior changes.
- **Versioning & releases** – `docs/versioning-and-releases.md` for version flow, image tagging, release process, and infra prerequisites.

## Testing Modes & Flags
| Mode | When to use | Command |
| --- | --- | --- |
| Playback (default) | Fast feedback when API shapes haven’t changed | `go test ./... -ldflags "-s -w"` or targeted `go test -run TestNodePod` |
| Record | After touching Kubernetes interactions or when playback asks for re-record | `go test -run <TestName> -record -v` or `make test/record` |
| Integration | Before shipping controller or tenant changes that need real clusters/images | `make test/integration` or `go test -tags=integration -run <TestName> -v` |

Flags & notes:
- `-debug-env` preserves the test namespace for kubectl inspection.
- `-race` (via `make test/race`) is slow but required for concurrency-heavy changes.
- Tests create isolated namespaces but reuse the same k3d cluster, so keep it running for faster feedback.

## Change-Type Checklist (ties back to sections above)
| Change type | Required steps |
| --- | --- |
| API types / manifests | Update types, run `make generate`, rerun relevant playback tests, re-record fixtures if manifests or API calls changed. |
| Controller or reconciler logic | Add/adjust unit tests, run playback suite, re-record affected tests, use `-race` when touching concurrency paths, run integration tests if behavior is cluster-facing. Run `/retro` after significant refactoring to surface quality gaps. |
| Integration-facing (CRDs, Helm, deploy docs) | Run integration tests or targeted suites, update `deploy/**` artifacts, document rollout steps. |
| Local tooling or scripts | Exercise `make local-cluster` / `./scripts/run-local.sh`, update script README or inline docs, and note any new env vars. |
| Binary/image changes | `make build`, `make push/local` (or `/dev`), coordinate follow-up per `deploy/README.md`. |
| Version bump | Update `VERSION` file, update `CHANGELOG.md`, merge, then push `v<version>` tag. See `docs/versioning-and-releases.md`. |
| Submitting a PR | Run `/review` before opening; use `/pr-comments` to read and address review feedback. |

## Migration Controller

`controllers/migrationctrl` automates migration of CockroachDB clusters from Helm StatefulSets or Public Operator v1alpha1 to CockroachDB Operator v1beta1 CrdbNodes.

**Enabling**: Start the operator with `--enable-migration`. Without this flag the migration reconciler is not registered and migration labels are ignored.

**Trigger**: Apply `crdb.io/migrate=start` to the StatefulSet (Helm migration) or v1alpha1 CrdbCluster (public operator migration).

**Phase flow**: Init → CertMigration → PodMigration → Finalization → (user deletes STS) → Complete

**Completing a migration**: Finalization sets `Mode=MutableOnly` on the CrdbCluster and stops processing. The user must manually delete the StatefulSet. The delete watch event triggers a reconcile that records PhaseComplete and updates the migration label to `complete`.

## Troubleshooting & Pitfalls
- `make generate` hangs or fails if Docker Desktop is paused; resume it first.
- Local pushes require the registry at `registry.localhost:5000`; rerun the registries script if pushes time out.
- Playback “needs re-record” errors mean fixture drift—rerun the specific test with `-record` immediately to keep diffs tidy.
- Turn on `KUBERECORD_LOG_LEVEL=debug` when diagnosing playback mismatches; logs land under `testenv`.
- `./scripts/run-local.sh` exits early without a valid kube context or a readable `cockroach-operator-certs` Secret; check both before filing bugs.

## Namespace Scoping

The operator can watch specific namespaces instead of the entire cluster. This is useful for:
- Running multiple operator versions side-by-side (e.g., v2.12 for prod, v2.13 for staging)
- Limiting the operator's reconciliation scope (security-conscious deployments)
- Testing new operator versions in isolation

**Configuration**: Set `WATCH_NAMESPACE` environment variable.
- Unset or `””` = **Global mode** (watches all namespaces) — default, backward compatible
- `”prod-a”` = **Single-namespace mode** (watches only prod-a)
- `”prod-a,prod-b,staging”` = **Multi-namespace mode** (watches comma-separated list)

**RBAC**: ClusterRole + ClusterRoleBinding used for all modes. Namespace scoping is enforced by the cache, not RBAC. The operator needs cluster-scoped permissions for CRDs, webhooks, nodes, and leader election.

**Webhooks**: Mode-dependent behavior for multi-operator support (managed by operator at runtime):
- **Global mode**: Uses fixed webhook names (`cockroach-webhook-config`, `cockroach-mutating-webhook-config`) with no namespace selectors. Validates all namespaces cluster-wide (backward compatible).
- **Scoped mode**: Uses unique webhook names per deployment (`cockroach-webhook-config-{namespace}`) with namespace selectors. Validates only resources in watched namespaces.
- **Service selector**: Runtime-detected from operator Deployment pod labels. Defaults to `cockroach-operator` for all deployments.
- **Multi-operator scenarios**: Different deployment namespaces = different webhook configs (no conflicts). Same deployment namespace = not recommended (webhook requests may route to either operator non-deterministically).

**K8s Node Controller**: Intentionally ignores namespace scoping. When a K8s node is decommissioned, it finds ALL CrdbNodes on that node across all namespaces (decommissioning a physical node affects all pods on it).

**Overlapping namespaces**: Deploying multiple operators with overlapping `WATCH_NAMESPACE` values is **unsupported** except during migrations. Both operators will reconcile the same resources independently with no coordination, which is safe (thanks to K8s optimistic concurrency) but inefficient and can cause unpredictable behavior if operators run different versions.

**Webhook cleanup**: On graceful shutdown, the operator deletes its webhook configurations. With unique webhook names per deployment, each operator cleans up only its own webhooks without affecting other operators.


## Recurring Code Pitfalls (from past sessions)

These are mistakes that have come up multiple times. Treat them as rules, not suggestions.

- **Base branch is `main`, not `init`.** The `init` branch holds only the initial commit and is never used as a PR base. All feature branches target `main`.
- **Never commit `.claude/` directory files.** That directory holds session artifacts, skill definitions, and tool configs. It is gitignored for a reason. Stage files explicitly by name; never use `git add .` or `git add -A`.
- **`RequeueAfter` takes a `time.Duration`, not a bare integer.** `controllers.RequeueAfter(30, nil)` requeues after 30 nanoseconds. Always write `30*time.Second` or a named constant.
- **Call `ctl.UpdateStatus` before `NoRequeue()`.** If you mutate an object's status in memory and then return `NoRequeue()` without persisting, the change is lost across restarts. Status updates must come first.
- **Avoid magic array or slice indices.** Do not write `items[0]` or `containers[2]` without a comment or a named variable that makes the index unambiguous. If the index is meaningful, extract it as a named constant or verify the length first.
- **Check adjacent code before adding new resource manipulation.** Before writing owner reference logic, patch logic, or RBAC additions, look at how the nearest existing code does the same thing. This catches pattern deviations and prevents unintentional RBAC expansions.
- **Verify RBAC changes do not grant unintended access.** When adding a new verb or resource to `cockroach-operator.yaml`, scan the ClusterRole for the existing permission surface. Never add `*` wildcards without explicit sign-off.
- **Read adjacent test files before writing new ones.** The test file that is already in the same package is the authoritative example for the harness, fixture paths, and wait helpers used there.
