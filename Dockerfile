# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booth-api ./cmd/api

# booth-core's credential sidecar (ADR 0095), which booth-api runs as child processes: one per
# workspace and key creator for reads (ADR 0103), one per API and granting owner for writes
# (ADR 0117). Pinned by digest, never a tag. This is the image published from booth-core a562691
# (v0.3.5), the first that accepts --access=tablewrite; read and readwrite are unchanged from the
# digest booth-pipeline and booth-notebooks pin.
FROM ghcr.io/projectbooth/credential-sidecar@sha256:a5bb9dee7914a097868512608a5d0c023e1968db824744a134880adbc8ce9907 AS sidecar

# Distroless static + nonroot (uid/gid 65532): no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/booth-api /booth-api
COPY --from=sidecar /credential-sidecar /credential-sidecar
USER nonroot:nonroot
ENTRYPOINT ["/booth-api"]
