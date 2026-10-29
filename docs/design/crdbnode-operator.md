# Design: CrdbCluster and CrdbNode Without StatefulSets

- Status: implemented
- Start date: 2022-02-14
- Authors: Chris Seto, Ye Ji, David Muto

This is the original design for the CrdbCluster and CrdbNode controllers
(`controllers/betaclusterctrl` and `controllers/nodectrl`). It explains why the
operator does not use StatefulSets and how the two controllers split the work.
The code has moved on since this was written, so treat the code as the source
of truth for details.

## Summary

This operator does not use Kubernetes StatefulSets. It has its own Pod
replication controller, heavily inspired by the StatefulSet controller. It also
does not try to cover every part of running CockroachDB in Kubernetes. It
focuses on running CockroachDB nodes as Pods that a user-specified [label
selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/)
can target. That lets it fit into existing setups, whether the environment is
fully managed or only the Kubernetes API is available.

## Motivation

Running CockroachDB in Kubernetes is not trivial. As a distributed stateful
system, CockroachDB has needs that are hard to express in Kubernetes. Rolling
restarts have to be paced to avoid service interruptions, a cluster can span
several Kubernetes clusters, and a node doesn't have a simple ready or not ready
state. The helpers written to deal with this tend to be duplicated across
projects and need knowledge of both CockroachDB and Kubernetes to use well. A
dedicated operator reduces that knowledge, removes the duplication, and gives a
more natural way to express CockroachDB in Kubernetes.

Earlier ways of running CockroachDB on Kubernetes each built features for their
own users, so their feature sets don't line up. SQL Services are a good
example: each approach creates one, but they all differ. One aims for the most
correct TCP load balancer per cloud, the Helm chart allows unlimited
customization, and the public operator supports just enough customization for
what users asked for. Matching every Service configuration a user may want isn't
feasible when Kubernetes already provides that through Service definitions. So
this operator leaves things like Service definitions to the user, which keeps it
small and easier to reuse.

StatefulSets are the most common way to deploy CockroachDB on Kubernetes without
an operator. They are a fast way to get started, but they have several problems
for CockroachDB.

### Ordinal based ordering

