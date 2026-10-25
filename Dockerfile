# Official rerun: 2 CPU, 1 GiB, read-only root filesystem, only /tmp writable, no root privileges.
# Before submission pin both base images by digest (golang:1.25@sha256:..., distroless@sha256:...).
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/orchestrator ./cmd/orchestrator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/orchestrator /orchestrator
COPY challenge /challenge
USER nonroot
ENTRYPOINT ["/orchestrator", "-catalog", "/challenge"]
