# Testing Patterns (Operator-Specific)

These rules govern how tests are written and run in this codebase. Follow them when adding,
modifying, or debugging tests.

## Test Modes

| Mode | When to use | Command |
|------|-------------|---------|
| Playback (default) | After any change; no cluster needed | `go test ./... -ldflags "-s -w"` |
| Targeted playback | Focus on one test | `go test -run TestName -v` |
| Record | After touching K8s interactions | `go test -run TestName -record -v` |
| Record all | After disruptive fixture changes | `make test/record` |
| Golden update | After pod/flag generation changes | `go test ./manifests/... -update-golden` |
| Race | Concurrency-heavy changes | `make test/race` |
| Integration | Before shipping controller changes | `make test/integration` |

## Playback (kuberecord)

- Default mode; requires no live cluster. Fixtures are stored in `testenv/` directories.
- `testenv.Env` activates kuberecord automatically. Do not bypass or re-implement it.
- If playback fails with a "needs re-record" error, it means the fixture has drifted. Re-record
  the specific test immediately with `-record` and commit the updated fixture alongside the code.
- Turn on `KUBERECORD_LOG_LEVEL=debug` when diagnosing playback mismatches.

## Recording

- Only run with `-record` against a live k3d cluster (`k3d cluster list` to verify one is running).
- Record the narrowest scope possible — prefer `-run TestSpecificTest -record` over re-recording
  everything.
- Never nest `t.Run` inside a record/playback test without ensuring fixture isolation (each subtest
  gets its own namespace or fixture prefix).

## Golden Files

- Golden files in `manifests/` are the authoritative assertions for pod and flag generation.
  There are no separate `require.Contains` checks for the same output — the golden file is the test.
- Flag: `-update-golden` (not `-update` or any other variant).
- After touching any of the following, run `go test ./manifests/... -update-golden` and commit
  the updated golden files:
  - Pod spec construction (`manifests/crdb_node.go`, `manifests/crdb_pod.go`)
  - Flag generation (`manifests/crdb_node_flags.go`)
  - Hash exclusion logic (`GetNodeHashRevisionV2`)

## Data-Driven Tests

- Some controller packages use the `cockroachdb/datadriven` framework for multi-step
  reconciliation scenarios. Test data lives in `testdata/datadriven/*.datadriven` files
  within the package.
- Each file is a sequence of commands (`create-cluster`, `reconcile-cluster`, `set-node-state`,
  etc.) followed by `----` and the expected state dump.
- If expected output drifts after a code change, rewrite with `-rewrite`:
  `go test ./path/to/package/ -run TestName -rewrite`
  then review the diff before committing — rewrite blindly accepts current output.
- To add a new scenario, create a `.datadriven` file with commands and placeholder `----`
  sections, run with `-rewrite` to fill in expected output, then verify the output matches
  the intended behavior.
- Look for the `datadriven.RunTest` call in the package's test files to find available
  commands and their arguments.

## Integration Tests

- Require a live k3d cluster and a freshly pushed operator image.
- Run `k3d cluster list` before invoking to confirm a cluster exists.
- If the test needs a new operator image, run `make push/local` first.
- Use `-debug-env` to keep the test namespace alive after the test finishes so you can inspect
  resources with `kubectl`.

## Concurrency

- Any change to reconciler scheduling, requeue logic, or shared state requires `-race` before
  submitting. Use `make test/race`.
- Do not add `time.Sleep` to tests to work around races — fix the underlying synchronization.

## Test Namespace Isolation

- Tests create isolated namespaces; they share the same k3d cluster.
- Keep the cluster running between test runs for faster feedback.
- Do not depend on cluster-level state from previous tests unless the fixture explicitly sets it up.
