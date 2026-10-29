# deploy

This is a parent chart for the cluster and operator subcharts. It can be used to deploy both the
operator and a CRDB cluster at the same time.

> This is a WIP and currently only used for local development.

## Common Setup

* Stand up a local cluster (`[BETACTL=1] make local-cluster`).
* Ensure you've got helm installed (`make bin/helm`).

## Basic Installation of a 3-Node Cluster

* Run `bin/helm install [FLAGS] <clusterName> ./deploy`.
  * e.g. `bin/helm install -n us-east-1 --create-namespace crdb ./deploy`

## Test FluentBit Integration

Now that modern versions of CRDB can send logs directly to fluentd we don't necessarily need to run a 
sidecar for each pod. To test this out you'll need to run fluentd in a daemon set and configure CRDB
nodes accordingly to write logs to fluentd over TCP.

```bash
# Install the DaemonSet
kubectl apply -f deploy/support/fluentbit.yaml

# Deploy a new cluster that logs to fluentd
bin/helm install -n us-east-1 --create-namespace <clusterName> ./deploy -f deploy/values/fluentbit.yaml
```
