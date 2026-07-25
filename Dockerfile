# beeline — the same binary for every role: `serve` (a head) and `work` (a
# follower). Built for `make fulldemo`'s docker-compose cluster, but it is a
# perfectly ordinary deployment image.
#
# The build needs cgo — uber/h3-go wraps the C H3 library — so the builder pulls
# in a toolchain and the binary links against the builder's musl libc, which the
# matching Alpine runtime provides. (The rest of the tree is pure Go: modernc's
# SQLite driver needs no C, and distributed mode never opens SQLite at all.)
#
# Version metadata mirrors scripts/build.sh's ldflags, but arrives as build args
# because .dockerignore keeps .git out of the build context. docker-compose.yml
# fills them in from the host's git checkout; they default to "unknown".

FROM golang:1.26-alpine AS builder

RUN apk add --no-cache build-base

WORKDIR /src

# Dependencies first: this layer is cached until go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG COMMIT_HASH=unknown
ARG BUILD_TIME=unknown
ARG COMMIT_TIME=unknown

ENV CGO_ENABLED=1
RUN go build -trimpath \
  -ldflags "-s -w \
  -X github.com/primandproper/beeline/version.CommitHash=${COMMIT_HASH} \
  -X github.com/primandproper/beeline/version.BuildTime=${BUILD_TIME} \
  -X github.com/primandproper/beeline/version.CommitTime=${COMMIT_TIME}" \
  -o /out/beeline ./cmd/main

FROM alpine:3.21

# ca-certificates for outbound HTTPS (a real OSRM/Valhalla provider); wget comes
# from busybox and backs the compose healthchecks.
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /out/beeline /usr/local/bin/beeline
# COPY preserves the host's modes, and `make configs` renders these files 0600 —
# world-readable is what the non-root runtime user below needs.
COPY config/ /app/config/
RUN chmod -R a+rX /app/config

# A non-root user: nothing in the image is written to at runtime (in distributed
# mode all state lives in Postgres/Redis).
RUN adduser -D -H -u 10001 beeline
USER beeline

EXPOSE 8080 8081

ENTRYPOINT ["beeline"]
CMD ["serve", "--config", "config/cluster.json"]
