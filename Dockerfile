FROM golang:1.26-trixie AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -v -o /out/wormtamer ./cmd/wormtamer

FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o nobody -g nogroup /var/lib/wormtamer \
    && install -d /etc/wormtamer

COPY --from=build /out/wormtamer /usr/local/bin/wormtamer

EXPOSE 8080
STOPSIGNAL SIGTERM

USER nobody
ENTRYPOINT ["/usr/local/bin/wormtamer"]
CMD ["-config", "/etc/wormtamer/config.json"]
