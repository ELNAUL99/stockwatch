# syntax=docker/dockerfile:1

# ---- Build stage ------------------------------------------------------------
# Pinned to a specific Go minor version rather than :latest, so a Go release
# cannot change what this image builds without a commit saying so.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies first, in their own layer. Docker caches layers by the files they
# touch, so copying go.mod/go.sum alone means `go mod download` is only re-run
# when dependencies actually change — not on every source edit. Copying
# everything up front would invalidate this on each keystroke.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO_ENABLED=0 is what makes the binary static, and static is what makes the
# scratch-like final stage possible. With cgo on, the binary would dynamically
# link against the builder's musl and fail at runtime in an image that has no
# libc at all.
#
# -trimpath strips local filesystem paths from the binary, which makes builds
# reproducible and avoids leaking the builder's directory layout in panics.
# -s -w drop the symbol table and DWARF data, cutting roughly a third off the
# size. The trade-off is that a core dump is far less readable; stack traces
# still carry function names, so this is a fair swap for a service.
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/stockwatch ./cmd/server \
 && CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
      -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
      -o /out/seed ./cmd/seed

# ---- Runtime stage ----------------------------------------------------------
# distroless/static rather than scratch. Both are ~2MB and neither has a shell,
# but static supplies three things scratch does not and that a real service
# needs:
#
#   - ca-certificates, so a TLS connection to a managed Postgres verifies
#     instead of failing with "x509: certificate signed by unknown authority"
#   - /etc/passwd with a nonroot user, so USER below resolves to a real entry
#   - tzdata, so any time.LoadLocation call works
#
# The absence of a shell is a security property, not an oversight: an attacker
# who achieves RCE finds no sh, no package manager and no coreutils to pivot
# with. It is also why the compose healthcheck below cannot use curl.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/stockwatch /usr/local/bin/stockwatch
COPY --from=build /out/migrate    /usr/local/bin/migrate
# The seeder ships too, so the demo works with only Docker installed:
#   docker compose exec stockwatch /usr/local/bin/seed
COPY --from=build /out/seed       /usr/local/bin/seed

# Run unprivileged. distroless defines nonroot as uid 65532; if the container
# runtime is later told to drop capabilities, there are none to drop.
USER nonroot:nonroot

EXPOSE 8080

# Exec form, not shell form. Shell form would wrap the process in /bin/sh -c,
# which this image does not have — and even where it exists it makes the shell
# PID 1, so SIGTERM goes to the shell rather than to the server and the graceful
# shutdown path never runs.
ENTRYPOINT ["/usr/local/bin/stockwatch"]
