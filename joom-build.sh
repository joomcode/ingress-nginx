#!/bin/bash

export REGISTRY="jfrog.joom.it/docker-registry/joom-ingress-nginx/controller"

export BASE_TAG
BASE_TAG=$(cat TAG)
export TAG="${BASE_TAG}-batching-patch"

export BASE_IMAGE
BASE_IMAGE=$(cat NGINX_BASE)

export COMMIT
COMMIT=$(git rev-parse --short HEAD)

docker buildx build --no-cache --platform=linux/amd64 \
--build-arg BASE_IMAGE="$BASE_IMAGE" \
--build-arg VERSION="$TAG" \
--build-arg COMMIT_SHA="$COMMIT" \
--build-arg BUILD_ID=UNSET \
 -t "${REGISTRY}:${TAG}" rootfs \
 --load

docker push "${REGISTRY}:${TAG}"
