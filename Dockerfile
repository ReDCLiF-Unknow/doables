# Build: everything is pure Go (SQLite included), so CGO is off and the result
# is a static binary that needs nothing at runtime.
#
# The build stage always runs on the builder's own architecture and
# cross-compiles for the target, so multi-arch images build in seconds
# instead of crawling through emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/doables-server ./cmd/server \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/doables ./cmd/doables

# Runtime: Alpine rather than scratch, so you can still `docker exec ... sh`
# and look around when something misbehaves.
FROM alpine:3.21

RUN adduser -D -u 10001 doables \
 && mkdir -p /data \
 && chown doables:doables /data

COPY --from=build /out/doables-server /out/doables /usr/local/bin/

USER doables
WORKDIR /data
VOLUME /data
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/welcome >/dev/null || exit 1

# Note the address: the default is localhost, which inside a container would
# only be reachable from the container itself.
CMD ["doables-server", "-addr", "0.0.0.0:8080", "-db", "/data/doables.db"]
