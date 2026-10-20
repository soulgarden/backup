FROM alpine:3.24.2

ARG POSTGRES_MAJOR=16

LABEL org.opencontainers.image.source="https://github.com/soulgarden/backup"

RUN apk add --no-cache \
        ca-certificates \
        gnupg \
        "postgresql${POSTGRES_MAJOR}-client" \
        s3cmd \
    && addgroup -S -g 10001 backup \
    && adduser -S -D -u 10001 -G backup -h /home/backup backup

ENV HOME=/home/backup
WORKDIR /home/backup

COPY --chown=10001:10001 --chmod=0600 .s3cfg.example /home/backup/.s3cfg
COPY --chmod=0755 docker-entrypoint.sh /docker-entrypoint.sh

USER 10001:10001

ENTRYPOINT ["/docker-entrypoint.sh"]
