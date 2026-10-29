# Skill: codex-review

Run an independent AI review of staged or changed code using `roachdev codex`. This is a
second-opinion gate that runs after `/review` has already passed. Because codex uses a
different model with different training, it surfaces blind spots that Claude may have
normalised away.

## When to use

Use this for high-stakes changes where an independent check is worth the extra time:

- Controller or reconciler logic with concurrent state (decommission, rolling upgrade, scale)
- Security-sensitive changes (RBAC, Cilium network policy, webhook validation, cert handling)
- Large diffs touching multiple reconcilers or packages
- Any change flagged as "complex" by `/review` that you want confirmed before merging

Do NOT use for trivial fixes, logging changes, comment updates, or golden file refreshes —
the overhead is not worth it for low-risk changes.

## How to run

### Step 1 — Export the diff

```bash
git diff --staged > /tmp/codex-review.diff
# Or for a branch review:
git diff main...HEAD > /tmp/codex-review.diff
```

### Step 2 — Launch codex with the review prompt

```bash
roachdev codex -- "Review the Go code changes in /tmp/codex-review.diff. This is a Kubernetes operator for CockroachDB (a distributed SQL database). Focus on:
1. Race conditions in concurrent reconciler state — look for shared state read/written across goroutines without synchronisation, and retrieve→check→act sequences where the object could change between retrieval and the action.
2. Dead or unreachable code branches — branches that return the same value as fallthrough, or variables computed but never used.
3. Missing recovery path if the operator is killed mid-operation — stateful operations (SQL commands, K8s patches) that have no way to detect they already ran on restart.
4. Security blast radius — RBAC or network policy rules that are broader than necessary (missing port constraints, wildcard verbs, overly broad label selectors).
5. API backwards compatibility — renamed, removed, or type-changed fields in api/v1beta1/ that would break existing customers on upgrade.
6. Test coverage — new conditional branches with no test that would fail if the branch were removed.
7. Cross-cycle status bleed — for any new gate that reads cluster.Status (HasAction, a condition, or a status field), check whether status written by a PREVIOUS reconcile cycle could satisfy the gate and silently bypass it. Specifically: (a) if a status-mutating call (like updating Status.Image or Status.Version) runs in the same reconciler path as a failed gate check, verify it cannot advance the field the gate uses as its trigger, and (b) if an action like Upgrade:Finished persists across upgrade cycles, verify that a new gate introduced in the diff correctly resets or ignores the stale value. Ask: if the gate condition were set by an older reconcile cycle and the operator restarted, would the gate still behave correctly?
8. UpdateStatus ordering — if a reconciler sets status fields in memory and then calls a full object Update (ctl.Update / ctl.UpdateWithOptions) before ctl.UpdateStatus, the status will be silently discarded. K8s treats status as a separate subresource: a regular Update ignores the status field in the request and returns the server's current status in the response, which controller-runtime stores back into the in-memory object, resetting any in-memory mutations. If UpdateStatus is then called with that reset object, it writes back the pre-mutation state. Fix: always call UpdateStatus before any full object Update that follows an in-memory status mutation.
State each finding with: file and line, severity (blocking / important / nit), what the problem is, and what to fix. If the diff is clean, say so."
```

### Step 3 — Review codex output

Codex will run autonomously. Read through its findings and compare them against what `/review`
already flagged:

- **New Tier 1 finding from codex**: fix it before committing.
- **Finding already caught by `/review`**: note the agreement and confirm your planned fix.
- **Codex finding that seems wrong**: verify it against the code; codex can misread context.
  Do not dismiss without checking.
- **Codex raises a question rather than a finding**: treat it as a Tier 2 — worth
  understanding even if it turns out to be intentional design.

### Step 4 — Clean up

```bash
rm -f /tmp/codex-review.diff
```

## What codex will not catch

Codex does not know this codebase's specific conventions. It will not flag:

- Wrong reconciler return helper (bare `ctrl.Result{}` instead of `controllers.NoRequeue()`)
- `RequeueAfter` with a bare integer (nanoseconds bug)
- `ctl.UpdateStatus` missing before `NoRequeue()`
- Import layering violations
- Comment style (block vs inline)

These are covered by the local `/review` skill and the rules in `.claude/rules/`. Run
`/review` first, then `/codex-review` for the independent pass.

## Workflow summary

```
/review        ← operator-specific checks, catches style/pattern/correctness issues
    ↓ pass
/codex-review  ← independent AI pass, focuses on concurrency/security/recovery/compat
    ↓ pass
/commit        ← write message and commit
```
