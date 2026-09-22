# syntax=docker/dockerfile:1.7
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/priorauth ./cmd/priorauth && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/payer-sim ./cmd/payer-sim

FROM gcr.io/distroless/static-debian12:nonroot AS engine
ARG VERSION=dev
LABEL org.opencontainers.image.title="priorauth-engine" \
      org.opencontainers.image.description="Prior authorization determination, evidence assembly and submission tracking" \
      org.opencontainers.image.source="https://github.com/udaykishore-resu/priorauth-engine" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /app
COPY --from=build /out/priorauth /app/priorauth
COPY --from=build /out/payer-sim /app/payer-sim
COPY rules /app/rules
ENV PA_HTTP_ADDR=:8080 PA_RULES_DIR=/app/rules PA_VERSION=${VERSION}
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/priorauth"]
