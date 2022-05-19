docker buildx build --no-cache --platform=linux/amd64 \
--build-arg BASE_IMAGE=k8s.gcr.io/ingress-nginx/nginx:81c2afd975a6f9a9847184472286044d7d5296f6@sha256:a71ac64dd8cfd68341ba47dbdc4d8c2cb91325fce669875193ea0319118201b5 \
--build-arg VERSION=v0.47.0-batching-patch \
--build-arg COMMIT_SHA=7c2d7470d \
--build-arg BUILD_ID=UNSET \
 -t jfrog.joom.it/docker-registry/test-rationalex/ingress-nginx/controller:v0.47.0-batching-patch rootfs \
 --load
