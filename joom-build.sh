export TAG="v1.3.1-batching-patch"
export REGISTRY="jfrog.joom.it/docker-registry/joom-ingress-nginx"

make build image

docker push $REGISTRY/controller:$TAG
