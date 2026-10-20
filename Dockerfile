FROM alpine:3.24.2
ARG POSTGRES_MAJOR=16
LABEL org.opencontainers.image.source="https://github.com/soulgarden/backup"
RUN apk add --no-cache ca-certificates python3 py3-pip "postgresql${POSTGRES_MAJOR}-client" \
    && addgroup -S -g 10001 backup \
    && adduser -S -D -u 10001 -G backup -h /home/backup backup \
    && mkdir /work && chown 10001:10001 /work \
    && python3 -m venv /opt/venv
COPY requirements.txt /opt/backup/requirements.txt
RUN /opt/venv/bin/pip install --no-cache-dir -r /opt/backup/requirements.txt
COPY backup /opt/backup/backup
COPY VERSION /opt/backup/VERSION
ENV PATH="/opt/venv/bin:$PATH" PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /opt/backup
USER 10001:10001
ENTRYPOINT ["python", "-m", "backup.runner"]
