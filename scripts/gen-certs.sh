#!/usr/bin/env bash
# Generate self-signed TLS certificates for development.
# Usage: ./scripts/gen-certs.sh [output_dir]
# Default output: deploy/certs/

set -euo pipefail

OUT_DIR="${1:-deploy/certs}"
mkdir -p "$OUT_DIR"

echo "=== Generating certificates in $OUT_DIR ==="

# --- Certificate Authority ---
openssl genrsa -out "$OUT_DIR/ca.key" 4096 2>/dev/null
openssl req -new -x509 -key "$OUT_DIR/ca.key" -sha256 -days 3650 \
  -out "$OUT_DIR/ca.crt" \
  -subj "/CN=nmonit-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
echo "  [OK] CA certificate"

# --- Server certificate (control-plane) ---
openssl genrsa -out "$OUT_DIR/server.key" 4096 2>/dev/null
openssl req -new -key "$OUT_DIR/server.key" \
  -out "$OUT_DIR/server.csr" \
  -subj "/CN=control-plane" 2>/dev/null
cat > "$OUT_DIR/server.ext" <<'EOF'
[server_cert]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:control-plane,DNS:localhost,IP:127.0.0.1,IP:::1
EOF
openssl x509 -req -in "$OUT_DIR/server.csr" \
  -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
  -extfile "$OUT_DIR/server.ext" -extensions server_cert \
  -out "$OUT_DIR/server.crt" -days 365 -sha256 2>/dev/null
rm -f "$OUT_DIR/server.csr" "$OUT_DIR/server.ext"
echo "  [OK] Server certificate"

# --- Agent certificate (for mTLS) ---
openssl genrsa -out "$OUT_DIR/agent.key" 4096 2>/dev/null
openssl req -new -key "$OUT_DIR/agent.key" \
  -out "$OUT_DIR/agent.csr" \
  -subj "/CN=compute-agent" 2>/dev/null
cat > "$OUT_DIR/agent.ext" <<'EOF'
[agent_cert]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = clientAuth
subjectAltName = DNS:compute-agent
EOF
openssl x509 -req -in "$OUT_DIR/agent.csr" \
  -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
  -extfile "$OUT_DIR/agent.ext" -extensions agent_cert \
  -out "$OUT_DIR/agent.crt" -days 365 -sha256 2>/dev/null
rm -f "$OUT_DIR/agent.csr" "$OUT_DIR/agent.ext"
echo "  [OK] Agent certificate"

# Verify both leaf certificates chain to the generated CA.
openssl verify -CAfile "$OUT_DIR/ca.crt" "$OUT_DIR/server.crt" >/dev/null
openssl verify -CAfile "$OUT_DIR/ca.crt" "$OUT_DIR/agent.crt" >/dev/null

echo ""
echo "=== Certificates generated ==="
echo "  CA:       $OUT_DIR/ca.crt"
echo "  Server:   $OUT_DIR/server.crt  +  $OUT_DIR/server.key"
echo "  Agent:    $OUT_DIR/agent.crt   +  $OUT_DIR/agent.key"
echo ""
echo "Control-plane flags:"
echo "  --tls-cert=$OUT_DIR/server.crt --tls-key=$OUT_DIR/server.key --tls-ca-cert=$OUT_DIR/ca.crt"
echo ""
echo "Agent flags:"
echo "  --tls-ca-cert=$OUT_DIR/ca.crt --tls-cert=$OUT_DIR/agent.crt --tls-key=$OUT_DIR/agent.key"
