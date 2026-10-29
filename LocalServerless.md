# Local Serverless

`make local-serverless-cluster` creates a k3d cluster and installs Serverless.
`make clean` tears down the environment.

## Pushing Custom Images

The makefile sets up a local registry that is available to the cluster. The
registry can be used to push custom binaries into the Serverless host.

```
# Push a cockroach image into the local repository
docker tag cockroachdb/cockroach:latest localhost:5000/cockroachdb:some_tag
docker push localhost:5000/cockroachdb:some_tag

# Configure the sqlproxy's image
kubectl config set-context --current --namespace test-integration-run-serverless 
kubectl set image deployment/sqlproxy sqlproxy=localhost:5000/cockroachdb:some_tag
```

## Connecting to Serverless

The cluster contains three pre-created tenants. Each of the tenants contains a
user with the name "me" and password "password". Connecting to the tenant
requires port forwarding to the sqlproxy.

* happy-koala-10
* scary-bear-11
* eager-beaver-12

```
# Set up port forwarding to sqlproxy
kubectl config set-context --current --namespace test-integration-run-serverless 
kubectl port-forward deployment/sqlproxy 26257:26257

# Connect to the cluster
cockroach sql --url 'postgres://me:password@127.0.0.1:26257?sslmode=require&options=--cluster=happy-koala-10'
```
