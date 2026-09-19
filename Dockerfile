# Multi-stage build: the final image carries only the static binary, so a
# deployment does not ship a toolchain.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Copy the module files first so dependency download is cached independently of
# source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off produces a static binary that runs on a scratch/distroless base.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=docker" -o /out/llmgateway .

FROM alpine:3.20

# ca-certificates is required to reach HTTPS upstreams; tzdata makes log
# timestamps locally meaningful.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 gateway

COPY --from=build /out/llmgateway /usr/local/bin/llmgateway

USER gateway

EXPOSE 8080

# The gateway is stateless: configuration arrives from the config file and the
# environment, and counters live in memory for the life of the process.
ENTRYPOINT ["/usr/local/bin/llmgateway"]
CMD ["--config", "/etc/llmgateway/gateway.yaml"]
