FROM --platform=linux/amd64 gcr.io/distroless/static:nonroot
WORKDIR /
COPY manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
