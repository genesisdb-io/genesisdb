ARG GO_IMAGE=golang:1.26.10-alpine3.23
ARG RUNTIME_IMAGE=alpine:3.23
FROM ${GO_IMAGE} AS builder

# VERSION and CHANNEL are baked into the binary, see Makefile. A local build
# without build arguments reports 0.0.0-dev/development.
ARG VERSION=0.0.0-dev
ARG CHANNEL=development
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The license inventory of the linked modules is generated inside the build,
# from the module cache, so it always matches the binary.
RUN go install github.com/google/go-licenses/v2@v2.0.1 \
    && PATH="$(go env GOPATH)/bin:$PATH" scripts/third-party-licenses.sh
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/genesisdb-io/genesisdb/v2/packages/core.Version=${VERSION} -X github.com/genesisdb-io/genesisdb/v2/packages/core.ReleaseChannel=${CHANNEL}" \
    -o /out/genesisdb ./cmd/genesisdb

FROM ${RUNTIME_IMAGE}

WORKDIR /app
RUN apk add --no-cache tzdata ca-certificates \
    && addgroup -g 10001 genesisdb \
    && adduser -D -H -u 10001 -G genesisdb genesisdb \
    && mkdir -p /app/data \
    && chmod 700 /app/data \
    && chown genesisdb:genesisdb /app/data
COPY --from=builder /out/genesisdb /app/genesisdb
COPY --from=builder /src/LICENSE /app/LICENSE
COPY --from=builder /src/THIRD_PARTY_LICENSES /app/THIRD_PARTY_LICENSES

ENV GENESISDB_DATA_DIR=/app/data \
    GENESISDB_HOST=0.0.0.0
USER 10001:10001
EXPOSE 8080
CMD ["/app/genesisdb"]
