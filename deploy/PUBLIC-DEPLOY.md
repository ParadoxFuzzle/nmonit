# Public deployment — nmonit.jerithai.com

Step-by-step to expose the control plane at `nmonit.jerithai.com` so outside
agents can join.

> **Current production setup: Cloudflare Tunnel** (sections 1–4). This was
> chosen because the ISP blocks inbound ports 80/443/8080, which rules out
> both Let's Encrypt HTTP-01 and any port-forward approach. The tunnel is
> outbound-only and needs no router configuration.
>
> Historical alternatives are kept at the bottom (Caddy + origin cert,
> sections A/B) for reference if the network situation changes.

## Architecture (current)

```
Internet ──> Cloudflare edge (TLS termination, proxy)
                 │  outbound tunnel (cloudflared, QUIC/http2)
                 ▼
             cloudflared (systemd user unit nmonit-cloudflared.service)
                 │  https://127.0.0.1:8080 (noTLSVerify — self-signed origin)
                 ▼
             control-plane :8080 (REST + /join UI)
             control-plane :9000 (gRPC, mTLS, agents)
```

## 1. Install cloudflared

```bash
curl -fsSL -o /tmp/cloudflared.deb \
  https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb
sudo dpkg -i /tmp/cloudflared.deb
```

## 2. Authenticate and create the tunnel

```bash
cloudflared tunnel login        # opens browser; pick the jerithai.com zone
cloudflared tunnel create nmonit
# Credentials land in ~/.cloudflared/<tunnel-id>.json — keep secret.
```

Current tunnel: `nmonit` → id `c4139072-354a-405b-af03-64aadcf7ae52`.

## 3. Config and DNS

`~/.cloudflared/config.yml`:

```yaml
tunnel: c4139072-354a-405b-af03-64aadcf7ae52
credentials-file: /home/noodly/.cloudflared/c4139072-354a-405b-af03-64aadcf7ae52.json
ingress:
  - hostname: nmonit.jerithai.com
    service: https://127.0.0.1:8080
    originRequest:
      noTLSVerify: true      # control plane uses its self-signed server cert
      connectTimeout: 10s
  - service: http_status:404
```

Route public traffic (overwrites the old A record with a CNAME to the
tunnel — the DNS record must be proxied/orange-clouded, which the CNAME to
`<tunnel-id>.cfargotunnel.com` implies):

```bash
cloudflared tunnel route dns --overwrite-dns nmonit nmonit.jerithai.com
```

## 4. Run as a persistent service

`~/.config/systemd/user/nmonit-cloudflared.service`:

```ini
[Unit]
Description=Cloudflare Tunnel for nmonit (nmonit.jerithai.com)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/bin/cloudflared tunnel run nmonit
Restart=on-failure
RestartSec=5
Environment=TUNNEL_TRANSPORT_PROTOCOL=http2

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now nmonit-cloudflared.service
journalctl --user -u nmonit-cloudflared.service -f   # expect 4x "Registered tunnel connection"
```

## 5. Verify

```bash
curl -s https://nmonit.jerithai.com/health                 # {"status":"ok"}
curl -s -o /dev/null -w '%{http_code}\n' https://nmonit.jerithai.com/join   # 200
```

Verified working (2026-10-08): `/health` → `{"status":"ok"}`, `/join` → 200,
invalid invite POST → 403, all through the Cloudflare edge.

## 6. gRPC for agents (implemented 2026-10-08)

**Decision made:** same tunnel, dedicated hostname.

- Ingress rule added for `grpc-nmonit.jerithai.com` → `https://127.0.0.1:9000`
  with `http2Origin: true` (gRPC requires end-to-end HTTP/2). DNS CNAME routed
  via `cloudflared tunnel route dns --overwrite-dns`.
- The control plane's gRPC server **no longer requires client certificates**
  (the `--tls-ca-cert` mTLS flag was removed from the systemd unit; the
  `--require-tls` flag replaces it). Reason: tunnel-origin connections carry
  no client certs, so mandatory mTLS would break the handshake. Auth is
  enforced at the application layer instead — bootstrap token + per-node
  `nmt_` tokens + per-credential rate limiting (Phase-1 design).
