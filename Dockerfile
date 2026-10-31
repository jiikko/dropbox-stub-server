# syntax=docker/dockerfile:1
# Builds a small image for Docker or Apple's `container` (README "Run in a container").
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /dropbox-stub-server .

FROM gcr.io/distroless/static-debian12
COPY --from=build /dropbox-stub-server /dropbox-stub-server
# Mount the directory to serve at /data. The stub renames and deletes files there, so it runs as
# root inside the container (the bind mount's owner on the host varies).
VOLUME ["/data"]
EXPOSE 8765
# Inside the container loopback is unreachable from the host, so listen on all interfaces and
# publish the port to the host's 127.0.0.1 only (-p 127.0.0.1:8765:8765).
ENTRYPOINT ["/dropbox-stub-server", "-root", "/data", "-addr", "0.0.0.0:8765", "-allow-non-loopback"]
