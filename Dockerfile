# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go metrics.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/llm-router .

# Distroless static: no shell, CA certificates for HTTPS model servers, and
# the unprivileged "nonroot" user (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/llm-router /usr/local/bin/llm-router
ENV LLM_ROUTER_CONFIG=/etc/llm-router/config.json
USER 65532:65532
EXPOSE 1234
ENTRYPOINT ["/usr/local/bin/llm-router"]
