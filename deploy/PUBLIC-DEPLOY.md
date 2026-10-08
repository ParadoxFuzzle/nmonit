# Public deployment — nmonit.jerithai.com

Step-by-step to expose the control plane at `nmonit.jerithai.com` so outside
agents can join.

## Prerequisites (owner actions)

1. **DNS**: an A record `nmonit.jerithai.com → <your public IP>`.
   Let's Encrypt HTTP-01 needs ports 80 + 443 reachable from the internet.
2. **Router**: forward 80/tcp and 443/tcp to this host.
   (Alternative: Cloudflare proxied + origin cert, or run Caddy on a VPS.)
3. Control plane already runs TLS 1.3 on :9000 (gRPC) and :8080 (REST) with
   the identity layer enabled (`--pg-dsn` set in the systemd unit).

## 1. Install Caddy

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy.list
sudo apt update && sudo apt install caddy
```

## 2. Caddyfile

`/etc/caddy/Caddyfile`:

```caddy
# Public onboarding UI + REST (proxies to the control plane's own TLS).
# Caddy terminates Let's Encrypt TLS and re-encrypts to :8080.
nmonit.jerithai.com {
    reverse_proxy https://127.0.0.1:8080 {
        transport http {
            tls_insecure_skip_verify   # self-signed origin; change to CA pin when rotating to a public origin cert
        }
    }
    # Basic rate limiting is handled by the identity layer (per-token);
    # add a WAF/CDN in front for volumetric DoS.
}

# gRPC for agents: HTTP/2 + TLS passthrough is NOT possible with a single
# cert unless Caddy owns it. Simplest: L4 passthrough on 9005 -> 9000.
# Agents connect with --control-plane https://nmonit.jerithai.com:9005
# and verify against the ORIGINAL self-signed CA (deploy/certs/ca.crt),
# or we switch the control plane to use the Let's Encrypt cert (step 4).
```

## 3. Start

```bash
sudo systemctl reload caddy
```

Caddy obtains the Let's Encrypt certificate automatically on first request.

## 4. (Recommended) Give the control plane the public cert

So agents can verify `nmonit.jerithai.com` without shipping your private CA:

```bash
# After Caddy obtains the cert:
sudo cp /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/nmonit.jerithai.com/nmonit.jerithai.com.crt \
     ~/Documents/Projects/nmonit/deploy/certs/public.crt
sudo cp /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/nmonit.jerithai.com/nmonit.jerithai.com.key \
     ~/Documents/Projects/nmonit/deploy/certs/public.key
sudo chown noodly:noodly ~/Documents/Projects/nmonit/deploy/certs/public.*
# Point --tls-cert/--tls-key at public.crt/public.key (cert hot-reload picks
# up the change within --tls-reload-interval), and have Caddy L4-proxy :9005
# straight through to :9000.
```

Then a contributor joins with:

```bash
curl -X POST https://nmonit.jerithai.com/join -d '{"email":"me@example.com","role":"provider"}'
# ...operator approves via nmonitctl approve <id>...
./compute-agent --control-plane https://nmonit.jerithai.com:9005 \
  --agent-token nmt_...            # system CA trust, no --tls-ca-cert needed
```

## 5. Contributor instructions (publish these)

```
1. Submit the join form:      https://nmonit.jerithai.com/join
2. Wait for approval email/DM with your token (nmt_...)
3. Download compute-agent, then:
   ./compute-agent --control-plane https://nmonit.jerithai.com:9005 \
     --agent-token nmt_... --tls-ca-cert ca.crt
```

## 6. Operator cheat sheet

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
bootstrap tokens bypass limits (trusted path).
