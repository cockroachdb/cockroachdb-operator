# Go Conventions (Operator-Specific)

These rules apply to all Go code in this repository. They supplement standard Go style with
conventions specific to this codebase.

## Package import layering

The codebase has a strict import hierarchy. Violating it causes circular imports.

```
kube  ←  manifests  ←  controllers
```

`kube/` must not import `manifests/`. Pass container names and other manifest constants as
function parameters instead of importing them directly. `manifests/` may import `kube/` but
not `controllers/`. No `doc.go` enforces this yet, so verify manually before any cross-package
reference.

## Control flow

Prefer early returns to minimize nesting. Handle errors at the earliest opportunity. Avoid
deeply nested if/else chains by inverting conditions to return or continue early. Use `defer`
for cleanup: releasing locks, cancelling contexts, closing resources.

## Struct literals

Always supply field names in struct literals, never positional initialization. Prefer `&T{}`
over `new(T)` for pointer construction.

## Slices

Declare uninitialized slices as `var s []T`, not `s := []T{}`. Test emptiness with
`len(s) == 0` rather than comparing to nil, since nil and empty slices behave the same in
most contexts and `len` is unambiguous.

## Logging

Use `logging.Infof`, `logging.Warnf`, `logging.Errorf` from `pkg/logging/tracing.go` for
all structured log output. These functions take a `context.Context` as the first argument,
which carries the logger, trace ID, and span ID for distributed tracing correlation. Do not
use `fmt.Println`, `log.Print`, or `ctrl.Log` directly in controller code.

```go
logging.Infof(ctx, "pod not found for CrdbNode %q, creating it", node.Name)
logging.Warnf(ctx, "pod %q is in phase %q due to %q", pod.Name, phase, reason)
logging.Errorf(ctx, "failed to update status: %v", err)
```

Use `logging.Info/Warn/Error` (not the `f` variants) when you need structured `zap.Field`
arguments rather than a format string.

## Error handling

Use `github.com/cockroachdb/errors` for all error creation and wrapping, not the standard
library `errors` or `fmt.Errorf`. This package captures stack traces automatically.

```go
// Wrap with context (preferred over fmt.Errorf)
errors.Wrap(err, "failed to get CrdbNode")
errors.Wrapf(err, "failed to get CrdbNode %s", node.Name)

// Create new errors
errors.New("node still decommissioning")

// Preserve stack without adding message
errors.WithStack(err)
```

For Kubernetes API errors, use `kube/kubeerrors` for type checking rather than the raw
`k8s.io/apimachinery/pkg/api/errors` package:

```go
kubeerrors.IsNotFound(err)
kubeerrors.IsAlreadyExists(err)
kubeerrors.IsConflict(err)
```

## Reconciler return helpers

Use the helpers in `controllers/result.go` for all reconciler returns. Do not return
`ctrl.Result{}`, `ctrl.Result{RequeueAfter: ...}`, or any other bare `ctrl.Result` literal
directly from a reconciler function or a function whose return propagates to a reconciler.

```go
controllers.NoRequeue()           // done, no requeue
controllers.RequeueIfError(err)   // requeue on error (rate-limited)
controllers.RequeueAfter(30*time.Second, nil)  // requeue after delay
controllers.RequeueImmediately()  // requeue right away (idempotent ops)
controllers.RequeueOnDependent()  // wait for a watched dependent to change
```

Always use `N*time.Second` (or another time unit) in `RequeueAfter`. Bare integers are
nanoseconds and will produce unexpectedly fast requeues.

Call `ctl.UpdateStatus(ctx, obj)` before returning `NoRequeue()` whenever you have mutated
the object's status in memory, otherwise the status change is lost across restarts.

### Triple-return helper pattern

For sub-steps of a reconciler that may or may not short-circuit, use a triple return:

```go
func mySubStep(ctx context.Context, ...) (result ctrl.Result, done bool, err error) {
    // "not applicable" path — caller ignores result when done=false
    if conditionNotMet {
        return ctrl.Result{}, false, nil
    }
    // error path — controller-runtime ignores ctrl.Result when err!=nil
    if err := ctl.Update(ctx, obj); err != nil {
        return ctrl.Result{}, true, errors.Wrap(err, "update failed")
    }
    // requeue path
    result, _ = controllers.RequeueAfter(10*time.Second, nil)
    return result, true, nil
}
// caller:
if result, done, err := mySubStep(ctx, ...); done {
    return result, err
}
```

`ctrl.Result{}` is acceptable as the zero-value placeholder on the `done=false` path because
the caller discards it. Use `controllers.*` helpers for all `done=true` paths.

### Named return variable shadowing

When a function uses named returns, an `if err := ...; err != nil` block introduces a new
local `err` that shadows the named return. Returning `err` inside that block returns the
local value, not the named return. Prefer one of these patterns instead:

```go
// preferred: avoid named returns in functions with multiple error sites
func f() (ctrl.Result, error) { ... }

// or: assign to named return explicitly before returning
func f() (result ctrl.Result, err error) {
    if updateErr := ctl.Update(ctx, obj); updateErr != nil {
        return ctrl.Result{}, errors.Wrap(updateErr, "update failed")
    }
    ...
}
```

