# Skill: review

Review staged or changed code for correctness, safety, and quality before committing or
opening a PR. This local skill shadows the global `/review` with operator-specific checks
derived from this repo's review history.

## Usage

```
/review
```

Run on staged changes (`git diff --staged`) or on a branch diff against `main`. Can
also be invoked on a specific file or function: `/review controllers/nodectrl/pvc.go`.

---

## How to run a review

1. Run `git diff --staged` (or `git diff main...HEAD` for a branch review).
2. Work through the checks below in tier order. Flag every finding — do not silently skip.
3. For each finding, state: the file and line, the tier, what the problem is, and what to do.
4. After all checks, summarise: how many Tier 1 / Tier 2 / nit findings, and whether the
   code is ready to commit or needs changes first.
5. Offer to fix any Tier 1 or Tier 2 findings inline before committing.

---

## Tier 1 — Must fix before committing

These are correctness, safety, and security issues. Do not proceed to commit while any of
these are open.

### Concurrent reconciler safety

- Look for any state read outside a mutex that is also written by another goroutine or
  reconciler. Flag any use of a shared map, slice, or status field without synchronisation.
- Look for "retrieve → check → act" sequences where the object could change between retrieval
  and the act. The fix is `Preconditions.ResourceVersion` on the K8s delete/update call.
  Example: a pending node becomes running between the list and the delete call.
- Look for in-memory state (e.g., a set of "currently decommissioning nodes") that is lost on
  operator restart. Ask: "if the operator crashes here, is it safe to re-run from scratch?"

### Dead or unreachable branches

- Identify any `if` or `else` branch that returns the same value as the fallthrough. Flag it
  as dead code — either it was never needed or the logic changed and the branch is stale.
  Example: a restructured condition that returns `NoRequeue()` on both the true and false paths.
- Identify any return value or variable that is computed but never used.

### Webhook validation completeness

- For every new field validated in `manager/webhook.go`, check that the inverse case is also
  covered. If `TLSEnabled=true` requires X, then `TLSEnabled=false` must explicitly forbid X
  (or the reverse). Missing inverse validation is a common source of inconsistent cluster state.
- Check that new API fields with mutual exclusivity constraints are validated together, not
  individually.

### API backwards compatibility

- If a field is renamed, removed, or its type changed in `api/v1beta1/`, flag this explicitly.
  Ask: "does any existing CrdbCluster or CrdbNode in production break on upgrade?"
- If a field's `omitempty` or default value changed, note the migration impact.

### Operator-restart recovery

- For any multi-step stateful operation (decommission, version upgrade, cert rotation), ask:
  "if the operator is killed at step N and restarted, can it safely resume?" Flag any step
  that issues a side effect (SQL command, K8s patch) without a way to detect it already
  happened. Example: decommission state held only in memory is lost on restart, causing the
  operator to re-decommission nodes on the next reconcile.

### Cross-cycle status bleed

- When a new flow adds a gate that reads from `cluster.Status` (e.g., `HasAction`, a status
  field, or a condition), check whether status written by a *previous reconcile cycle* could
  satisfy the gate and silently bypass it. Common pattern: an action like `Upgrade:Finished`
  set in cycle N persists into cycle N+1 where a guard reads it without knowing which cycle
  set it. Ask: "if this status value were set by an older reconcile cycle and the operator
  restarted, would the gate still behave correctly?"
- When a status-mutating reconciler (e.g., `detectCurrentVersion`) runs after a failed gate
  check (e.g., `ValidationFailed`), verify it cannot advance status fields (like `Status.Image`)
  that the gate uses as its trigger signal. If it can, the gate will be bypassed on the next
  reconcile. Fix: guard the status mutation with a check that the gate condition is still valid
  (e.g., `!HasAction(ValidateVersion, Failed)`).
  Example: `detectCurrentVersion` advancing `Status.Image = Spec.Image` even after
  `ValidationFailed`, causing `requiresUpgradeValidation` to return false on the next
  reconcile and allowing the rolling update to proceed unchecked.

### UpdateStatus ordering with full object Update

- If a reconciler sets status in memory and then calls `ctl.UpdateWithOptions` (a full object
  Update) before `ctl.UpdateStatus`, the status will be silently lost. K8s treats status as a
  separate subresource: a regular Update ignores the status field in the request body and
  returns the *server's current status* in the response. Controller-runtime stores that
  response back into the in-memory object, resetting any status fields that were set in
  memory. If `UpdateStatus` is then called with the reset object, it writes back the
  pre-mutation state — the status change never reaches the server.
  Fix: always call `ctl.UpdateStatus` before any `ctl.Update` / `ctl.UpdateWithOptions` call
  that follows an in-memory status mutation in the same reconcile path.

### Security and blast radius

- For new Cilium NetworkPolicy or RBAC ClusterRole changes: verify the scope is as narrow as
  possible. Missing `toPorts` on an egress rule allows all ports — always add port constraints
  alongside endpoint selectors.
- For ClusterRole verb additions: verify no `*` wildcard is introduced and that the new verb
  applies only to the intended resource group.
- For user-supplied path values (spec fields used as mount paths or flag arguments): check
  whether setting the path to an existing sensitive directory would cause harm. Note in the
  review even if it is deemed acceptable.

### OOM and resource bounds

- If a new function fetches logs, events, or lists without a limit, flag it. Use
  `io.LimitReader` or set a `LimitBytes` / `Limit` on the K8s list call. A generically-named
  helper in `kube/` will be reused by future callers who may not think about bounds.

---

## Tier 2 — Should fix, not blocking

Flag these clearly but mark them as non-blocking. Resolve before the PR is merged, not
necessarily before the first commit.

### Test coverage gaps

