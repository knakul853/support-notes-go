# support-notes-go

A small Go + Postgres HTTP API for support notes.

## Endpoints

- `GET /health` -> `200 {"status":"ok"}`
- `GET /notes` -> `200` list of notes
- `GET /notes/{id}` -> `200` single note, `404` if missing
- `POST /notes` (json `{"owner":"...","body":"..."}`) -> `201` created note

## Env vars

- `DATABASE_URL` (required) - Postgres DSN, e.g. `postgres://assay:assay@postgres:5432/assay`
- `PORT` (optional, default `8080`)

## Run locally

```bash
docker network create support-notes-net || true

docker run -d --name postgres --network support-notes-net \
  -e POSTGRES_USER=assay -e POSTGRES_PASSWORD=assay -e POSTGRES_DB=assay \
  postgres:16-alpine

docker build -t support-notes-go .

docker run --rm -p 8080:8080 --network support-notes-net \
  -e DATABASE_URL=postgres://assay:assay@postgres:5432/assay \
  support-notes-go
```

Then:

```bash
curl localhost:8080/health
curl localhost:8080/notes
curl -X POST localhost:8080/notes -d '{"owner":"carol","body":"hi"}'
```
