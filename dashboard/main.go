package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type RegionMetrics struct {
	Region          string  `json:"region"`
	LocalHour       float64 `json:"local_hour"`
	CarbonIntensity float64 `json:"carbon_gco2_kwh"`
	SpotPriceUSD    float64 `json:"spot_price_usd"`
	EvictionRisk    float64 `json:"eviction_risk"`
	NetworkRTTMs    float64 `json:"network_rtt_ms"`
}

type TelemetrySnapshot struct {
	SimUTC     string                   `json:"sim_utc"`
	SimHourUTC float64                  `json:"sim_hour_utc"`
	Regions    map[string]RegionMetrics `json:"regions"`
}

type PolicyWeights struct {
	Alpha      float64 `json:"alpha"`
	Beta       float64 `json:"beta"`
	Gamma      float64 `json:"gamma"`
	Hysteresis float64 `json:"hysteresis"`
}

type MigrationEvent struct {
	ID        int    `json:"id"`
	Timestamp string `json:"timestamp"`
	SimUTC    string `json:"sim_utc"`
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason"`
	CutoverMs int64  `json:"cutover_ms"`
}

type DashboardState struct {
	mu              sync.RWMutex
	ActiveRegion    string                   `json:"active_region"`
	SimUTC          string                   `json:"sim_utc"`
	Regions         map[string]RegionMetrics `json:"regions"`
	Scores          map[string]float64       `json:"scores"`
	Weights         PolicyWeights            `json:"weights"`
	Migrations      []MigrationEvent         `json:"migrations"`
	TotalCO2SavedG  float64                  `json:"total_co2_saved_g"`
	TotalCostSaved  float64                  `json:"total_cost_saved_usd"`
	ChaosRegion     string                   `json:"chaos_region"`
	CheckpointSeq   int                      `json:"checkpoint_seq"`
	IsMigrating     bool                     `json:"is_migrating"`
}

var state = &DashboardState{
	ActiveRegion: "us-east",
	Regions:      make(map[string]RegionMetrics),
	Scores:       make(map[string]float64),
	Weights: PolicyWeights{
		Alpha:      0.45,
		Beta:       0.35,
		Gamma:      0.20,
		Hysteresis: 0.12,
	},
	Migrations:    []MigrationEvent{},
	CheckpointSeq: 1,
}

func computeUtility(m RegionMetrics, w PolicyWeights) float64 {
	normCarbon := m.CarbonIntensity / 500.0
	normSpot := ((m.SpotPriceUSD / 0.10) + m.EvictionRisk) / 2.0
	normLatency := m.NetworkRTTMs / 200.0
	return (w.Alpha * normCarbon) + (w.Beta * normSpot) + (w.Gamma * normLatency)
}

