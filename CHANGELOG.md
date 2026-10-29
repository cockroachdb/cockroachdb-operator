# Changelog

All notable changes to the CockroachDB Enterprise Operator will be documented
in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] - Unreleased

### Added
- Basalt: mTLS between CRDB and Basalt for encrypted control-plane traffic ([CC-36677] #303).
- Basalt: encryption-at-rest for Basalt stores ([CC-35992] #296, #302).
- Basalt: reclaim store directories on node decommission and cluster delete ([CC-36676] [CC-36679] #286).
- Basalt: optional PVC backing for the local scratch directory ([CC-36960] #300).
- Init-container: customizable locality mapping (#307).
- Operator: `zone-id` flag support (#308).
- Autoremediation: filter CRDB pods when node CPU usage is too high ([CC-36473] #298).
- Tenant SQL pods: enable CDC billing accounting ([CC-36793] #314).

### Changed
- All three UBI-based images (operator, init-container, cert-reloader) rebased from UBI9 to UBI10 to improve Red Hat Partner Connect health grades ([CC-35894] #319, #320).
- betaclusterctrl no longer owns the cluster-scoped PriorityClass created from CrdbCluster (#321).
- Manifests: propagate current CRDB fields to tenant pools ([CC-36835] #316).

### Fixed
- Operator: keep the CrdbCluster conversion webhook CA bundle synchronized when webhook certificates rotate without a pod restart ([CC-37215]).
- migrationctrl: public operator migration handoff ([CC-36528] #293).
- betaclusterctrl: use ready pod for under-replicated range check (#294).
- betaclusterctrl: skip replication check for single-node clusters (#292).
- WAL disk size changes no longer trigger pod restarts (#295, #297).
- Operator fails fast when CloudRegion is empty instead of nil-panicking (#313).
- Manifests: correct CRDB store path when Basalt secure mode is enabled (#309).
- k3d: de-flake node startup and bump k3s image to v1.32.13-k3s1 ([CC-36192] #323, #324).
- nodectrl: derive CrdbNode topology values from effective Pod constraints so
  PodTemplate configurations remain topology-aware during explicit placement
  and decommissioning ([CC-37223]).

### Security
- Bump vendored Go dependencies for VULM-708 findings (#304).

## [1.0.0] - 2026-08-03

### Added
- Semantic versioning for all operator images (operator, init-container,
  cert-reloader) with tag-driven release workflow (`v*` tag push), `--version`
  flag, and startup version logging. See
  [docs/versioning-and-releases.md](docs/versioning-and-releases.md).
- Migration controller (`--enable-migration`) to automate migration from Helm
  and public operator workloads to v1beta1 CrdbNodes with phased execution,
  rollback support, and certificate handling. When the flag is set, the
  conversion webhook and v1alpha1 serving are only enabled if the CRD's
  `status.storedVersions` contains v1alpha1. Helm-only clusters skip the
  conversion webhook entirely since all operations are v1beta1.
- Autoremediation that evicts hot CockroachDB pods off overloaded Kubernetes
  nodes to rebalance load across the cluster.
- CockroachDB pod restart classifier that records why a pod restarted, making
  restart causes visible for diagnostics and alerting.

### Fixed
- Webhook CA certificate mismatch when mounted certs are rotated. The operator
  now reads the CA from the filesystem and watches for changes, eliminating
  TLS errors without a pod restart.
- Version validation no longer permanently blocks upgrades when the kubelet
  garbage-collects the version checker job's pod before the operator reads its
  logs. The detected version is now cached on the job as an annotation, and a
  missing pod triggers a retry instead of a hard failure.
- Cloud region detected from node topology labels is no longer discarded when the
  cloud provider cannot be determined. Previously this caused a region mismatch
  after operator restart, leading to a nil-pointer panic in the scale reconciler.
- Reconciler robustness across operator restarts. betaclusterctrl no longer
  deadlocks evictions after a restart, where lost in-memory eviction state could
  stall rolling updates. nodectrl no longer evicts pods that are actively
  decommissioning just because their spec drifted, and avoids a PVC filesystem
  resize deadlock.
- Decommission and restart gating checks under-replicated ranges through
  `node status` instead of querying crdb_internal directly, which stays safe
  under virtualized clusters and avoids unsafe internal access.
- tenantctrl terminates stamped pods before destroy_tenant so tenant teardown
  does not leave orphaned pods behind.
- The conversion webhook returns a minimal read-only v1alpha1 projection for
  non-migration v1beta1 clusters (fresh installs and completed migrations)
  instead of injecting migration annotations. This prevents Helm Server-Side
  Apply field-ownership conflicts on upgrade while keeping actively migrating
  clusters on the full conversion path.

### Changed
- Release builds tag container images with the semantic version instead of
  commit SHA.
- Update the `cockroachlabs/version` dependency.

### Upgrade Notes
- Clusters upgrading from an earlier enterprise operator version that used
  v1alpha1 must ensure `v1alpha1` is removed from the CRD's
  `status.storedVersions` before deploying this release. Without this, the
  operator will refuse to start. To verify and clean up:
  ```
  kubectl get crd crdbclusters.crdb.cockroachlabs.com -o jsonpath='{.status.storedVersions}'
  kubectl patch crd crdbclusters.crdb.cockroachlabs.com --subresource=status \
    --type json -p '[{"op":"replace","path":"/status/storedVersions","value":["v1beta1"]}]'
  ```
