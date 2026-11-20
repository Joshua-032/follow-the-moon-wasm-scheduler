
set -e
echo "[1/3] Starting Diurnal Carbon & Spot Telemetry Simulator (:8080)..."
fuser -k 8080/tcp 2>/dev/null || true
(cd telemetry && go run main.go > /tmp/telemetry.log 2>&1) &
TELEM_PID=$!
sleep 2

echo "[2/3] Starting Follow-the-Moon Web Control Plane (:9090)..."
fuser -k 9090/tcp 2>/dev/null || true
(cd dashboard && go run main.go > /tmp/dashboard.log 2>&1) &
DASH_PID=$!

echo "$TELEM_PID $DASH_PID" > /tmp/cloud-wasm-lab.pids
echo "[3/3] Stack running! Open http://localhost:9090 in your browser."
echo "      Run ./stop.sh when finished."