## Struct field comments

Document the purpose, lifecycle, and invariants of fields at declaration, not at call sites.
This is especially important for fields on reconciler structs and spec types that drive state
machines.

## Naming

Reconciler types follow the pattern `<Resource>Reconciler`. Helper types in `kube/` use a
`Ctl` suffix for controller-style wrappers. Test fixtures follow the `testenv.Env` and
`testmanifest` patterns; do not introduce ad-hoc fixtures.

## Resource manipulation patterns

Before writing new resource manipulation code, check how adjacent resources do the same thing.
For owner references use `ctrl.SetControllerReference(owner, obj, scheme)` rather than
`metav1.NewControllerRef`, which avoids hardcoding kind strings and validates through the
scheme. This is the established pattern in `nodectrl/pod.go` and should be followed for all
new owner reference assignments.

## Dead code

Remove unreachable code rather than leaving it with a defensive guard comment. If a code path
is not reachable via the current call graph and there is no concrete known future caller,
delete it.

## Idempotency in update loops

When iterating resources to apply an update, guard the API call to skip no-op cases. For
example, skip a PVC update if the owner references are already empty rather than issuing an
unnecessary patch. This matters because reconcilers run repeatedly and unnecessary API calls
add latency and noise.

## Server-side apply field ownership

Do not do a full-object `Update`/`UpdateWithOptions` on a resource that an external actor
manages via server-side apply (the CrdbCluster is applied by the tool the customer uses to
install the operator, be it a package manager, GitOps controller, or `kubectl apply`). A
client-side Update sends the whole object, so the operator's field manager takes co-ownership
of every field it serializes, including spec fields it never meant to touch like the pod
template containers. The external applier's next apply then fails with a field-ownership
conflict.

When the operator only needs to change its own metadata on such a resource (a finalizer, an
annotation), use a scoped patch instead of a full Update:

```go
base := cluster.DeepCopy()
if controllerutil.AddFinalizer(cluster, finalizerName) {
    // MergeFrom captures the diff at send time, so mutate the object between the
    // DeepCopy and the patch, not before.
    if err := ctl.Patch(ctx, cluster, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
        return ...
    }
}
```

Notes:
- Capture `base` before the mutation. `client.MergeFrom` computes its diff when the patch is
  sent, so any change made before the DeepCopy is invisible and any change after it is included.
- `ctl.Patch` does not wait for readiness (unlike Update/Apply), so there is no NoWait option
  to set and no readiness-wait deadlock to avoid.
- A patch, like Update, decodes the server response back into the object and so resets in-memory
  status. If you mutate status in the same path, call `ctl.UpdateStatus` before the patch.
- Only the CrdbNode and other operator-owned objects are safe to full-Update; they have no
  external field-manager owner.

## Array and slice access

Do not use magic indices. If a slice access by index is necessary, check the length first and
assign the element to a named variable before using it. If the index has semantic meaning
(e.g., "the first container is always the CRDB container"), document that assumption with a
comment. Prefer named accessors or helper functions over bare `items[0]` in business logic.

## Pointer construction

Go 1.26+ supports `new(value)` to create a pointer to a value (not just a type). Prefer
this over helper functions or external packages:

```go
// preferred (Go 1.26+)
ownerRef.BlockOwnerDeletion = new(true)
spec.MaxPods = new(int32(3))

// also acceptable — k8s.io/utils/ptr
ptr.To("nginx")
ptr.To(int32(3))
```

Do not use `k8s.io/utils/pointer` (deprecated) or hand-rolled `boolPtr`/`int32Ptr` helpers.

When importing `k8s.io/utils/ptr` into a test file that also has a package-level function
named `ptr`, use an import alias to avoid the conflict:

```go
import kubeptr "k8s.io/utils/ptr"
```

## Switch over if/else if

When branching on a single string or typed constant with two or more cases, use `switch`
rather than `if/else if`. golangci-lint flags this as `QF1003`:

```go
// wrong
if x == "a" { ... } else if x == "b" { ... }

// right
switch x {
case "a": ...
case "b": ...
}
```

## Shared utility packages

Before writing a new helper function, check the existing utility packages for something
reusable. If your helper is generic enough to be used across packages, add it to the
appropriate utility package instead of keeping it local.

| Package | What it provides |
|---------|-----------------|
| `pkg/utils/sliceutils` | Generic slice operations: `Filter`, `Map`, `Contains`, `SetsEqual`, `Unique`, `Partition`, `Batch` |
| `util/stringutil` | String-as-set helpers: `StringSetAdd`, `StringSetRemove` |
| `kube/` | K8s client wrappers, `Ctl`, `kubeerrors` |

When adding a new generic function to a utility package, add a corresponding test. When you
find yourself writing a one-off helper that duplicates something in these packages, use the
existing function instead.

## Dependency changes

After any change to `go.mod`, run `make vendor` which runs `go mod tidy && go mod vendor`
atomically. Do not run them separately.