StatefulSets use ordinals for [Pod
identity](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#pod-identity),
which imposes an order on the Pods. If Pod 2 of 5 needs a change, the Pods
after it have to be updated first. Say Pebble hits a bug on Pod 2 and it needs
manual repair. The first step is to take Pod 2 offline, which [isn't possible
without first taking every Pod above 2
offline](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#deployment-and-scaling-guarantees).
The workaround is a script in the Pod command that stops Pod 2 from starting,
but that change has to roll through every Pod above 2 first. Restarting healthy
nodes while one is already down can make the cluster less stable. The same
applies if Pod 2 has to be decommissioned and replaced.

### Ordinal reuse

CockroachDB scales horizontally, so `.Replicas` changes often in both
directions. Scaling down and back up causes ordinal reuse: Pod X goes away and
then comes back. CockroachDB [doesn't allow nodes that fully left the cluster to
rejoin](https://github.com/cockroachdb/cockroach/commit/57033b4bccd693e7b30a3359b256e0aa6375a14b).
The workaround is to delete the PVC Pod X would reuse. That is risky because
there is no way to delete a PVC only if it isn't in use. If a scale up happens
during the deletion, the PVC goes into a pending deletion state and is deleted
the next time it is unmounted, for example when the Pod crashes or restarts. The
only known fix is to edit state in etcd by hand.

### No PDB awareness in rolling restarts

StatefulSets [don't use the eviction API during a rolling
restart](https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/statefulset/stateful_set_control.go),
so a restart can violate a [Pod Disruption
Budget](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/#pod-disruption-budgets)
(PDB). Sometimes that is wanted, but in general a rolling restart shouldn't risk
a service interruption unless asked to.

### Required homogeneity

StatefulSets force all Pods to look the same. That is usually good, but it makes
the most secure setup hard. Ideally each node has its own TLS certificate for
its own hostname. With a StatefulSet the only way to get that is to give the
CockroachDB Pods some Kubernetes access so they can fetch or mint their own
certificate, which widens the blast radius if a Pod is compromised.

### Immutable fields

[The persistent volume spec in a StatefulSet can't be changed without deleting
and recreating the
StatefulSet](https://github.com/cockroachdb/cockroach/issues/55768). That can
be worked around, but it fails badly. A wrong delete request deletes every
CockroachDB Pod, and a wrong update strategy restarts all Pods at once.

None of these alone is a reason to drop StatefulSets. Together they are.

## Requirements

- Must support multi-region CockroachDB deployments.
- Should make manual operations on a cluster simpler.
- Must support managed and self-hosted environments without putting unnecessary
  limits on the environment.
- Must support migrating existing long-running clusters with zero downtime.

## Detailed design

### Custom resources

The operator adds two custom resources, CrdbCluster and CrdbNode. This document
covers how the controllers behave, not every field they expose.

The CrdbCluster acts as a replication controller for CrdbNodes. It has three
modes:

- In Disabled mode the controller only updates status. It doesn't reconcile
  CrdbNodes.
- In CreateOnly mode it creates CrdbNodes to reach the desired scale but doesn't
  reconcile existing ones.
- In MutableOnly mode it reconciles every mutable field of the CrdbNodes it
  owns. That excludes the StorageClass and shrinking PVCs, because a PVC's
  StorageClass can't change and its size can't decrease.

A Full mode that replaces nodes to change immutable fields may come later.

The CrdbNode controller manages one CockroachDB Pod and its related resources,
such as PVCs. Tracking each node as its own custom resource solves [Pod
identity](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#pod-identity)
without ordinals. A CrdbNode exposes a low-level API, such as the image, PVC and
even the Pod command. Together with the cluster modes this makes targeted
changes to one node easy and safe, and it is what makes migrations from
StatefulSets possible.

```
          ┌─────────────────────────────┐
          │         CrdbCluster         │
          └─────────────────────────────┘
                         │
        ┌────────────────┼────────────────┐
        ▼                ▼                ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│   CrdbNode   │ │   CrdbNode   │ │   CrdbNode   │
└──────────────┘ └──────────────┘ └──────────────┘
        │                │                │
   ┌────┴──┐        ┌────┴──┐        ┌────┴──┐
   ▼       ▼        ▼       ▼        ▼       ▼
┌─────┐┌───────┐ ┌─────┐┌───────┐ ┌─────┐┌───────┐
│ PVC ││  Pod  │ │ PVC ││  Pod  │ │ PVC ││  Pod  │
└─────┘└───────┘ └─────┘└───────┘ └─────┘└───────┘
```

### Rolling updates

A rolling update starts when the CrdbCluster spec changes, for example new
resource limits, a new CockroachDB version, or just a changed annotation, which
amounts to a rolling restart. The CrdbCluster controller then restarts nodes in
a way that doesn't interrupt service. Nodes are
[evicted](https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/)
one at a time with a delay in between.

Like most Kubernetes replication controllers, the operator uses a fingerprint
(a hash of the spec) to detect drift. The CrdbCluster controller looks for
outdated CrdbNodes and the CrdbNode controller looks for outdated Pods.

1. A user changes the CrdbCluster spec, which changes its fingerprint.
2. The operator records the new fingerprint as current and keeps the old one as
   previous.
3. While current and previous differ, the operator updates outdated CrdbNodes.
   It handles nodes whose fingerprint matches neither current nor previous
   first.
4. When every CrdbNode has the current fingerprint, the operator sets previous
   to current, which ends the rollout.

Step 3 only runs in MutableOnly mode.

A rolling update can be aborted by reverting the CrdbCluster spec, or changed
by editing the spec again. Because nodes with an unknown fingerprint go first, a
bad update is easy to correct.

### Migrating from a StatefulSet

Migrations use the Disabled mode and the low-level CrdbNode API. For each
StatefulSet being taken over, the operator creates a CrdbCluster in Disabled
mode with the same scale. Then, one Pod at a time, it lowers the StatefulSet's
scale and creates a CrdbNode that uses the PVC the removed Pod left behind. When
every Pod has moved, the CrdbCluster stays in Disabled mode until the user
deletes the StatefulSet. Only then does it switch to MutableOnly, after which the
operator applies any Pod changes it needs. As long as existing Services select both the
StatefulSet Pods and the CrdbNode Pods, clients only see a rolling restart.

`controllers/migrationctrl` implements this for Helm StatefulSets and for
clusters managed by the public operator.

```
 StatefulSet scale 3          CrdbCluster Disabled, scale 3
   Pod 0  Pod 1  Pod 2
   PVC 0  PVC 1  PVC 2

 StatefulSet scale 2          CrdbCluster Disabled, scale 3
   Pod 0  Pod 1
   PVC 0  PVC 1  PVC 2

 StatefulSet scale 2          CrdbCluster Disabled, scale 3
   Pod 0  Pod 1               CrdbNode
   PVC 0  PVC 1  PVC 2 ◀──────┘
```

### Failure scenarios

The operator runs in a highly available setup with leader election. If it is
unavailable, administrative actions wait until it is back, as with any operator.
The cluster keeps serving SQL and HTTP traffic because it only depends on the
operator for bootstrapping. A Pod that crashes during an operator outage is
restarted by Kubernetes. If a Pod is evicted for using too many resources while
every operator replica is down, the operator has to be recovered before that
node comes back. With the operator running highly available, that is unlikely in
a well-provisioned cluster.

### Temporary differences between nodes (canarying)

Some users want to test a new CockroachDB version on a few nodes first, or run a
different container on one node for a while. The CrdbCluster modes support this.
Set the CrdbCluster to Disabled or CreateOnly to allow manual changes to
CrdbNodes, then roll the new version out at whatever pace and to whichever nodes
you want. If it works, update the CrdbCluster and set it back to MutableOnly to
finish the rollout. If it doesn't, set it back to MutableOnly and the operator
rolls the change back.

## Drawbacks

This looks like a lot of work that duplicates the StatefulSet controller. In
practice that controller is fairly thin and relies on other Kubernetes features,
so the duplication is small and the extra control is worth it.

With a custom replication controller, the operator's own reliability matters
more. As noted above, there is a new failure mode when the operator is down and
a CockroachDB Pod is evicted. With [Guaranteed quality of
service](https://kubernetes.io/docs/tasks/configure-pod-container/quality-service-pod/)
and a highly available operator, losing both at once should only happen in a
misconfigured or badly underprovisioned cluster.

## Alternatives considered

Porting existing StatefulSet-based tooling into an operator wouldn't remove any
of the pain points above. The existing public operator shows this. That tooling
also relies on long-running jobs, which don't fit the short reconcile loops of
an operator.

Moving StatefulSet management into the operator first and building a custom
replication controller later has no clear benefit. Migrations don't get easier,
because the migration path can check which kind of deployment exists and act on
it.

Improving StatefulSets upstream would take a long time and would need a design
that suits every distributed system. Many Kubernetes users also run versions well
behind the latest, so an upstream fix wouldn't reach them for a long time.

## Open questions

- Would an improved StatefulSet upstream ever be worth migrating back to?
- How should the operator handle split brain? The leader election it uses [can
  have short periods of split brain if clocks
  skew](https://github.com/kubernetes/client-go/blob/30b06a83d67458700a5378239df6b96948cb9160/tools/leaderelection/leaderelection.go#L21-L24).
  The window is short, and
  [ResourceVersions](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)
  and PDBs give some protection, so this is acknowledged but not addressed
  directly.
