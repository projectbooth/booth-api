# syntax=docker/dockerfile:1
# The real-stack check (check/main.go), run as a Job inside the test cluster. Build context is the
# repo root; Dockerfile.dockerignore (next to this file) keeps test/ in, which the root
# .dockerignore excludes from booth-api's own image.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/check ./test/integration/realstack/check

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/check /check
ENTRYPOINT ["/check"]