- For every behaviour change: is there a test that would fail if the change were reverted?
  If not, flag the gap. A test that passes with or without the code under test has no value.
- For every new conditional branch: is there a test that exercises each branch?
- For every new golden-file-affecting change: were the golden files updated?
- For gates that protect multi-cycle state machine flows (version validation, decommission,
  cert rotation): require a multi-reconcile test that exercises the full cycle — including
  what happens after the gate fails and the operator reconciles again. A single-reconcile test
  cannot catch bugs where stale status from a prior cycle bypasses the gate on the next pass.
  Example: tests covering only the first reconcile after validation failure miss the case where
  a stale `Upgrade:Finished` from a previous cycle causes the gate to be bypassed.

### Hardcoded strings vs existing constants

- Before introducing a string literal (package name, port number, label key, annotation key),
  search for an existing constant in `api/v1beta1/`, `manifests/`, or `kube/`. Use it.

### Abstraction and layer violations

- If controller code is exec'ing into a pod to retrieve information that could be stored in
  a CrdbNode status field, flag it. The controller layer should observe state through the
  CR status, not through direct pod exec.
- If an ownerReference could be used instead of a label selector or name lookup, flag it.
- Import layering: `kube` must not import `manifests`; `manifests` must not import
  `controllers`. Flag any new cross-layer import.

### Code reuse and DRY

- If the same protection pattern, validation block, or helper function appears more than once
  in the diff, flag the duplication. Check whether a helper already exists nearby that does
  the same thing.
- If a new helper function would be used in only one place and is trivially inlined, prefer
  inline code.

### Naming clarity

- Flag names that are self-contradictory, use the wrong terminology for the domain, or mix
  inconsistent vocabulary within the same file (e.g., `Upgrade` vs `Update` for the same
  concept in the same reconciler).
- Flag noun names where the surrounding functions use verb names.
- Flag variable names whose meaning is opposite to their name (e.g., `desired` that means
  "number to remove" rather than "target count").

### Cleanup co-location

- If a resource is created in one file and cleaned up in another with no cross-reference,
  flag it. Cleanup logic that is co-located with creation logic is easier to audit.

### Operator-specific patterns

- `controllers.RequeueAfter(N, nil)` where `N` is a bare integer: flag as nanoseconds bug.
  Must be `N*time.Second` or a named `time.Duration` constant.
- Any bare `ctrl.Result{...}` literal in a reconciler function or a function whose result
  propagates to a reconciler: flag it. Must use `controllers.NoRequeue()`,
  `controllers.RequeueAfter(...)`, `controllers.RequeueIfError(err)`, etc.
  Exception: `ctrl.Result{}` as the zero-value placeholder on the `done=false` path of a
  triple-return helper `(ctrl.Result, bool, error)` is acceptable because the caller discards
  it when `done=false`.
- `ctl.UpdateStatus` must be called before `controllers.NoRequeue()` if the object's status
  was mutated in memory. Flag any `NoRequeue()` that follows an in-memory status mutation
  without a preceding `UpdateStatus`. Exception: phase handlers that return `(ctrl.Result,
  error)` to the main `Reconcile` switch are exempt if the main `Reconcile` always calls
  `UpdateStatus` before returning to controller-runtime.
- Error creation and wrapping must use `github.com/cockroachdb/errors`, not `fmt.Errorf` or
  `errors.New` from the standard library.
- Log output must use `logging.Infof/Warnf/Errorf(ctx, ...)` from `pkg/logging/tracing.go`,
  not `fmt.Println`, `log.Print`, or `ctrl.Log`.
- New owner references must use `ctrl.SetControllerReference(owner, obj, scheme)`, not
  `metav1.NewControllerRef`.
- `k8s.io/utils/pointer` is deprecated. Flag any new import of it. Use `k8s.io/utils/ptr`
  with `ptr.To(value)` instead of `pointer.String(value)`.
- `if x == "a" { } else if x == "b" { }` on the same variable: flag as QF1003. Use `switch`.
- Named return variables: flag any `if err := ...; err != nil { return ..., err }` where `err`
  is a named return. The `:=` shadows the named return; the returned `err` is the local one,
  losing any prior wrapping applied to the named return.

---

## Tier 3 — Nit

Prefix these with "nit:" in the review output. They are low priority and should not block
merging, but are worth noting for quality.

### Comment style

- Block comments must be full sentences: capitalised first word, period at end.
- Inline comments must be lowercase with no terminal punctuation.
- Comments that repeat the code without adding context should be removed.
- Comments that no longer match the code they describe (stale after refactor) should be
  updated or removed.
- Comments on non-obvious sorts, requeue decisions, or state machine transitions should be
  present. Flag their absence.
- Grammar errors in comments are nits, not bugs.

### Unnecessary boilerplate

- `nil` check before appending to a slice: appending to a nil slice in Go is always safe.
  Remove the guard.
- `var s []T` followed by an immediate `if s == nil { s = []T{} }`: unnecessary.
- `break` inside a loop that can only execute once: either restructure or add a comment
  explaining why `break` is intentionally absent.

### Stale markers

- TODO comments that have been resolved by the current PR should be removed, not left behind.

### Enum and constant style

- Prefer typed constants over naked strings in tests that already import the constant package.

---

## Output format

Structure the output as:

```
## Tier 1 findings

[file:line] <what the issue is and why it matters>
Fix: <what to do>

## Tier 2 findings

[file:line] <what the issue is>
Fix: <what to do>

## Nits

nit: [file:line] <short description>

## Summary

Tier 1: N  Tier 2: N  Nits: N
Ready to commit: yes / no (fix Tier 1 first)
```

If there are no findings in a tier, omit that section. If the diff is clean, say so directly
rather than listing every check that passed.
