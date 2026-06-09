#!/bin/bash
# Start the compute-nmonit cluster: control plane on host, agent on Jetson

set -e

HOST_IP="192.168.1.6"
JETSON_IP="192.168.1.150"
CP_LOG="/tmp/nmonit/control-plane.log"
AGENT_LOG="/tmp/nmonit/agent.log"

mkdir -p /tmp/nmonit

# Kill any existing processes
echo "Cleaning up old processes..."
pkill -f "control-plane" 2>/dev/null || true
ssh -o ConnectTimeout=5 jetson "pkill -f compute-agent" 2>/dev/null || true
sleep 1

# Start control plane on host
echo "Starting control plane on host..."
cd /home/noodly/Desktop/compute-nmonit
./bin/control-plane --bootstrap --http-addr ":8080" --grpc-addr ":9000" --log-level info > "$CP_LOG" 2>&1 &
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

# Start agent on Jetson
echo "Starting compute agent on Jetson..."
ssh -o ConnectTimeout=5 jetson "source ~/.cargo/env; mkdir -p /tmp/nmonit; nohup ~/nmonit/target/release/compute-agent --control-plane http://${HOST_IP}:9000 --log-level info > ${AGENT_LOG} 2>&1 &"
echo "Agent started on Jetson"

# Wait for registration
sleep 8

echo ""
echo "=== CONTROL PLANE LOG ==="
cat "$CP_LOG"

echo ""
echo "=== JETSON AGENT LOG ==="
ssh jetson "cat $AGENT_LOG" 2>/dev/null

echo ""
echo "=== CLUSTER STATUS ==="
curl -s http://localhost:8080/api/v1/nodes | python3 -c "
import sys, json
nodes = json.load(sys.stdin)
print(f'Registered nodes: {len(nodes)}')
for nid, n in nodes.items():
    cpu = n['Resources']['cpu']['physical_cores']
    arch = n['Resources']['cpu']['architecture']
    ram_gb = n['Resources']['memory']['total_bytes'] / (1024**3)
    gpus = n['Resources'].get('gpus', [])
    gpu_count = len(gpus) if gpus else 0
    print(f'  {nid}: {cpu} cores ({arch}), {ram_gb:.1f} GB RAM, {gpu_count} GPUs')
    print(f'    Last heartbeat: {n[\"LastBeat\"]}')
"
