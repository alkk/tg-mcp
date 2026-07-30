FROM ghcr.io/alkk/baseimage/builder-go:latest AS builder

ARG REVISION=unknown

WORKDIR /build
COPY . .
RUN go build -mod=vendor -ldflags "-X main.revision=${REVISION} -s -w" -o /build/tg-mcp ./cmd/tg-mcp

FROM ghcr.io/alkk/baseimage:latest

ENV DATA_DIR=/srv/data \
    CHATS_FILE=/srv/chats.yml \
    LISTEN=:8080

COPY --chown=root:root --chmod=755 init.sh /srv/init.sh
COPY --from=builder --chown=app:app /build/tg-mcp /srv/tg-mcp

EXPOSE 8080
CMD ["/srv/tg-mcp"]
