# The build stage must satisfy go.mod's `go` directive. It previously pinned
# golang:1.23 while go.mod required 1.25, so `go mod download` failed on the
# first line and the image could not be built at all.
#
# The version is stamped in so /api/version reports something other than "dev"
# inside the container (same -X target the SPK build uses).
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG APP_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags="-s -w -X m365-copilot2api/internal/web.Version=${APP_VERSION}" \
        -o /out/m365-copilot2api ./cmd/server

FROM alpine:3.20
RUN addgroup -S m365 && adduser -S -G m365 m365 \
    && mkdir -p /data /app
WORKDIR /app
COPY --from=build /out/m365-copilot2api /app/m365-copilot2api
RUN chown -R m365:m365 /app /data
USER m365
EXPOSE 4141
ENV M365_LISTEN=0.0.0.0:4141 \
    M365_DATA_DIR=/data \
    M365_CONFIG=/data/accounts.json \
    M365_TOKEN_CACHE=/data/token-cache.json \
    M365_SESSION_CACHE=/data/sessions.json \
    M365_API_KEYS=/data/api-keys.json \
    M365_ADMIN_PASSWORD_FILE=/data/admin-password \
    M365_ADMIN_PASSWORD_RESET_FILE=/run/secrets/m365_admin_password

# Note: the SPA and admin console are embedded in the binary via go:embed, so
# there is nothing to copy alongside it. An earlier revision copied web/ into the
# image, which suggested the files were served from disk when they are not.

# Probes /api/version rather than /api/health: health sits behind the admin
# session (it is not on the middleware's public allow-list), so a healthcheck
# against it would always report 401 and the container would never be healthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:4141/api/version || exit 1

VOLUME ["/data"]
ENTRYPOINT ["/app/m365-copilot2api"]