- **Verified end-to-end at the origin:** a gRPC client reaching :9000 through
  the tunnel ingress path gets app-level responses (auth + validation working;
  a malformed Register returns `InvalidArgument`, not a crash).
- **Blocked at the Cloudflare zone level:** proxied gRPC requires the
  `gRPC` toggle ON in the CF dashboard (Network tab). Without it CF returns a
  403 HTML page to gRPC clients. Owner action: CF dashboard →
  jerithai.com → Network → WebSockets/gRPC → enable. Note: a CF error page
  (or Bot Fight Mode) can also produce 403s — if the toggle is already on,
  check Security → Bots.
- Cert trust for contributors: agents connect to
  `https://grpc-nmonit.jerithai.com:443` and verify against **system CA
  trust** — they see Cloudflare's public edge cert, never the origin cert.
  The origin keeps its self-signed `server.crt` (cloudflared uses
  `noTLSVerify` for the last hop). The earlier plan to install a public cert
  on the origin is unnecessary with the tunnel and was dropped.

> Historical note: the old `cf-origin.crt` is a Cloudflare **client**-auth
> cert (EKU: Client Authentication, no SAN) from the Caddy era — it was
> never usable as a server cert for the gRPC listener.

## 7. Contributor instructions (publish these)

```
1. Submit the join form:      https://nmonit.jerithai.com/join
2. Wait for approval with your token (nmt_...)
3. Download compute-agent, then:
   ./compute-agent --control-plane https://grpc-nmonit.jerithai.com:443 \
     --agent-token nmt_...
   (System CA trust — no --tls-ca-cert needed. Requires the operator to
   have enabled the gRPC toggle in the Cloudflare dashboard.)

## 8. Operator cheat sheet

```bash
nmonitctl --api-url https://nmonit.jerithai.com --api-key "$KEY" requests --status pending
nmonitctl --api-url https://nmonit.jerithai.com --api-key "$KEY" approve <id>   # prints nmt_ token once
nmonitctl --api-url https://nmonit.jerithai.com --api-key "$KEY" token revoke <token-id>
nmonitctl --api-url https://nmonit.jerithai.com --api-key "$KEY" audit tail --n 50
```

## Rate limits & quotas

Per-token rate limiting and node-count quotas are enforced by the identity
layer (see `internal/identity`): each `nmt_` token is rate-limited for gRPC
calls, and users have a max-node quota enforced at Register time. Owner
bootstrap tokens bypass limits (trusted path). Cloudflare additionally
absorbs volumetric DoS in front of the tunnel.

---

# Historical alternatives (not in use)

## A. Caddy + Cloudflare origin cert (retired 2026-10-08)

Caddy is **disabled and its site block removed** (config preserved at
`/etc/caddy/Caddyfile.nmonit-retired-20261008`) — the tunnel made it
redundant. It worked locally (`curl --resolve
nmonit.jerithai.com:443:127.0.0.1` → ok) but was unreachable publicly
because the ISP blocks inbound 443. Kept for reference if ports open up or
the host moves behind a VPS.

- Caddyfile site block: `tls /etc/caddy/certs/cf-origin.crt
  /etc/caddy/certs/cf-origin.key`, `reverse_proxy https://127.0.0.1:8080`
  with `tls_insecure_skip_verify`.
- Certs at `/etc/caddy/certs/` (caddy:caddy) and source copies in
  `deploy/certs/cf-origin.{crt,key}` (valid Oct 2026 → Oct 2036).
- Requires CF SSL mode **Full (strict)** and router forward 443 → host.
- Known quirk: `sudo systemctl reload caddy` can hang (state "reloading");
  fix by SIGKILL caddy then start.

## B. Let's Encrypt + Caddy

Requires inbound 80/443 reachable from the internet (blocked by ISP). Steps
omitted — see git history of this file for the original version.
