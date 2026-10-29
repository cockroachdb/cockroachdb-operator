# Building inotifywait-base Base Image

The inotifywait image uses the inotifywait-base image as base image for the repository which can be build using Dockerfile present.

The current base image is v2.0.1. Mostly we don't need to build it again unless something is
changed in the Dockerfile, or the upstream UBI image it is built from picks up a patch we need.
Steps to build and push the image:

```
docker buildx build --platform linux/amd64,linux/arm64 -t us-docker.pkg.dev/releases-prod/self-hosted/inotifywait-base:<tag> --push .
```

The digest does not exist until that push lands, so afterwards update the `@inotifywait-base-ubi`
digest in the WORKSPACE file to the multi-arch manifest list the push produced. Bazel resolves the
base from that pin, not from the tag.

# Building inotifywait image

After merging your change, build and push the inotifywait image with:

```
bazel run --config=ubi //services/inotifywait:push_production_ubi --stamp
```

This command will push the image in production, look for the image in the Artifact Registry
[here](https://console.cloud.google.com/artifacts/docker/releases-prod/us/self-hosted).


Finally, update the all references in the cockroach-enterprise-operator repository. For example, https://github.com/cockroachlabs/cockroach-enterprise-operator/blob/e3b96804a1270d2244e4cca24f7d5a9193c22a70/pkg/constants/images_ubi.go#L11.
