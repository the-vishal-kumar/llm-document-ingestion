# syntax=docker/dockerfile:1

# Build: static binary, no cgo.
FROM golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ingest ./cmd/ingest

# Runtime: Debian slim rather than distroless, because PDF text extraction
# needs poppler's pdftotext. Runs as an unprivileged user.
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update \
 && apt-get install -y --no-install-recommends poppler-utils ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --create-home --home-dir /app ingest
COPY --from=build /out/ingest /usr/local/bin/ingest
USER 10001
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["ingest"]
CMD ["serve", "-addr", "0.0.0.0:8080", "-store", "/app/var"]
