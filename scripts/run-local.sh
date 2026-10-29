#!/bin/bash

set -e
set -o pipefail

echo "Locating existing operator deployment"
deployments="$(kubectl get deploy --all-namespaces --selector app=cockroach-operator --output=json)"
count="$(echo "$deployments" | jq '[ .items[].metadata.namespace ] | length')"
if [[ $count -ne 1 ]]; then
  echo "Expected 1 'cockroach-operator' deployment, found $count instead"
  exit 1
fi
operator="$(echo "$deployments" | jq '.items[0]')"
namespace="$(echo "$operator" | jq -r '.metadata.namespace')"
original_replicas="$(echo "$operator" | jq -r '.spec.replicas')"

echo "Retrieving configuration from cluster"
crdb_certs="$(kubectl get secret -n "$namespace" cockroach-operator-certs -o json)"
certs_dir="$(pwd)/certs"

mkdir -p "$certs_dir"
echo "$crdb_certs" | jq -r '.data."ca.crt" | @base64d' > "$certs_dir/ca.crt"
echo "$crdb_certs" | jq -r '.data."tls.crt" | @base64d' > "$certs_dir/tls.crt"
echo "$crdb_certs" | jq -r '.data."tls.key" | @base64d' > "$certs_dir/tls.key"

function cleanup() {
  echo "Restoring operator deployment state..."
  # 1. Restore replicas
  kubectl -n "$namespace" scale deploy cockroach-operator --replicas="$original_replicas"

  # 2. Remove --webhook-only arg if we added it
  if [[ "${ENABLE_WEBHOOKS}" == "true" ]]; then
      echo "Removing --webhook-only flag from in-cluster operator..."
      # we use 'replace' instead of 'apply' to force the update even if there are no other changes,
      # and to avoid "kubectl.kubernetes.io/last-applied-configuration" issues with large objects
      kubectl -n "$namespace" get deploy cockroach-operator -o json | \
      jq 'del(.status, .metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.generation) | .spec.template.spec.containers[0].args |= map(select(. != "--webhook-only"))' | \
      kubectl replace --force -f -
  fi

  rm -rf "$certs_dir"
}
trap cleanup EXIT

# Case 1: Run webhooks in cluster (ENABLE_WEBHOOKS=true)
# - We assume the in-cluster operator image supports --webhook-only
# - We patch the deployment to use --webhook-only
# - We ensure 1 replica is running in cluster (for webhooks) and 1 locally (for controllers)
# Case 2: Run everything locally (default)
# - We scale down the in-cluster operator to 0
# - We run the local operator (controllers only, effectively)
# In both cases, the local operator runs with --disable-leader-election to avoid conflicts
# or to simply run as a standalone controller.

if [[ "${ENABLE_WEBHOOKS}" == "true" ]]; then
  echo "ENABLE_WEBHOOKS=true: Configuration for running webhooks in-cluster"
  echo "Ensuring 1 replica for in-cluster operator..."
  if [[ "$original_replicas" -ne 1 ]]; then
     kubectl -n "$namespace" scale deploy cockroach-operator --replicas=1
  fi

  echo "Patching in-cluster operator to run with --webhook-only..."
  # Try adding the arg to the existing args list
  if ! kubectl -n "$namespace" patch deploy cockroach-operator --type='json' -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--webhook-only"}]' 2>/dev/null; then
      echo "Patching args list failed (likely undefined), creating args list..."
      kubectl -n "$namespace" patch deploy cockroach-operator --type='json' -p='[{"op": "add", "path": "/spec/template/spec/containers/0/args", "value": ["--webhook-only"]}]'
  fi
else
  echo "Scaling down existing operator deployment in namespace $namespace"
  kubectl -n "$namespace" scale deploy cockroach-operator --replicas=0
fi

echo "Retrieving api-server address"
ctx="$(kubectl config current-context)"
clstr="$(kubectl config view -o json | jq -r --arg ctx "$ctx" '.contexts[] | select(.name == $ctx) | .context.cluster')"
endpoint="$(kubectl config view -o json | jq -r --arg clstr "$clstr" '.clusters[] | select(.name == $clstr) | .cluster.server')"

# A corporate network security agent may try to mitm all outgoing traffic.
# This creates a problem as the k8s api-server has a self-signed cert which is
# embedded in the kubeconfig and unknown to the agent. Luckily for us, such
# agents give up after the first try if they encounter these types of cert
# errors (a common problem with CLI tooling) and will not intercept future
# requests to the same IP.
echo "Curl-ing api-server so a network security agent does not interfere"
set +e
curl --silent --connect-timeout 5 --max-time 10 "$endpoint" > /dev/null
set -e

node_name="$(kubectl get node -o 'jsonpath={.items[0].metadata.name}')"
echo "Setup complete, starting operator:"
NODE_NAME="$node_name" NAMESPACE="$namespace" go run . \
  -metrics-addr=0.0.0.0:8080 -rpc-addr=0.0.0.0:9070 \
  --local-mode \
  --skip-tenant-controller=true \
  --disable-tenant-finalization=true \
  --disable-leader-election=true \
  --certs-dir="$certs_dir"
