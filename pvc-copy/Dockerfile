# A minimal image that copies data between two mounted PersistentVolumeClaims
# using rsync. Intended to run as the Pod of a Kubernetes Job managed by the
# pvc-autoscaler.
FROM alpine:3.24
RUN apk add --no-cache rsync
# Run as non-root, mirroring the project's USER 65532:65532 convention.
USER 65532:65532

ENTRYPOINT ["rsync"]
