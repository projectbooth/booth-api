# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booth-api ./cmd/api

# booth-core's credential sidecar (ADR 0095), which booth-api runs as child processes, one per
# workspace and key creator (ADR 0103). Pinned by digest, never a tag, the same image
# booth-pipeline and booth-notebooks pin; bump all three together.
FROM ghcr.io/projectbooth/credential-sidecar@sha256:6a0a795efd27f165e0714beb163d91f5c2feff55cfdc6f287aee979ae02cce14 AS sidecar

# Distroless static + nonroot (uid/gid 65532): no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/booth-api /booth-api
COPY --from=sidecar /credential-sidecar /credential-sidecar
USER nonroot:nonroot
ENTRYPOINT ["/booth-api"]
