# init-container

init-container is a collection of utilities that runs as an
[init-container](https://kubernetes.io/docs/concepts/workloads/pods/init-containers/)
for CockroachDB pods. It was originally forked from
[`locality-checker`](https://github.com/cockroachdb/k8s/tree/master/locality-checker)
to allow CockroachCloud-specific functionality to be added. These
features are being bundled into a single binary/container to minimize
the total download size required for a CockroachDB pod.

## Container Requirements

The following environment variables must be set for this container:
- `KUBERNETES_NODE`, the name of the k8s node that the pod is running
	on.

## Component Utilities

### Locality Checking

The first thing that init-container does is write the region and zone
values of the pod to a volume mounted at /etc/cockroach-locality. These
values are then read by the CockroachDB pod at startup, to fill in its
[`--locality`](https://www.cockroachlabs.com/docs/stable/training/locality-and-replication-zones.html)
flag value. This allows CockroachDB to be aware of its pods' region and
zone placements within a Kubernetes deployment.

A complete locality-flag which can be passed into `cockroach start` is
written to `/etc/cockroach-locality/locality`. For users that want more
control of the locality flag, the region a pod is running in is written
to `/etc/cockroach-locality/region`, and the zone is written to
`/etc/cockroach-locality/zone`.
