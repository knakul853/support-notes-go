# support-notes-go

A small Go + Postgres HTTP API for support notes.

## Endpoints

- `GET /health` -> `200 {"status":"ok"}`
- `POST /register` (json `{"username":"...","email":"...","password":"..."}`) -> `201` created user, `role` defaults to `member`
- `POST /login` (json `{"username":"...","password":"..."}` or `{"email":"...","password":"..."}`) -> `200`, sets a `session` cookie
- `GET /notes` (auth required) -> `200` list of the caller's own notes
- `GET /notes/{id}` (auth required) -> `200` single note owned by the caller, `404` if missing or owned by someone else
- `POST /notes` (auth required, json `{"body":"..."}`) -> `201` created note, owned by the authenticated caller
- `GET /admin/notes` (auth required, `admin` role only) -> `200` all notes across all users, `403` for non-admins

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
curl -X POST localhost:8080/register -d '{"username":"carol","email":"carol@example.com","password":"hunter2!"}'
curl -c cookies.txt -X POST localhost:8080/login -d '{"username":"carol","password":"hunter2!"}'
curl -b cookies.txt -X POST localhost:8080/notes -d '{"body":"hi"}'
curl -b cookies.txt localhost:8080/notes
```
