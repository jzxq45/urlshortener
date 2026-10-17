# build stage compiles a static binary: modernc.org/sqlite is pure Go, so
# CGO can be disabled and the final image runs from scratch
# must be >= the toolchain in go.mod (1.27), see CI note
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
    && mkdir -p /out/data && chmod 0777 /out/data

# runtime: no shell, no packages — smallest surface the server needs
FROM scratch
COPY --from=build /out/server /server
# uid 65534 runs the server; mkdir+chmod in the build stage does not survive
# the volume bootstrap on all Docker versions, so ownership/mode are re-set
# here explicitly (SQLITE_CANTOPEN(14) otherwise)
COPY --from=build --chown=65534:65534 --chmod=0777 /out/data /data
# /r/ redirects and static assets are embedded; only the db file lives here
VOLUME ["/data"]

USER 65534:65534
ENV SHORTENER_DB_PATH=/data/urlshortener.db
EXPOSE 8080

ENTRYPOINT ["/server"]
