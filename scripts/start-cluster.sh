#!/bin/bash
# Start the compute-nmonit cluster: control plane on host, agent on Jetson

set -e

# --- Configuration ---
# Override these via environment variables or edit inline.
HOST_IP="${HOST_IP:-192.168.1.6}"
JETSON_IP="${JETSON_IP:-192.168.1.150}"
JETSON_USER="${JETSON_USER:-jetson}"
PROJECT_DIR="${PROJECT_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
CP_LOG="${CP_LOG:-/tmp/nmonit/control-plane.log}"
AGENT_LOG="${AGENT_LOG:-/tmp/nmonit/agent.log}"
AGENT_TOKEN="${AGENT_TOKEN:-}"
API_KEY="${API_KEY:-}"

mkdir -p /tmp/nmonit

# --- Cleanup ---
echo "Cleaning up old processes..."
pkill -f "control-plane" 2>/dev/null || true
ssh -o ConnectTimeout=5 "${JETSON_USER}@${JETSON_IP}" "pkill -f compute-agent" 2>/dev/null || true
sleep 1

# --- Build flags ---
CTRL_EXTRA=""
if [ -n "$AGENT_TOKEN" ]; then
    CTRL_EXTRA="$CTRL_EXTRA --agent-token $AGENT_TOKEN"
fi
if [ -n "$API_KEY" ]; then
    CTRL_EXTRA="$CTRL_EXTRA --api-key $API_KEY"
fi

# --- Start control plane ---
echo "Starting control plane on host..."
cd "$PROJECT_DIR"
./bin/control-plane --bootstrap --http-addr ":8080" --grpc-addr ":9000" --log-level info $CTRL_EXTRA > "$CP_LOG" 2>&1 &
CP_PID=$!
echo "Control plane PID: $CP_PID"

# Wait for it to be ready
for i in $(seq 1 10); do
    if curl -s http://localhost:8080/health > /dev/null 2>&1; then
        echo "Control plane is ready"
        break
    fi
    sleep 1
done

# --- Start agent on Jetson ---
AGENT_EXTRA=""
if [ -n "$AGENT_TOKEN" ]; then
    AGENT_EXTRA="$AGENT_EXTRA --agent-token $AGENT_TOKEN"
fi

echo "Starting compute agent on Jetson..."
ssh -o ConnectTimeout=5 "${JETSON_USER}@${JETSON_IP}" \
  "source ~/.cargo/env 2>/dev/null || true; \
   mkdir -p /tmp/nmonit; \
   nohup ${PROJECT_DIR}/target/release/compute-agent \
     --control-plane http://${HOST_IP}:9000 \
     --log-level info \
     ${AGENT_EXTRA} \
     > ${AGENT_LOG} 2>&1 &"
echo "Agent started on Jetson"

# Wait for registration
sleep 8

echo ""
echo "=== CONTROL PLANE LOG ==="
cat "$CP_LOG"

echo ""
echo "=== JETSON AGENT LOG ==="
ssh "${JETSON_USER}@${JETSON_IP}" "cat $AGENT_LOG" 2>/dev/null || echo "(could not read agent log)"

echo ""
echo "=== CLUSTER STATUS ==="
curl -s http://localhost:8080/api/v1/nodes | python3 -c "
import sys, json
nodes = json.load(sys.stdin)
print(f'Registered nodes: {len(nodes)}')
for n in nodes:
    c = n.get('cpu', {})
    m = n.get('memory', {})
    g = n.get('gpus', [])
    print(f\"  {n['node_id']} ({n.get('hostname', '?')}): {c.get('physical_cores', '?')} cores ({c.get('architecture', '?')}), {m.get('total_gb', 0):.1f} GB RAM, {len(g)} GPUs\")
    print(f\"    Last heartbeat: {n.get('last_beat', '?')}\")
"
