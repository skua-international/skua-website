# skua-site

Unit page for Skua International — Arma 3 PMC.

## Stack

- **Server**: Go — serves static files, pulls certification docs from GitHub, renders markdown to HTML
- **Frontend**: Vanilla HTML/CSS/JS, IBM Plex Mono/Sans, dark theme
- **Infra**: Docker + nginx reverse proxy

## How Docs Work

Certifications are pulled from [`skua-international/certifications`](https://github.com/skua-international/certifications) (master branch). The server fetches all root-level `.md` files on startup and caches rendered HTML in memory. `README.md` is excluded.

Updates are triggered by a GitHub Action in the certifications repo that hits `POST /api/docs/refresh` on push to master. No polling.

### Setup

1. Set `REFRESH_SECRET` on the server (env var)
2. In the **certifications** repo, add two repository secrets:
   - `REFRESH_URL` — `https://skua.international/api/docs/refresh`
   - `REFRESH_SECRET` — same value as the server env var
3. Copy `.github/workflows/notify-site.yml` into the certifications repo

### Adding a Certification

Push a `.md` file to the root of the certifications repo. The action fires, the site updates.

## Local Development

```bash
# Direct (requires Go 1.22+)
go run ./cmd/server

# Docker
docker compose up --build
```

Site at `http://localhost:3000` (direct) or `http://localhost:8080` (nginx).

## Deployment

1. DNS: point `skua.international` to your server
2. TLS: place certs in `nginx/certs/` (fullchain.pem, privkey.pem)
3. Set env vars in `docker-compose.yml`
4. `docker compose up -d`

## Structure

```
├── cmd/server/main.go          # Go server
├── static/
│   ├── css/main.css            # Styles
│   ├── js/app.ts               # TypeScript source
│   ├── js/app.js               # Compiled JS (shipped)
│   └── img/logo.png            # Unit logo
├── templates/index.html        # SPA shell
├── nginx/nginx.conf            # Nginx reverse proxy
├── .github/workflows/
│   └── notify-site.yml         # Goes in the certifications repo
├── Dockerfile
├── docker-compose.yml
└── README.md
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `ADDR` | `:8080` | Listen address |
| `GITHUB_TOKEN` | — | **Required.** PAT with read access to `skua-international/certifications`. Fine-grained token with Contents read-only on that repo is sufficient. |
| `REFRESH_SECRET` | — | Shared secret for `POST /api/docs/refresh`. If unset, endpoint is open. |