func kubectlApply(context, manifest string) error {
	cmd := exec.Command("kubectl", "--context", context, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return nil
}

func deployWasmPod(region string, seq int, from string) (int64, error) {
	start := time.Now()
	ctx := "kind-" + region

	cm := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: wasm-checkpoint-state
data:
  checkpoint_seq: "%d"
  migrated_from: "%s"
  timestamp: "%s"
`, seq, from, time.Now().UTC().Format(time.RFC3339))
	if err := kubectlApply(ctx, cm); err != nil {
		return 0, err
	}

	pod := `apiVersion: v1
kind: Pod
metadata:
  name: follow-the-moon-wasm
  labels:
    app: follow-the-moon
spec:
  runtimeClassName: wasmedge
  containers:
    - name: wasm-worker
      image: ghcr.io/containerd/runwasi/wasi-demo-app:latest
`
	if err := kubectlApply(ctx, pod); err != nil {
		return 0, err
	}

	for i := 0; i < 30; i++ {
		out, err := exec.Command("kubectl", "--context", ctx, "get", "pod", "follow-the-moon-wasm", "-o", "jsonpath={.status.phase}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "Running" {
			return time.Since(start).Milliseconds(), nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return time.Since(start).Milliseconds(), fmt.Errorf("timeout waiting for pod in %s", region)
}

func deleteWasmPod(region string) {
	ctx := "kind-" + region
	_ = exec.Command("kubectl", "--context", ctx, "delete", "pod", "follow-the-moon-wasm", "--ignore-not-found=true", "--wait=false").Run()
}

func reconciliationLoop() {
	log.Println("[Control Plane] Deploying initial Wasm workload in us-east...")
	if ms, err := deployWasmPod("us-east", 1, "init"); err != nil {
		log.Printf("[Warning] Initial pod deploy notice: %v", err)
	} else {
		log.Printf("[Control Plane] Initial pod Running in us-east (%d ms)", ms)
	}

	for {
		resp, err := http.Get("http://localhost:8080/api/telemetry")
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var snap TelemetrySnapshot
		_ = json.NewDecoder(resp.Body).Decode(&snap)
		resp.Body.Close()

		state.mu.Lock()
		// Apply manual Chaos Eviction Spike if triggered from UI
		if state.ChaosRegion != "" {
			if reg, ok := snap.Regions[state.ChaosRegion]; ok {
				reg.EvictionRisk = 0.96
				reg.SpotPriceUSD = 0.094
				snap.Regions[state.ChaosRegion] = reg
			}
			state.ChaosRegion = ""
		}

		state.SimUTC = snap.SimUTC
		state.Regions = snap.Regions
		scores := make(map[string]float64)
		for k, m := range snap.Regions {
			scores[k] = computeUtility(m, state.Weights)
		}
		state.Scores = scores

		active := state.ActiveRegion
		curMetrics := snap.Regions[active]
		curScore := scores[active]

		bestReg := active
		bestScore := curScore
		for k, sc := range scores {
			if sc < bestScore {
				bestScore = sc
				bestReg = k
			}
		}

		// Accumulate savings vs static us-east baseline
		base := snap.Regions["us-east"]
		state.TotalCO2SavedG += (base.CarbonIntensity - curMetrics.CarbonIntensity) * 0.15
		state.TotalCostSaved += (base.SpotPriceUSD - curMetrics.SpotPriceUSD) * 0.75

		evictTrigger := curMetrics.EvictionRisk >= 0.75 && bestReg != active
		paretoTrigger := (curScore-bestScore) > state.Weights.Hysteresis && bestReg != active

		if (evictTrigger || paretoTrigger) && !state.IsMigrating {
			state.IsMigrating = true
			state.CheckpointSeq++
			seq := state.CheckpointSeq
			reason := "PARETO_OPTIMAL_SHIFT"
			if evictTrigger {
				reason = "SPOT_EVICTION_PREEMPTION"
			}
			simUTC := snap.SimUTC
			state.mu.Unlock()

			log.Printf(">>> Migrating %s -> %s (%s)...", active, bestReg, reason)
			cutoverMs, err := deployWasmPod(bestReg, seq, active)

			state.mu.Lock()
			if err == nil {
				deleteWasmPod(active)
				state.ActiveRegion = bestReg
				ev := MigrationEvent{
					ID:        len(state.Migrations) + 1,
					Timestamp: time.Now().Format("15:04:05"),
					SimUTC:    simUTC,
					From:      active,
					To:        bestReg,
					Reason:    reason,
					CutoverMs: cutoverMs,
				}
				state.Migrations = append([]MigrationEvent{ev}, state.Migrations...)
			}
			state.IsMigrating = false
		}
		state.mu.Unlock()

		time.Sleep(2500 * time.Millisecond)
	}
}

func apiStateHandler(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

func apiPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var wts PolicyWeights
	if err := json.NewDecoder(r.Body).Decode(&wts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state.mu.Lock()
	state.Weights = wts
	state.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func apiChaosHandler(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	state.mu.Lock()
	if region == "" {
		region = state.ActiveRegion
	}
	state.ChaosRegion = region
	state.mu.Unlock()
	log.Printf("[CHAOS] Injected Spot Eviction Spike into %s!", region)
	w.WriteHeader(http.StatusOK)
}

const htmlUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Follow-the-Moon | GreenOps & FinOps Wasm Control Plane</title>
<style>
  :root { --bg:#0b1120; --card:#111827; --border:#1f2937; --text:#f3f4f6; --muted:#9ca3af; --green:#10b981; --blue:#3b82f6; --amber:#f59e0b; --red:#ef4444; }
  * { box-sizing: border-box; font-family: system-ui, -apple-system, sans-serif; }
  body { margin: 0; background: var(--bg); color: var(--text); padding: 20px; }
  .header { display: flex; justify-content: space-between; align-items: center; border-bottom: 1px solid var(--border); padding-bottom: 14px; margin-bottom: 20px; }
  .badge { background: #064e3b; color: #34d399; padding: 4px 10px; border-radius: 999px; font-size: 12px; font-weight: 600; }
  .grid-3 { display: grid; grid-template-columns: repeat(3, 1fr); gap: 16px; margin-bottom: 20px; }
  .grid-2 { display: grid; grid-template-columns: 1.2fr 1fr; gap: 16px; }
  .card { background: var(--card); border: 1px solid var(--border); border-radius: 12px; padding: 16px; position: relative; transition: 0.25s; }
  .card.active { border-color: var(--green); box-shadow: 0 0 20px rgba(16,185,129,0.18); }
  .active-pill { position: absolute; top: 14px; right: 14px; background: var(--green); color: #022c22; font-size: 11px; font-weight: 800; padding: 3px 9px; border-radius: 6px; }
  .metric-row { display: flex; justify-content: space-between; margin: 8px 0; font-size: 14px; }
  .bar-bg { height: 6px; background: #1f2937; border-radius: 4px; overflow: hidden; margin-bottom: 10px; }
  .bar-fill { height: 100%; transition: width 0.4s; }
  button { cursor: pointer; border: none; border-radius: 8px; padding: 9px 14px; font-weight: 600; font-size: 13px; }
  .btn-chaos { background: rgba(239,68,68,0.15); color: #fca5a5; border: 1px solid rgba(239,68,68,0.4); width: 100%; margin-top: 8px; }
  .btn-chaos:hover { background: rgba(239,68,68,0.3); }
  .btn-preset { background: #1f2937; color: var(--text); margin-right: 6px; }
  .btn-preset:hover { background: #374151; }
  input[type=range] { width: 100%; accent-color: var(--blue); }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th, td { text-align: left; padding: 8px; border-bottom: 1px solid var(--border); }
  .kpi-row { display: flex; gap: 16px; margin-bottom: 20px; }
  .kpi { flex: 1; background: var(--card); border: 1px solid var(--border); border-radius: 10px; padding: 12px 16px; }
  .kpi-val { font-size: 22px; font-weight: 700; margin-top: 4px; }
</style>
</head>
<body>
  <div class="header">
    <div>
      <h2 style="margin:0">Follow-the-Moon Wasm Cloud Control Plane</h2>
      <span style="color:var(--muted);font-size:13px">Multi-Objective Carbon & Spot-Aware Kubernetes Orchestrator (WasmEdge + runwasi)</span>
    </div>
    <div style="text-align:right">
      <div class="badge" id="simClock">Sim UTC: --:--</div>
      <div style="color:var(--muted);font-size:12px;margin-top:4px" id="activeSummary">Active Cluster: --</div>
    </div>
  </div>

  <div class="kpi-row">
    <div class="kpi"><div style="color:var(--muted);font-size:12px">Active Wasm Pod</div><div class="kpi-val" style="color:var(--green)" id="kpiRegion">--</div></div>
    <div class="kpi"><div style="color:var(--muted);font-size:12px">Cumulative CO₂e Saved</div><div class="kpi-val" style="color:#34d399" id="kpiCO2">0.0 g</div></div>
    <div class="kpi"><div style="color:var(--muted);font-size:12px">Cumulative Spot Cost Saved</div><div class="kpi-val" style="color:#60a5fa" id="kpiCost">$0.0000</div></div>
    <div class="kpi"><div style="color:var(--muted);font-size:12px">Live Migrations Executed</div><div class="kpi-val" id="kpiMigs">0</div></div>
  </div>

  <div class="grid-3" id="regionCards"></div>

  <div class="grid-2">
    <div class="card">
      <h3 style="margin-top:0">Pareto Policy Weights (&alpha;, &beta;, &gamma;)</h3>
      <div style="margin-bottom:12px">
        <button class="btn-preset" onclick="setPreset(0.65,0.20,0.15)">Green-First</button>
        <button class="btn-preset" onclick="setPreset(0.45,0.35,0.20)">Balanced</button>
        <button class="btn-preset" onclick="setPreset(0.15,0.70,0.15)">FinOps Spot-First</button>
      </div>
      <label>Carbon Weight (&alpha;): <b id="lblA">0.45</b></label>
      <input type="range" id="slA" min="0" max="1" step="0.05" value="0.45" oninput="pushWeights()">
      <label>Spot Cost & Eviction Weight (&beta;): <b id="lblB">0.35</b></label>
      <input type="range" id="slB" min="0" max="1" step="0.05" value="0.35" oninput="pushWeights()">
      <label>Network Latency Weight (&gamma;): <b id="lblG">0.20</b></label>
      <input type="range" id="slG" min="0" max="1" step="0.05" value="0.20" oninput="pushWeights()">
    </div>

    <div class="card">
      <h3 style="margin-top:0">Live Cross-Region Migration Log</h3>
      <table>
        <thead><tr><th>Time</th><th>Route</th><th>Trigger</th><th>Cutover</th></tr></thead>
        <tbody id="migTable"><tr><td colspan="4" style="color:var(--muted)">Waiting for migration event...</td></tr></tbody>
      </table>
    </div>
  </div>

<script>
const REGION_META = {
  "us-east":  { title: "US-East (N. Virginia)", tz: "UTC-5" },
  "eu-north": { title: "EU-North (Stockholm)", tz: "UTC+1" },
  "ap-south": { title: "AP-South (Mumbai)",    tz: "UTC+5:30" }
};

function setPreset(a, b, g) {
  document.getElementById('slA').value = a;
  document.getElementById('slB').value = b;
  document.getElementById('slG').value = g;
  pushWeights();
}

function pushWeights() {
  const a = parseFloat(document.getElementById('slA').value);
  const b = parseFloat(document.getElementById('slB').value);
  const g = parseFloat(document.getElementById('slG').value);
  document.getElementById('lblA').innerText = a.toFixed(2);
  document.getElementById('lblB').innerText = b.toFixed(2);
  document.getElementById('lblG').innerText = g.toFixed(2);
  fetch('/api/policy', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ alpha: a, beta: b, gamma: g, hysteresis: 0.12 })
  });
}

function triggerChaos(reg) {
  fetch('/api/chaos?region=' + reg, { method: 'POST' });
}

async function refresh() {
  try {
    const res = await fetch('/api/state');
    const data = await res.json();
    document.getElementById('simClock').innerText = 'Sim UTC: ' + (data.sim_utc || '--:--');
    document.getElementById('activeSummary').innerText = 'Checkpoint #' + data.checkpoint_seq + (data.is_migrating ? ' (MIGRATING...)' : '');
    document.getElementById('kpiRegion').innerText = 'kind-' + data.active_region;
    document.getElementById('kpiCO2').innerText = data.total_co2_saved_g.toFixed(1) + ' g';
    document.getElementById('kpiCost').innerText = '$' + data.total_cost_saved_usd.toFixed(4);
    document.getElementById('kpiMigs').innerText = data.migrations.length;

    let cardsHtml = '';
    for (const reg of ['us-east', 'eu-north', 'ap-south']) {
      const m = data.regions[reg];
      if (!m) continue;
      const isAct = data.active_region === reg;
      const score = (data.scores[reg] || 0).toFixed(3);
      const cPct = Math.min(100, (m.carbon_gco2_kwh / 500) * 100);
      const ePct = Math.min(100, m.eviction_risk * 100);
      cardsHtml += '<div class="card ' + (isAct ? 'active' : '') + '">' +
        (isAct ? '<span class="active-pill">ACTIVE WASM POD</span>' : '') +
        '<h3 style="margin:0 0 2px 0">' + REGION_META[reg].title + '</h3>' +
        '<div style="color:var(--muted);font-size:12px;margin-bottom:12px">' + reg + ' (' + REGION_META[reg].tz + ') &bull; RTT ' + m.network_rtt_ms + 'ms</div>' +
        '<div class="metric-row"><span>Grid Carbon</span><b>' + m.carbon_gco2_kwh.toFixed(0) + ' gCO₂/kWh</b></div>' +
        '<div class="bar-bg"><div class="bar-fill" style="width:' + cPct + '%;background:var(--green)"></div></div>' +
        '<div class="metric-row"><span>Spot Price</span><b>$' + m.spot_price_usd.toFixed(4) + '/hr</b></div>' +
        '<div class="metric-row"><span>Eviction Risk</span><b>' + (m.eviction_risk * 100).toFixed(0) + '%</b></div>' +
        '<div class="bar-bg"><div class="bar-fill" style="width:' + ePct + '%;background:' + (m.eviction_risk > 0.7 ? 'var(--red)' : 'var(--amber)') + '"></div></div>' +
        '<div class="metric-row" style="border-top:1px solid var(--border);padding-top:8px"><span>Pareto Cost Score U(r)</span><b style="color:#60a5fa">' + score + '</b></div>' +
        '<button class="btn-chaos" onclick="triggerChaos(\'' + reg + '\')">Chaos: Simulate Spot Eviction Spike</button>' +
      '</div>';
    }
    document.getElementById('regionCards').innerHTML = cardsHtml;

    if (data.migrations.length > 0) {
      document.getElementById('migTable').innerHTML = data.migrations.slice(0, 6).map(m =>
        '<tr><td>' + m.sim_utc + '</td><td><b>' + m.from + ' &rarr; ' + m.to + '</b></td><td>' + m.reason + '</td><td>' + m.cutover_ms + ' ms</td></tr>'
      ).join('');
    }
  } catch (e) {}
}
setInterval(refresh, 1500);
refresh();
</script>
</body>
</html>`

func main() {
	go reconciliationLoop()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, htmlUI)
	})
	http.HandleFunc("/api/state", apiStateHandler)
	http.HandleFunc("/api/policy", apiPolicyHandler)
	http.HandleFunc("/api/chaos", apiChaosHandler)
	log.Println("[Web Control Plane] Open http://localhost:9090 in your Windows browser")
	log.Fatal(http.ListenAndServe(":9090", nil))
}
