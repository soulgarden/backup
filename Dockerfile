FROM alpine:3.20.1

RUN apk add --update --no-cache postgresql-client s3cmd gnupg

COPY ./docker-entrypoint.sh /docker-entrypoint.sh
COPY .s3cfg /root/.s3cfg

RUN chmod +x /docker-entrypoint.sh

ENTRYPOINT ["/docker-entrypoint.sh"]
