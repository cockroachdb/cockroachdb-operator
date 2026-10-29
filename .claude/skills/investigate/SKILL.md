# Skill: investigate

Analyze operator logs, resource specs, pod events, and CR status to explain what the
reconciliation loops are doing, surface errors and their causes, and produce a structured
root-cause report. If the investigation reveals a customer-facing incident, pass the output
to `/rca` to write the full Root Cause Analysis document.

## Usage

```
/investigate
```

Then paste or provide any combination of:
- Operator logs (stdout/stderr from the operator pod or `run-local.sh`)
- `kubectl describe crdbcluster <name>` output
- `kubectl describe crdbnode <name>` output
- `kubectl describe pod <name>` output
- `kubectl get events -n <ns> --sort-by=.lastTimestamp` output
- `kubectl get crdbnode -o yaml` or `kubectl get pvc -o yaml` output
- A description of what operation was triggered (scale 3→6, rolling update, cert rotation, etc.)

Provide as much or as little as you have. The skill works with partial data.

## What it produces

For each piece of input, the skill generates:

**Reconciliation timeline**: a chronological sequence of what happened across the control
loop iterations, inferred from log timestamps and status condition transitions. Shows each
reconciler invocation, what it observed, what action it took, and what it returned
(requeue, no-requeue, error).

**Component interaction map**: which reconcilers were active and how they triggered each other.
For example, a CrdbCluster scale-up triggers betaclusterctrl to create CrdbNodes, which
triggers nodectrl to create Pods, which in turn triggers status updates back to CrdbCluster.

**Error and warning analysis**: for each error or warning in the logs, explains what code
path produced it, what the root cause is, and whether it is expected (transient) or a real
problem.

**Spec change impact**: if a CR spec update is visible in the logs or events, explains which
field changed, how the reconciler detected it (hash comparison, condition check), and what
that triggered (rolling restart, requeue, no-op).

**Status condition timeline**: maps condition transitions on CrdbCluster and CrdbNode
resources to log events, showing when each node moved through PodRunning → PodReady, or
when a decommission moved through Draining → Drained → ZeroReplicas → Decommissioned.

**Visual flow** (when operation is known): a diagram of the operation from trigger to
completion. Example for scale-up 3→6:

```
User applies CrdbCluster (Nodes: 3→6)
  ↓
betaclusterctrl: detects node count delta
  → creates 3 new CrdbNode objects
  ↓
nodectrl (×3 in parallel): each CrdbNode enters reconciliation
  → creates PVC (data, optionally logs/WAL)
  → creates Pod with join flags pointing to existing nodes
  ↓
Pod scheduled and starts → PodRunning condition set
  ↓
CRDB init: node joins cluster, rebalances data
  ↓
Pod Ready → PodReady condition set
  ↓
nodectrl: fetches NodeID via SQL, sets Status.NodeID
  ↓
betaclusterctrl: all CrdbNodes quiescent → CrdbCluster quiescent
```

## Key code locations to reference

When explaining errors or behaviors, link to the relevant code by file and rough line:

| Symptom | Code location |
|---------|--------------|
| Pod not found, creating | `controllers/nodectrl/reconciler.go` |
| Drift detected, pod needs update | `controllers/nodectrl/pod.go:podResourcesNeedPatch` |
| Hash mismatch, evicting pod | `manifests/crdb_node.go:GetNodeHashRevisionV2` |
| Drain running | `kubecrdb/kubecrdbnode/exec.go:Drain` |
| Decommission state machine | `controllers/nodectrl/reconciler.go` decommission block |
| PVC resize waiting | `controllers/nodectrl/reconciler.go:monitorPVCResize` |
| Cert reload SIGHUP | `services/inotifywait/graceful_inotifywait.go` |
| Webhook validation error | `manager/webhook.go` |
| Node pool assignment | `controllers/betaclusterctrl/reconciler.go:GetFreeNode` |
| Cluster settings apply | `kubecrdb/kubenet/db.go` |
| TenantPool warmup | `controllers/tenantctrl/reconciler.go` |
| SQL proxy deployment | `controllers/clusterctrl/reconciler.go` |

## Specific patterns to recognize

**Redundant reconciliations**: consecutive log lines showing the same reconciler called
within seconds with the same resource version indicate a spurious requeue. Explain whether
this is expected (e.g., watching a Deployment triggers on every pod status change) or a bug
(e.g., UpdateStatus called unconditionally when nothing changed).

**Stuck resources**: a CrdbNode that has been in the same condition for longer than expected
(PodRunning but not PodReady after 2+ minutes, or Draining for 10+ minutes) — identify the
last action taken and what the reconciler is waiting for.

**Rolling restart gating**: explain why a rolling restart is paused (previous node not yet
PodReady, RollingRestartDelay not elapsed) versus why it is proceeding.

**PVC resize path**: distinguish between `Resizing` condition (volume plugin in progress)
and `FileSystemResizePending` (node-side filesystem resize pending). Explain that the
operator waits for both to clear before proceeding.

**In-place resize states**: `pod.Status.Resize` field values and what each means:
- `Proposed` — kubelet received the request, evaluating
- `InProgress` — resize is running
- `Deferred` — can't resize now, will retry
- `Infeasible` — node cannot satisfy the request; operator emits `ResizeInfeasible` event

**Insecure vs TLS split**: if logs show `--insecure` flag or missing cert volume mounts on
a cluster that was expected to be TLS-enabled, check `TLSEnabled` in the CrdbCluster spec.

**Empty nodeName error**: `betaclusterctrl: CrdbNode with empty node name` means
`GetFreeNode` returned a blank K8s node name, usually because the node label
`crdb.io/app=crdb` hasn't been applied yet by the cloud provider.

## Workflow

1. Ask the user what operation was being performed (or infer from the logs if obvious).
2. Ask for the relevant logs and resource YAML. Accept them in any order or format.
3. Parse and correlate: match log timestamps to resource condition transitions, match errors
   to code paths, identify requeue decisions from `Result` patterns in logs.
4. Produce the reconciliation timeline, component interaction map, error analysis, and visual
   flow as applicable. Use plain prose for explanation and ASCII diagrams for flows.
5. If the root cause is clear, state it directly and point to the relevant code location.
   If it is ambiguous, list the top hypotheses with evidence for each.
6. Offer to dig deeper into a specific phase, error, or component if the user wants more detail.

## Output style

Use a structured report with clearly labeled sections. Prefer ASCII flow diagrams for
operations with more than two components. State root causes directly — do not hedge
unnecessarily. When referencing code, use `file.go:FunctionName` format rather than line
numbers, since those change frequently. Keep error explanations concise: what happened,
why, and whether it is expected.
