# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/membership ./cmd/membership

# Static binary on distroless: no shell, CA certificates and tzdata included,
# runs as the unprivileged "nonroot" user (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/membership /usr/local/bin/membership
USER nonroot:nonroot
ENV LISTEN_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/membership", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/membership"]
CMD ["serve"]
