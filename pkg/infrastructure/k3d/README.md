# K3S/K3D Support for Testing and Development

K3D and K3S are a pair of tools which make it very easy to spin up a
Kubernetes cluster that runs in Docker on the host machine.

One issue with K3S is that it runs Docker-in-Docker, which means the
Docker daemons run inside Docker containers. One consequence of this
is that the daemon's internal image registry is empty when the cluster
starts up and all pod images will be pulled again everytime the cluster
is restarted. If local access to the Internet is slow, this can make it
very slow, if not impossible, to run tests.

K3S supports using local Docker image registries, which makes it
quite easy to have a local Docker registry that can be used to cache
images from the Internet, eliminating the need to load them after every
cluster restart.

This is also helpful for local development. Instead of having to upload
images to the cloud, it is possible to run a local registry to place
fresh image builds into and use them inside your k3s clusters.

The local registry on your dev machine will be insecure.

## Initial Setup

There are a couple of steps necessary to use caching and a local registry.
One is determining which external registries should be cached. The second
is telling K3S which registries are cached.

### Which Registries to Cache

The initial setup involves telling K3S which image repositories are being
cached. The Docker registry image can only mirror a single remote registry,
so it is necessary to create a registry container for each external repository.
You can see how this is done in the `docker-compose.yaml` file. It
mirrors `docker.io`, `quay.io`, and `us.gcr.io`. `quay.io` is particularly
interesting as it is only supporting version1 of the Docker registry format
so requires a bit of extra configuration. 

A new registry will need to be added for each external registry you wish to
cache.

This `docker-compose.yaml` also starts up a registry which can be used to
push locally built images into. It is exposed on port `5000` on `localhost`.
Usage will be described later in this document. The caching registries
are not exposed to localhost, they are private to the Docker network the
registries run in.

### Configuring K3S to Use the Caches and Dev Registry

The K3S containers need to know which Docker registries are being proxied.
An example file is `registries.yaml`.

The file specifies all mirrors. It can also include any information needed
for authenticating to a remote registry. Details can be found at
https://k3d.io/usage/guides/registries/.

There will be an entry for each and every local registry.

```
mirrors:
  "localhost:5000":
    endpoint:
      - http://registry-localhost:5000
  quay.io:
    endpoint:
      - http://registry-quayio:5000
  docker.io:
    endpoint:
      - http://registry-dockerio:5000
  us.gcr.io:
    endpoint:
      - http://registry-us-gcr-io:5000
```

The top one is the registry that is used for local development. Use of
this will be described later.

## Using the Local Registries

### Starting and Stopping the Registries

The local registries can be started and stopped using the `registries.sh`
command.

```
registries.sh up
```

brings the registries up as a Docker stack called `k3d-local`.

```
registries.sh down
```

will bring the registries down.

### Starting and Stopping a Dev Cluster

The script `dev-cluster.sh` is used to create a K3S development cluster.
This is outside the cluster created with the various integration and
E2E tests inside the `managed-cluster` codebase. A dev cluster can be used
for development of cloud components such as backupper or other services
meant to run inside a Kubernetes cluster.

A cluster with a single node can be started with

```
dev-cluster.sh up
```

and stopped with


```
dev-cluster.sh down
```

You can also specify the number of nodes you wish in the cluster. For example,

```
dev-cluster.sh up --nodes 3
```

will create a cluster with 3 nodes.

The flag ``--version`` lets you specify the version of Kubernetes to use.

### Building and Deploying Images Into The Cluster

It is now possible to locally build a Docker image and get it into the local K3S cluster
without first having to upload it to a remote Docker registry.

Say you locally build the image `cockroachcloud/backupper2:v3.4.1`. This image is now
found in your local Docker daemon's list of images. To make this local build available
to the local K3S cluster, use the following commands:

```
docker tag cockroachcloud/backupper2:v3.4.1 localhost:5000/cockroachcloud/backupper2:v3.4.1
docker push localhost:5000/cockroachcloud/backupper2:v3.4.1
```

Your Kubernetes descriptor should then refer to the image as `localhost:5000/cockroachcloud/backupper2:v3.4.1`
## How It Works

The caching registries and placed in a Docker network called `k3d-local`.
This Docker network is used as the network the K3S cluster runs in, which
means that the K3S containers have access to the registries through their
network interfaces.
