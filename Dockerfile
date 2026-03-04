FROM docker.io/golang:alpine AS build
ARG VERSION=dev
WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build — pure Go (no CGo needed), gormspan is in-tree
COPY . .
RUN go build -ldflags="-s -w -X main.version=$VERSION" \
    -o /usr/bin/headscale ./cmd/headscale && \
    test -e /usr/bin/headscale

FROM alpine:latest
RUN apk add --no-cache ca-certificates && \
    mkdir -p /var/run/headscale /var/lib/headscale
COPY --from=build /usr/bin/headscale /usr/bin/headscale

# gormspan runtime configuration (override at deploy time)
# When GORMSPAN_ENDPOINT is set, migrations and schema validation are
# automatically skipped (schema is managed by the out-of-band service).
ENV GORMSPAN_ENDPOINT="http://172.17.0.1:9999/sync"
ENV GORMSPAN_DB="headscale"

ENTRYPOINT ["headscale"]
CMD ["serve"]
EXPOSE 8080/tcp