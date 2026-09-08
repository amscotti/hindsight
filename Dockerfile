# Hindsight ships as one static binary on top of scratch: no shell, no
# package manager, no sibling files. Everything the server needs at
# runtime (web assets, migrations, templates) is embedded in the
# binary; the only disk touch is the SQLite file under /data.
FROM golang:1.27.1 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go generate ./... && \
  CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /hindsight .

FROM scratch

# No HEALTHCHECK: scratch has no shell or curl/wget for a CMD probe, and
# the binary has no healthcheck flag. Orchestrators should probe /healthz.
# No CA certs bundled: modernc.org/sqlite is pure Go and the server makes
# no outbound TLS calls. Runs as non-root; /data must be writable by 65532.
USER 65532:65532

WORKDIR /data
VOLUME /data

ENV PORT=8080 DB_PATH=/data/hindsight.db

EXPOSE 8080

COPY --from=build /hindsight /hindsight

ENTRYPOINT ["/hindsight"]
