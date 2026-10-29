# Skill: run-operator

Guide the user through running the operator locally against a live Kubernetes cluster.

## Four Modes

### Mode 1 — Live cluster (most common)

```bash
./scripts/run-local.sh
```

What it does: copies the operator certs from the `cockroach-operator-certs` Secret, scales down the in-cluster operator deployment to zero
replicas, then runs the operator binary with `--local-mode --disable-leader-election=true`.

Prerequisites:
- A valid kube context pointing at a k3d cluster (`kubectl config current-context`)
- The operator installed in the cluster, so the `cockroach-operator-certs` Secret exists (the script copies it automatically)

### Mode 2 — With webhooks kept in-cluster

```bash
ENABLE_WEBHOOKS=true ./scripts/run-local.sh
```

What it does: keeps one operator pod running in-cluster to serve webhook requests. The local
process runs controllers only. Use this when testing controller changes that must pass webhook
validation — without it, mutating/validating webhooks will fail (no local TLS endpoint).

### Mode 3 — Manual (fine-grained control)

```bash
NODE_NAME="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')" \
NAMESPACE="default" \
go run . --local-mode --certs-dir=./certs --disable-leader-election=true
```

Use when you need to control flags individually, point at a non-default cert directory, or
attach a debugger (`dlv exec` / VS Code Go debugger) to the process.

### Mode 4 — Leader election testing

Run two terminals, both without `--disable-leader-election`, to simulate leader contention:

```bash
# Terminal 1
NODE_NAME=node1 NAMESPACE=default go run . --local-mode --certs-dir=./certs

# Terminal 2
NODE_NAME=node2 NAMESPACE=default go run . --local-mode --certs-dir=./certs
```

Only one process will hold the lease; the other will wait and take over on failure.

## Workflow

1. Ask the user which mode they want (or infer from context — "debug controllers" → Mode 1,
   "test webhook validation" → Mode 2, "attach debugger" → Mode 3).
2. Check prerequisites:
   - `kubectl config current-context` — confirm a valid cluster context exists
   - For Mode 3: confirm `./certs` (or the specified cert dir) exists
3. Show the exact command; ask for confirmation before running.
4. After the user is done, remind them to scale the operator back up if it was scaled down:
   ```bash
   kubectl scale deploy cockroach-operator --replicas=1 -n <operator-namespace>
   ```

## Common Issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| Script exits immediately | No valid kube context | `k3d kubeconfig merge <cluster> --kubeconfig-merge-default` |
| Cert download fails | `cockroach-operator-certs` Secret missing or unreadable | Check the operator is installed, then run `kubectl get secret -n <ns> cockroach-operator-certs` |
| `cannot find module` | Wrong working directory | Run from repo root |
| Webhook calls fail (Mode 1) | Webhook service points in-cluster | Switch to Mode 2 |
| Two leaders both active | Leader election disabled on both | Expected — one will win once election is enabled |
