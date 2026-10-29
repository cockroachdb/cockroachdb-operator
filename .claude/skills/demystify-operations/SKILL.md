# Skill: demystify-operations

Trace how a CockroachDB operator operation works internally — from the triggering condition
through the reconciler state machine, kube/kubecrdb helpers, and down to the K8s API calls.

## Usage

```
/demystify-operations <operation>
```

Or invoke with no argument to be prompted.

## Supported operations

| Operation | What it traces |
|-----------|----------------|
| `rolling-update` | CrdbCluster version upgrade: state machine in betaclusterctrl, per-node drain and restart, readiness gating |
| `node-restart` | Single CrdbNode safe restart: nodectrl drain, delete, and recreate flow |
| `scale-up` | Adding nodes: pool assignment, CrdbNode creation, join flag handling |
| `scale-down` | Removing nodes: decommission state machine, PVC retention |
| `tenant-creation` | CrdbTenant provisioning: network policy, SQL proxy, routing |
| `cert-reload` | Certificate rotation: sidecar-based SIGHUP, shareProcessNamespace requirement |
| `in-place-resize` | CPU/memory change without pod eviction: hash exclusion, drift detection, patch path, resize status states |
| `webhook-cleanup` | Webhook config deletion on operator shutdown: cleanupWebhookConfigs flow, best-effort guarantees |
| `pvc-ownerref` | PVC owner reference assignment and retention flow: SetControllerReference, finalizer-based cleanup |

## Key entry points

| File | Role |
|------|------|
| `controllers/betaclusterctrl/reconciler.go` | CrdbCluster reconciliation loop |
| `controllers/nodectrl/` | CrdbNode state machine (pod lifecycle) |
| `controllers/nodectrl/pod.go` | Pod construction and drift detection including `podResourcesNeedPatch` |
| `controllers/tenantctrl/` | CrdbTenant reconciliation |
| `manifests/crdb_node.go` | Node pod construction and TLS/insecure branching |
| `manifests/crdb_node_flags.go` | CRDB startup flag assembly |
| `kube/ctl.go` | All K8s API calls (Apply, Patch, Delete, etc.) |
| `kubecrdb/` | CRDB-aware helpers (SQL exec, decommission, drain) |
| `manager/cleanup.go` | Webhook config cleanup on shutdown |

## Technical context for specific operations

**cert-reload**: The cert-reloader sidecar sends SIGHUP to the CRDB process to trigger a
certificate reload. This requires `shareProcessNamespace: true` on the pod so the sidecar
can signal the CRDB container's process. Insecure clusters (TLSEnabled=false) do not get the
cert-reloader sidecar at all, so `shareProcessNamespace` is false for them.

**in-place-resize**: `ResourceRequirements` and `ConditionalResourceRequirements` are excluded
from the pod hash in `GetNodeHashRevisionV2` so that CPU/memory changes do not trigger pod
eviction. Drift is detected by `podResourcesNeedPatch` in `nodectrl/pod.go`, which compares
only Limits and Requests (not Claims, to avoid a nil-to-empty-slice false positive). The patch
goes through `kube.Ctl.PatchPodResources` as a strategic-merge patch on the CRDB container.
The container name is passed as a parameter rather than imported from manifests to avoid a
circular import. The `pod.Status.Resize` field is monitored: Proposed, Deferred, and
InProgress states are logged as info; Infeasible emits a Warning event on the CrdbNode.

**webhook-cleanup**: `registerManifests()` in `manager/manager.go` applies both webhook
configs at every operator startup via `ctl.Apply`, so Helm does not own them and does not
delete them on uninstall. On shutdown, `cleanupWebhookConfigs()` in `manager/cleanup.go` runs
after `mgr.Start(ctx)` returns, using a fresh 30-second context because the original context
is already cancelled at that point. This is best-effort only and does not fire on SIGKILL,
OOMKill, node failure, or panic.

**pvc-ownerref**: Data, logs, and WAL PVCs get a controller ownerReference pointing to their
CrdbNode via `ctrl.SetControllerReference`. The ownerRef is for topology visibility, not
garbage collection — PVC cleanup is explicit via finalizer. When a node is scaled down with
retention enabled, `addRetentionMetadata()` clears `pvc.OwnerReferences` in the same update
loop to prevent GC from deleting the retained PVC.

**TLS vs insecure split**: TLSEnabled is the single source of truth throughout the codebase.
When false the operator uses the `--insecure` flag instead of `--certs-dir`, mounts no cert
volumes, sets no `COCKROACH_CERTS_DIR` env var, uses HTTP for the readiness probe, and skips
`ensureClusterSettings` (which requires an authenticated SQL connection).

## Workflow

1. Accept the operation name as an argument, or prompt the user to choose from the list above.

2. Capture the current git SHA for pinned source links:
   `git rev-parse HEAD`
   Use SHA-pinned GitHub URLs for all code references:
   `https://github.com/cockroachlabs/cockroach-enterprise-operator/blob/{SHA}/{file}#L{line}`

3. Identify the entry point for the requested operation using the tables above.

4. Read the relevant files. Trace the call graph step by step: what triggers the reconciler,
   what state it reads (CR status, pod conditions), what decision branches exist, what K8s API
   calls are made, what CRDB-specific operations happen, what gets written back to status, and
   when it requeues and with what delay.

5. Output the trace inline in chat, structured as phases with prose descriptions and code
   links. Include each state machine state and its transition condition, error paths and requeue
   behavior, any CRDB SQL operations (what query and why), and any non-obvious interactions like
   the shareProcessNamespace requirement for cert-reload.

6. End with a summary flow diagram if there are more than three phases.

## Output style

Write prose descriptions with inline code references (file:line). Use SHA-pinned GitHub links
for all referenced locations. Only trace code that actually exists. If a path is unclear, say
so and offer to dig deeper rather than guessing. Output is inline in chat unless the user
explicitly asks for a file.
