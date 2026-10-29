# Versioning and Releases

Operational reference for releasing the CockroachDB Enterprise Operator.

## Quick Reference

- Version source of truth: `VERSION` file at repo root (bare semver, e.g. `1.0.0`)
- Release trigger: push a `v*` tag (e.g. `v1.0.0` or `v1.0.0-rc.1`)
- All 3 images (operator, init-container, cert-reloader) share the same version tag
- CHANGELOG.md must be updated before every release (move Unreleased items under the new version header)
- Dev images: use `make push/dev` or `make push/local` as before (SHA-tagged, no version tag)
- RC/GA images: push a `v*` tag to trigger the release workflow

## How to Release

### 1. Bump the version

```bash
# Edit VERSION to the new version
echo "1.1.0" > VERSION
# Update CHANGELOG.md (move Unreleased items under the new version header)
# Commit and merge to main
```

### 2. Validate with an RC (recommended for first release or major changes)

```bash
git tag v1.1.0-rc.1
git push origin v1.1.0-rc.1
# VERSION file must match base version (1.1.0 for v1.1.0-rc.1)
```

Verify images appeared:
```bash
gcloud artifacts docker images list \
  us-docker.pkg.dev/releases-prod/self-hosted/cockroachdb-operator \
  --filter="tags:v1.1.0-rc.1"
```

If RC fails, fix, merge, tag `v1.1.0-rc.2` on the new commit.

Prerelease tag format: `v<VERSION>-<label>.<N>` where `<label>` is any
identifier like `rc`, `beta`, or `alpha`. The VERSION file must match the
base version (e.g. VERSION contains `1.1.0` for tag `v1.1.0-rc.1`).

Examples:
```
v1.1.0-rc.1      # release candidate
v1.1.0-rc.2      # second RC after fixes
v1.1.0-beta.1    # beta (for early testing)
v1.1.0-alpha.1   # alpha (for internal validation)
```

Prerelease images are tagged with the full version (e.g. `v1.1.0-rc.1`),
not the base version. This keeps prerelease and GA images distinct in
registries.

### 3. Push the GA tag

```bash
git tag v1.1.0
git push origin v1.1.0
# VERSION file must exactly match tag version (1.1.0)
```

### 4. Verify

```bash
# GAR
gcloud artifacts docker images list \
  us-docker.pkg.dev/releases-prod/self-hosted/cockroachdb-operator \
  --filter="tags:v1.1.0"
# DockerHub (if credentials are configured)
docker pull cockroachdb/cockroachdb-operator-v2:v1.1.0
```

### 5. Update helm-charts

Create a helm-charts PR: operator chart version → `1.1.0`, image tag → `v1.1.0`.

## Patch Releases

Patch releases (e.g. `1.0.0` → `1.0.1`) follow the same workflow as any
other release. Bump VERSION, update CHANGELOG, merge, tag.

```bash
echo "1.0.1" > VERSION
# Update CHANGELOG.md
# Commit, merge to main
git tag v1.0.1
git push origin v1.0.1
```

## Upgrade Philosophy

The operator is always forward-moving. Bug fixes and improvements land in
the latest version on `main`. Customers upgrade forward to pick up
fixes. All minor bumps within the same major are backward compatible, and
new features default to disabled, so it is safe to skip minor versions
(e.g. upgrading directly from 1.1.x to 1.4.x).

There are no maintenance branches. `main` is the only long-lived branch.

The operator handles CockroachDB-version-specific differences internally
using version-gated logic. A single operator version works correctly across
its entire supported CRDB range. If a new CRDB version introduces a feature
the operator does not yet support, that feature is not exposed via the CRD
and should be documented as unsupported in the chart's compatibility notes.

### Hotfix on an older version (escape hatch)

If a critical or security fix absolutely cannot wait for a normal forward
release, you can checkout from an older tag and release from there. This
should be rare.

```bash
git checkout -b hotfix-1.0.1 v1.0.0
# Apply fix, bump VERSION to 1.0.1, update CHANGELOG
git tag v1.0.1-rc.1
git push origin hotfix-1.0.1 v1.0.1-rc.1
# After validation
git tag v1.0.1
git push origin v1.0.1
```

The release workflow triggers on any `v*` tag regardless of branch. The
branch is ephemeral and does not need to be maintained. Cherry-pick the fix
back to `main` afterward.

```
main:         ──●──●──●──●──●── (v1.0.0) ──●──●── (v1.1.0) ──●──
                                  │
hotfix-1.0.1:                     └──●── (v1.0.1)
```

## How Version Flows Through the Build

```
VERSION file
  ├── build/stamp.sh → STABLE_OPERATOR_VERSION (Bazel stamping)
  │     ├── buildinfo/BUILD.bazel x_defs → buildinfo.OperatorVersion()
  │     ├── pkg/constants/BUILD.bazel x_defs (UBI only) → sidecar image refs
  │     ├── BUILD.bazel stamped_tags → push_production / push_dockerhub
  │     └── build/redhat_labels.bzl → Red Hat container labels
  └── Makefile LDFLAGS (local go build)
        ├── buildinfo.operatorVersion
        └── pkg/constants.operatorVersion
```

`buildinfo` and `pkg/constants` each have their own `operatorVersion` variable
because `pkg/constants` cannot import `buildinfo` (import layering). Both are
stamped from the same VERSION file.

## Local Testing

### Verify version stamping

```bash
./build/stamp.sh | grep OPERATOR_VERSION
# Expected: STABLE_OPERATOR_VERSION 1.0.0

make build
./cockroach-enterprise-operator --version
# Should show Operator Version: 1.0.0
```

### Verify Bazel targets resolve (no auth needed)

```bash
bazel build --config=ubi :push_production :push_dockerhub
bazel build --config=ubi //services/init-container:push_production
bazel build --config=ubi //services/inotifywait:push_production
```

### Verify local image push and labels

```bash
make push/local
docker pull localhost:5000/cockroach-operator:latest
docker inspect localhost:5000/cockroach-operator:latest | jq '.[0].Config.Labels'
# "version" and "release" will show template strings (e.g. {{STABLE_OPERATOR_VERSION}})
# in local builds because make push/local does not pass --stamp to Bazel.
# Production builds use --stamp, which substitutes real values.
```

To verify stamped labels locally, use Bazel directly:

```bash
bazel run --config=ubi :push_local --stamp
docker pull localhost:5000/cockroach-operator:latest
docker inspect localhost:5000/cockroach-operator:latest | jq '.[0].Config.Labels.version'
# Expected: "1.0.0"
```

### Run tests

```bash
go test ./buildinfo/... -v
go test ./... -ldflags "-s -w"
```