# Mutating Webhook for CrdbCluster PodTemplate

## Overview

This document describes the mutating webhook implementation that provides sensible defaults for the `PodTemplate` field in `CrdbCluster` resources.

## Problem Statement

When users want to configure pod-level settings like `nodeAffinity`, `tolerations`, or `nodeSelector` via the `PodTemplate` field, they are required by Kubernetes to also specify a `containers` array in the `PodSpec`. This creates a poor user experience because:

1. The container image is already specified in `CrdbNodeSpec.Image`
2. Users must redundantly specify container details just to set pod-level configurations
3. It's not obvious what the correct container name should be

## Solution

A mutating webhook intercepts `CrdbCluster` CREATE and UPDATE requests **before** Kubernetes validation occurs. If the webhook detects that:
- `PodTemplate` is specified, AND
- The `containers` array is empty

Then it automatically injects a default container with:
- **Name**: `cockroachdb` (from `manifests.CRDBContainerName`)
- **Image**: The value from `CrdbNodeSpec.Image`

This allows users to write manifests like:

```yaml
apiVersion: crdb.cockroachlabs.com/v1beta1
kind: CrdbCluster
metadata:
  name: my-cluster
spec:
  template:
    spec:
      image: cockroachdb/cockroach:v23.1.0
      podTemplate:
        spec:
          # No need to specify containers!
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                - matchExpressions:
                  - key: node-type
                    operator: In
                    values:
                    - crdb
```

The webhook will automatically inject the container, making it equivalent to:

```yaml
apiVersion: crdb.cockroachlabs.com/v1beta1
kind: CrdbCluster
metadata:
  name: my-cluster
spec:
  template:
    spec:
      image: cockroachdb/cockroach:v23.1.0
      podTemplate:
        spec:
          containers:
          - name: cockroachdb
            image: cockroachdb/cockroach:v23.1.0
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                - matchExpressions:
                  - key: node-type
                    operator: In
                    values:
                    - crdb
```

## Implementation Details

### Files Modified/Created

1. **`manager/mutating_webhook.go`** (new)
   - Implements the `MutatingWebhook` handler
   - Intercepts `CrdbCluster` resources
   - Injects default container when needed

2. **`manager/mutating_webhook_test.go`** (new)
   - Unit tests for the mutating webhook
   - Tests injection logic
   - Tests that existing containers are not overwritten

3. **`manager/manager.go`** (modified)
   - Registers the `/mutate` endpoint alongside `/validate`

4. **`manifests/internal/yaml/webhook.yaml`** (modified)
   - Adds `MutatingWebhookConfiguration` resource
   - Configures webhook to intercept CrdbCluster CREATE/UPDATE operations

5. **`manifests/webhook.go`** (modified)
   - Updates manifest loader to handle both mutating and validating webhooks
   - Sets CA bundle for both webhook configurations

### Webhook Configuration

The mutating webhook is configured to:
- **Trigger on**: CREATE and UPDATE operations
- **Resources**: `crdbclusters` in the `crdb.cockroachlabs.com` API group
- **Failure Policy**: Fail (reject requests if webhook is unavailable)
- **Timeout**: 20 seconds
- **Side Effects**: None

### Order of Operations

1. User submits a `CrdbCluster` manifest
2. **Mutating webhook** runs first, injecting defaults if needed
3. **Validating webhook** runs second, validating the (potentially mutated) object
4. Kubernetes performs schema validation
5. Object is persisted to etcd
6. Controller reconciles the object

## Testing

Run the tests with:

```bash
go test ./manager -run TestMutatingWebhook
```

The test suite covers:
- ✅ Injection of default container when PodTemplate has no containers
- ✅ Preservation of user-specified containers
- ✅ No mutation when PodTemplate is not specified
- ✅ Preservation of other PodSpec fields (affinity, etc.)

## Deployment

The mutating webhook is automatically deployed when the operator is installed. The webhook requires:
- TLS certificates (managed by the operator)
- Service account with appropriate RBAC permissions
- Webhook service running on port 9443

## Backward Compatibility

This change is **fully backward compatible**:
- Existing manifests that specify containers continue to work unchanged
- Existing manifests without PodTemplate continue to work unchanged
- Only new manifests with PodTemplate but no containers benefit from the injection

## Future Enhancements

Potential future improvements:
1. Support for other default fields (resources, security context, etc.)
2. Validation that the injected container name matches `CRDBContainerName`
3. Support for multiple container injection patterns
4. Metrics on webhook invocations and mutations

## Troubleshooting

### Webhook Not Working

Check webhook logs:
```bash
kubectl logs -n <operator-namespace> -l app=cockroach-operator
```

### Certificate Issues

Verify webhook configuration has CA bundle:
```bash
kubectl get mutatingwebhookconfiguration cockroach-mutating-webhook-config -o yaml
```

### Webhook Rejecting Requests

Check the webhook's failure policy:
```bash
kubectl get mutatingwebhookconfiguration cockroach-mutating-webhook-config -o jsonpath='{.webhooks[0].failurePolicy}'
```

If set to "Fail", the webhook must be available for cluster creation to succeed.
