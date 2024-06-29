build:
	docker build . -t soulgarden/backup:0.0.2 --platform linux/amd64
	docker push soulgarden/backup:0.0.2

run:
	docker run -it  --rm --name backup -e PGHOST=127.0.0.1 -e PGPORT=5432 -e PGUSER=user -e PGPASSWORD=pass -e PGDATABASE=db -e BUCKET=bucket soulgarden/backup:0.0.2