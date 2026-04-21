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
| `PUID` | `99` | User ID applied to created site files and `enabled.json`. Defaults to the Unraid `nobody` UID. |
| `PGID` | `100` | Group ID applied to created site files and `enabled.json`. Defaults to the Unraid `users` GID. |
| `MAILGUN_API_KEY` | — | Mailgun private API key. Required to enable contact form submission on any site. |
| `MAILGUN_DOMAIN` | — | The Mailgun sending domain (e.g. `mg.example.com`). Must match a verified domain in your Mailgun account. |

Cloudflare reconciliation is skipped if `CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_TUNNEL_ID`, or zone configuration is missing, so the service can run in Caddy-only mode without any Cloudflare credentials.

## Site templates

When creating a site from the dashboard, a starter template is copied into the new site directory. The following templates are available:

| Template | Description |
|---|---|
| `static-html` | A minimal static HTML site with a CSS stylesheet and placeholder `index.html`. Extra empty directories (`assets/js`, `assets/images`) are created automatically. |

File ownership is set using the configured `PUID`/`PGID` values (default `99`/`100`, matching the Unraid `nobody:users` owner).

## Contact form

The site-manager exposes a `POST /api/contact` endpoint that accepts a JSON form submission from a site visitor and sends an email to the site owner via Mailgun. The Caddyfile template automatically reverse-proxies `/api/contact` to the site-manager and injects an `X-Site-Name` header so the handler knows which site the submission came from.

### Enabling the contact form for a site

Contact form configuration is managed entirely through the dashboard — no manual file editing required.

1. Open the dashboard and click **Edit** on the site you want to configure.
2. Toggle **Contact form** on.
3. Fill in **Deliver to** (your inbox address — e.g. `you@example.com`).
4. Click **Save**.

The from address is derived automatically from `MAILGUN_DOMAIN` (e.g. `contact-form@mg.example.com`), so no per-site sender configuration is required. Emails include a `Reply-To` header set to the submitter's address, so you can reply directly from your inbox.

The settings are stored in `{sitesDir}/{siteName}/site.json` and written atomically by the site-manager. Caddy's config is regenerated automatically so `/api/contact` is only proxied for sites that have the contact form enabled.

### Mailgun DNS setup

Mailgun uses **TXT records** (SPF/DKIM) for sending verification, not MX records. Your existing MX records (e.g. for SimpleLogin aliases) are unaffected.

1. In the Mailgun dashboard, add your sending domain (e.g. `mg.yourdomain.com`) and follow the DNS verification steps — typically two TXT records and a CNAME.
2. Add `MAILGUN_API_KEY` and `MAILGUN_DOMAIN` to your `.env` file.

### Request format

```json
{
  "name":            "Jane Smith",
  "email":           "jane@example.com",
  "engagement_type": "Photography Commission",
  "message":         "Hi, I'd like to commission...",
  "website":         ""  // honeypot — must be empty; bots that fill it are silently dropped
}
```

### Security measures

- **Origin validation** — the `Origin` header must match the site's hostname.
- **Rate limiting** — max 3 submissions per IP per 10-minute window.
- **Honeypot field** — a hidden `website` field is checked server-side; filled submissions are silently accepted without sending.
- **Body size cap** — request body is limited to 16 KB.
- **Field length limits** — name ≤ 200 chars, email ≤ 320, engagement type ≤ 200, message ≤ 5,000.
- **`X-Site-Name` trust boundary** — the header is injected by Caddy inside the Docker network; it is never accepted from external requests directly.

## www redirect

For apex-domain sites, you can enable the **Redirect www → apex** toggle in the site's Edit modal. When enabled, the site-manager:

- Adds a `www.<domain>` Caddy vhost that permanently redirects to the apex domain.
- Adds a `www.<domain>` DNS CNAME record in Cloudflare pointing at the Tunnel.
- Adds a `www.<domain>` Tunnel ingress rule.

The toggle is hidden for subdomain sites (e.g. `blog.example.com`). If a dedicated `www.<domain>` directory already exists in the sites folder it is treated as an independent site and the toggle has no effect.

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
| `PATCH` | `/api/sites/:name` | Update a site's settings (enabled state, contact form config, www redirect). Body: `{"enabled":true,"contact_enabled":true,"contact_to":"you@example.com","www_redirect":false}` |
| `DELETE` | `/api/sites/:name` | Delete a site and permanently remove its folder. If the site is currently enabled it is disabled first. |
| `GET` | `/api/domains` | List domains available from the configured zone map. |
| `GET` | `/api/dns-check?site=:name` | Check whether a site's DNS is resolving via Cloudflare's `1.1.1.1` resolver. |
| `POST` | `/api/contact` | Accept a contact form submission and send it via Mailgun to the site owner. Requires the contact form to be enabled for the site via the dashboard. |
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
| `contact.go` | Contact form handler — rate limiting, validation, honeypot, and Mailgun delivery. |
| `siteconfig.go` | Per-site `site.json` config (contact form settings and www redirect toggle). |
| `sitetemplates.go` | Embedded site template scaffolding (`go:embed`). |
| `templates/` | Dashboard HTML and Caddyfile Go template. |
| `site-templates/` | Embedded starter templates copied when a new site is created. |
| `Dockerfile` | Multi-stage build producing a minimal Alpine image. |
| `docker-compose.yml` | Reference deployment with Caddy and site-manager services. |
