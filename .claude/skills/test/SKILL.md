# Skill: test

Run the right tests for what changed. Recommends a test mode, runs the command, and handles
common follow-up actions (re-record, golden update, cluster checks).

## Test Mode Table

| Mode | Command | When to use |
|------|---------|-------------|
| Build + fmt | `make build` | Compilation check after any change (requires Docker) |
| Playback | `go test ./... -ldflags "-s -w"` | After any change; no cluster needed |
| Targeted playback | `go test -run TestName -v` | Focus on one test |
| Record specific | `go test -run TestName -record -v` | After K8s interaction changes |
| Record all | `make test/record` | After disruptive fixture changes |
| Race | `make test/race` | Concurrency-heavy changes |
| Integration | `make test/integration` | Before shipping controller changes |
| Golden update | `go test ./manifests/... -update-golden` | After pod/flag generation changes |
| Push local image | `make push/local` | Before integration tests needing a new image |
| Data-driven rewrite | `go test ./path/... -run TestName -rewrite` | After changing logic tested by `datadriven` files |

## Workflow

1. Ask what changed if not already clear. The answer drives mode selection:
   - **Controller or reconciler logic** → Playback, then optionally Race
   - **Multi-step reconciliation behavior (rolling updates, evictions, scaling, decommissioning, version validation)** → Data-driven rewrite + review diff
   - **K8s interactions (new API calls, changed request shapes)** → Record specific test
   - **Manifests / pod spec / flags** → Golden update + Playback
   - **API types** → `make generate`, then Playback
   - **Concurrency paths** → Race
   - **Cluster-facing (new operator behavior, image changes)** → Integration

2. Recommend a mode from the table. Explain briefly why.

3. For integration tests: verify k3d cluster is running first:
   ```bash
   k3d cluster list
   ```
   If no cluster: `make local-cluster` before proceeding.

4. Run the command with user confirmation.

5. Handle common outcomes:
   - **Playback "needs re-record"**: offer to re-record the specific failing test:
     `go test -run <FailingTest> -record -v`
   - **Golden file mismatch**: offer `-update-golden`:
     `go test ./manifests/... -update-golden`
   - **Data-driven output mismatch**: offer `-rewrite`, then review the diff carefully —
     rewrite blindly accepts whatever the code produces
   - **Integration test failure**: check logs; if image is stale, offer `make push/local`

## Flags Reference

| Flag | Effect |
|------|--------|
| `-record` | Records kuberecord fixtures against live cluster |
| `-update-golden` | Overwrites golden files with actual output |
| `-debug-env` | Keeps test namespace alive for `kubectl` inspection |
| `-race` | Enables Go race detector (via `make test/race`) |
| `-v` | Verbose output — always use with `-record` to see progress |
| `-ldflags "-s -w"` | Strips debug info; makes playback faster |
