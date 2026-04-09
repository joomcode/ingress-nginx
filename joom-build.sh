#!/bin/bash
set -e

export REGISTRY="jfrog.joom.it/docker-registry/joom-ingress-nginx"

export BASE_TAG
BASE_TAG=$(cat TAG)
export TAG="${BASE_TAG}-batching-patch-$(date -u +%d%m%y-%H%M%S)"

ARCHES=(amd64 arm64)
IMAGE="${REGISTRY}/controller"

for ARCH in "${ARCHES[@]}"; do
  make build ARCH="$ARCH"
  make image PLATFORM="linux/$ARCH" TAG="${TAG}-${ARCH}" ARCH="$ARCH" REGISTRY="$REGISTRY"
  docker push "${IMAGE}:${TAG}-${ARCH}"
done

docker manifest create "${IMAGE}:${TAG}" \
  "${IMAGE}:${TAG}-amd64" \
  "${IMAGE}:${TAG}-arm64"

docker manifest push "${IMAGE}:${TAG}"
