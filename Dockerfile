FROM golang:1.27-trixie AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -v -o /out/wormtamer ./cmd/wormtamer

FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends bash ca-certificates curl fd-find git passwd ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 wormtamer-review \
    && useradd --system --uid 65532 --gid 65532 --home-dir /nonexistent --shell /usr/sbin/nologin wormtamer-review \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && install -d -m 0700 -o root -g root /var/lib/wormtamer \
    && install -d -m 0711 -o root -g root /var/lib/wormtamer-reviews \
    && install -d -m 0700 -o root -g root /etc/wormtamer

COPY --from=build /out/wormtamer /usr/local/bin/wormtamer

EXPOSE 8080
STOPSIGNAL SIGTERM

USER root
ENTRYPOINT ["/usr/local/bin/wormtamer"]
CMD ["-config", "/etc/wormtamer/config.json"]
