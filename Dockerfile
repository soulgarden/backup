FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=$(cat VERSION)" -o /out/backup ./cmd/backup
RUN CGO_ENABLED=0 go test -c -o /out/backup-test ./internal/backup

FROM alpine:3.24.2 AS runtime
ARG POSTGRES_MAJOR=16
LABEL org.opencontainers.image.source="https://github.com/soulgarden/backup"
RUN apk add --no-cache ca-certificates "postgresql${POSTGRES_MAJOR}-client" \
    && addgroup -S -g 10001 backup \
    && adduser -S -D -u 10001 -G backup -h /home/backup backup \
    && mkdir /work && chown 10001:10001 /work
COPY --from=build /out/backup /usr/local/bin/backup
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/backup"]

FROM runtime AS integration
COPY --from=build /out/backup-test /usr/local/bin/backup-test
COPY config.example.json /fixtures/config.example.json
ENV CONFIG_FIXTURE=/fixtures/config.example.json TMPDIR=/work

FROM runtime AS final
