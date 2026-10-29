#!/usr/bin/env bash

set -euxo pipefail

# Figure out, regardless of any symlinks, aliases, etc, where this script
# is located.
SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ]; do SOURCE="$(readlink "$SOURCE")"; done
DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"

DOCKER_REGISTRY_PROJECT_NAME=k3d-local

# The name of the docker network. This must change if it changes in the docker-compose.yaml file.
DOCKER_REGISTRY_NETWORK_NAME=k3d-local

COMMAND="${1-}"

case $COMMAND in
up)
  docker network create --driver bridge ${DOCKER_REGISTRY_NETWORK_NAME} || true
  docker compose -p ${DOCKER_REGISTRY_PROJECT_NAME} -f ${DIR}/docker-compose.yaml up -d
  ;;
down)
  docker compose -p ${DOCKER_REGISTRY_PROJECT_NAME} -f ${DIR}/docker-compose.yaml down
  ;;
*)
  echo "Unknown command: $COMMAND"
  exit 1
  ;;
esac
