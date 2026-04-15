# website-manager
A Golang-based site manager for Caddy and Cloudflare.

This service watches a mounted sites directory, tracks site enable/disable state in `enabled.json`, regenerates a single-port Caddyfile, reloads Caddy, and syncs Cloudflare DNS and Tunnel ingress rules.

## Quick start

1. Create a `.env` file in the project root with your Cloudflare values:

```env
CF_API_TOKEN=your_cloudflare_api_token
CF_ACCOUNT_ID=your_account_id
CF_TUNNEL_ID=your_tunnel_uuid
CF_ZONE_MAP=example.com=zone_id_for_example,example2=zone_id_for_example2,example3.com=zone_id_for_example3
CF_TUNNEL_HOSTNAME=<tunnel-id>.cfargotunnel.com
```

If you are using a single zone, you may instead set:

```env
CF_ZONE_ID=your_zone_id_for_example_com
CF_ZONE_DOMAIN=example.com
```

2. Run with Docker Compose:

```bash
docker compose up -d
```

3. Visit the dashboard on port `8080` to view discovered sites and toggle them on/off.

> Each directory under the mounted sites share should be named with the full hostname you want to serve, for example `example.com`, `blog.example2`, or `shop.example3.com`.

## Cloudflare API token

Create a restricted Cloudflare API token with the minimum permissions required for this service:

Permission groups (create a Custom Token):

- Account: Cloudflare Tunnel: Edit

- Zone: DNS: Edit

- Zone: Zone: Read

The Zone: Read permission is required because the service needs to look up zone metadata when making DNS API calls.

One important gotcha: Cloudflare Tunnel is not the same as Zero Trust in the permissions UI — tunnels are now marketed as "Zero Trust tunnels" but the actual permission group you want is still labeled Cloudflare Tunnel, not Zero Trust.

To create it:

Cloudflare dashboard → My Profile → API Tokens → Create Token → Custom Token

Add the 3 permissions above

Under Zone Resources: Include → Specific zone → [your domain]

No IP filtering needed for a home NAS (unless you want to lock it to your WAN IP)

If you manage multiple domains, include each domain's zone in `CF_ZONE_MAP`. The token must have `DNS Edit` permissions on every zone listed in `CF_ZONE_MAP`.

## Publishing container images

This repository is configured to publish container images to GitHub Container Registry whenever a new SemVer tag is pushed.

- Push a tag like `v1.2.0`
- GitHub Actions will build the image and push `ghcr.io/<owner>/<repo>:v1.2.0`

## Development

```bash
go test ./...
```

## Files

- `main.go` — application bootstrap and reconcile loop
- `state.go` — `enabled.json` state management
- `watcher.go` — site folder discovery and filesystem watcher
- `caddy.go` — Caddyfile generation and reload
- `cloudflare.go` — Cloudflare DNS and Tunnel ingress reconciliation
- `dashboard.go` — web dashboard and REST API
- `templates/` — dashboard UI and Caddyfile template
- `Dockerfile` / `docker-compose.yml` — container deployment
