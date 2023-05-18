#!/bin/bash

export TAG="v1.3.1-batching-patch"
export REGISTRY="jfrog.joom.it/docker-registry/joom-ingress-nginx"
export ARCH="amd64"

make build image

docker push $REGISTRY/controller:$TAG
