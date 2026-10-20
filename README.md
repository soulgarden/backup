# PostgreSQL backup

Creates a compressed SQL dump of the `public` schema and uploads it to
S3-compatible storage at `s3://BUCKET/PGDATABASE/<timestamp>.sql.gz`.

## Local use

You need Docker and Make. Copy the example configuration files:

```sh
cp .env.example .env
cp .s3cfg.example .s3cfg.local
chmod 600 .env .s3cfg.local
```

Set the PostgreSQL connection and bucket in `.env`, and the S3 credentials
and endpoints in `.s3cfg.local`. Then build the image and run a backup:

```sh
make build
make run
```

The default PostgreSQL client version is 16. To match a different server
version, use, for example, `make build POSTGRES_MAJOR=17`.

Use `make push` to publish the image and `make lint` to check shell syntax
and the Helm chart (requires Helm).

## Kubernetes

The chart is in `helm/backup`. Set the database connection, bucket and schedule
in your own values file. Reference credentials with `pg.existingSecret`
(key `PGPASSWORD`) and `s3.existingSecret` (file `.s3cfg`).

```sh
helm upgrade --install backup ./helm/backup -f /path/to/values.yaml
```

By default, backups run on schedule `30 23 */3 * *` on nodes labeled
`shared=true`. Change these settings with `schedule` and `nodeSelector`.
The S3 configuration path inside the container is `/home/backup/.s3cfg`.
