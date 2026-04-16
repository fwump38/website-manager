# website-manager

A Golang-based site manager for Caddy and Cloudflare, designed for self-hosted environments (e.g. Unraid NAS).

It watches a mounted sites directory, manages site enable/disable state, regenerates and reloads Caddy's configuration, and reconciles Cloudflare DNS records and Tunnel ingress rules — all from a single web dashboard.

## How it works

Each subdirectory under the configured sites path is treated as a site whose name is the hostname to serve (e.g. `blog.example.com`). The service:

1. **Discovers** site directories at startup and via filesystem watch.
2. **Tracks state** in `enabled.json` — which sites are enabled and whether their DNS has been provisioned.
3. **Generates** a Caddyfile for all enabled sites and reloads Caddy via its admin API.
4. **Reconciles** Cloudflare DNS (CNAME records pointing at the Cloudflare Tunnel) and Tunnel ingress rules for enabled sites, cleaning up rules for sites that are disabled.
5. **Exposes a dashboard** on port `8080` to view, create, enable, and disable sites.

## Quick start

1. Create a `.env` file in the project root with your Cloudflare values:

```env
CF_API_TOKEN=your_cloudflare_api_token
CF_ACCOUNT_ID=your_account_id
CF_TUNNEL_ID=your_tunnel_uuid
CF_ENABLE_WWW_REDIRECT=false
# CADDY_SERVICE_URL=http://192.168.1.2:80  # override if Caddy is not in the same Docker network
```

2. Run with Docker Compose:

```bash
docker compose up -d
```

3. Open the dashboard at `http://<host>:8080`.

From the dashboard you can create new sites (choosing a domain, an optional subdomain, and a starter template), enable or disable existing ones, and see live DNS status for each enabled site.

> Site directories can also be created manually. Any directory added to or removed from the sites folder is picked up automatically by the filesystem watcher.

## Configuration

All configuration is provided via environment variables.

| Variable | Default | Description |
|---|---|---|
| `SITES_DIR` | `/sites` | Path to the directory containing site folders. |
| `STATE_FILE` | `$SITES_DIR/enabled.json` | Path to the JSON state file. |
| `CADDYFILE_OUTPUT` | `/etc/caddy/Caddyfile` | Path where the generated Caddyfile is written. |
| `CADDY_ADMIN_URL` | `http://caddy:2019` | Caddy admin API URL used for config reload. |
| `CADDY_SERVICE_URL` | `http://caddy:80` | Caddy service URL used as the Cloudflare Tunnel backend. |
| `DASHBOARD_PORT` | `8080` | Port the dashboard listens on. |
| `CF_API_TOKEN` | — | Cloudflare API token (see below). |
| `CF_ACCOUNT_ID` | — | Cloudflare account ID. |
| `CF_TUNNEL_ID` | — | UUID of the Cloudflare Tunnel to manage. |
| `CF_TUNNEL_HOSTNAME` | `<tunnel_id>.cfargotunnel.com` | Tunnel hostname used as the CNAME target. Auto-derived from `CF_TUNNEL_ID` if not set. |
| `CF_ENABLE_WWW_REDIRECT` | `false` | When `true`, adds a `www.<domain>` Tunnel ingress rule for apex domain sites. |
| `PUID` | `99` | User ID applied to created site files and `enabled.json`. Defaults to the Unraid `nobody` UID. |
| `PGID` | `100` | Group ID applied to created site files and `enabled.json`. Defaults to the Unraid `users` GID. |

Cloudflare reconciliation is skipped if `CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_TUNNEL_ID`, or zone configuration is missing, so the service can run in Caddy-only mode without any Cloudflare credentials.

## Site templates

When creating a site from the dashboard, a starter template is copied into the new site directory. The following templates are available:

| Template | Description |
|---|---|
| `static-html` | A minimal static HTML site with a CSS stylesheet and placeholder `index.html`. Extra empty directories (`assets/js`, `assets/images`) are created automatically. |

File ownership is set using the configured `PUID`/`PGID` values (default `99`/`100`, matching the Unraid `nobody:users` owner).

## Optional www redirect

Set `CF_ENABLE_WWW_REDIRECT=true` to automatically add a `www.<domain>` ingress rule in the Cloudflare Tunnel for any enabled apex-domain site. This affects Tunnel ingress only — no extra DNS record or Caddy vhost is created. If you have a dedicated `www.<domain>` directory in the sites folder it is treated as an independent site and is unaffected by this setting.

## Cloudflare API token

Create a **Custom Token** with the following permissions:

| Scope | Permission |
|---|---|
| Account → Cloudflare Tunnel | Edit |
| Zone → DNS | Edit |
| Zone → Zone | Read |

> **Note:** In the Cloudflare dashboard, tunnels are listed under the *Zero Trust* product but the API permission is still labelled **Cloudflare Tunnel**, not Zero Trust.

Steps: Cloudflare dashboard → My Profile → API Tokens → Create Token → Custom Token → add the three permissions above → under Zone Resources set *Include → Specific zone → [your domain]*.

For multi-domain setups, include every domain's zone under Zone Resources and list all of them in `CF_ZONE_MAP`. The token must have `DNS Edit` permission on each zone.

## REST API

The dashboard is backed by a small REST API:

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/sites` | List all discovered sites and their state. |
| `POST` | `/api/sites` | Create a new site from a template. Body: `{"subdomain":"blog","domain":"example.com","template":"static-html"}` |
| `PATCH` | `/api/sites/:name` | Enable or disable a site. Body: `{"enabled":true}` |
| `DELETE` | `/api/sites/:name` | Delete a disabled site and permanently remove its folder and all contents. |
| `GET` | `/api/domains` | List domains available from the configured zone map. |
| `GET` | `/api/dns-check?site=:name` | Check whether a site's DNS is resolving via Cloudflare's `1.1.1.1` resolver. |
| `GET` | `/health` | Health check — returns `200 ok`. |

## Publishing container images

Images are published to GitHub Container Registry on every SemVer tag push.

```bash
git tag v1.2.0 && git push origin v1.2.0
# → builds and pushes ghcr.io/<owner>/<repo>:v1.2.0
```

## Development

```bash
go test ./...
```

## Project structure

| File / Directory | Description |
|---|---|
| `main.go` | Application bootstrap, config loading, and reconcile loop. |
| `state.go` | Thread-safe `enabled.json` state management. |
| `watcher.go` | Site folder discovery and filesystem watcher (fsnotify). |
| `caddy.go` | Caddyfile template rendering and Caddy admin API reload. |
| `cloudflare.go` | Cloudflare DNS record and Tunnel ingress reconciliation. |
| `dashboard.go` | Web dashboard HTTP handlers and REST API. |
| `sitetemplates.go` | Embedded site template scaffolding (`go:embed`). |
| `templates/` | Dashboard HTML and Caddyfile Go template. |
| `site-templates/` | Embedded starter templates copied when a new site is created. |
| `Dockerfile` | Multi-stage build producing a minimal Alpine image. |
| `docker-compose.yml` | Reference deployment with Caddy and site-manager services. |
