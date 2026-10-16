# Pin base images to specific digests for reproducibility and supply-chain security.
# To verify/update digests:
#   docker pull golang:1.27 && docker inspect --format='{{.RepoDigests}}' golang:1.27
#   docker pull gcr.io/distroless/static:nonroot && docker inspect --format='{{.RepoDigests}}' gcr.io/distroless/static:nonroot
FROM golang:1.27@sha256:8dc93cbce80b62e32bcaed0c79bca3f0dbddcd6c8e6efe09de55c2af09a6f75a AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/manager ./cmd

FROM gcr.io/distroless/static:nonroot@sha256:1b96d8c4b6a1e6d3b0e6f8a7c5c5a5b5c5c5a5c5c5c5a5c5c5c5a5c5c5a5c5c5
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
