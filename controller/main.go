package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
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

// Policy weights (must sum to 1.0)
const (
	AlphaCarbon      = 0.45
	BetaSpotFinOps   = 0.35
	GammaLatency     = 0.20
	HysteresisMargin = 0.12 // Prevents rapid migration flapping
	EvictionPanic    = 0.75 // Immediate preemption threshold
)

func computeUtilityCost(m RegionMetrics) float64 {
	// Normalize metrics to [0, 1] scale (lower score = better region)
	normCarbon := m.CarbonIntensity / 500.0
	normSpot := ((m.SpotPriceUSD / 0.10) + m.EvictionRisk) / 2.0
	normLatency := m.NetworkRTTMs / 200.0

	return (AlphaCarbon * normCarbon) + (BetaSpotFinOps * normSpot) + (GammaLatency * normLatency)
}

func kubectlApply(context string, manifest string) error {
	cmd := exec.Command("kubectl", "--context", context, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return nil
}

func deployWasmWorkload(region string, checkpointSeq int, sourceRegion string) (time.Duration, error) {
	start := time.Now()
	ctx := "kind-" + region

	// 1. Transfer linear-memory checkpoint metadata via ConfigMap
	cmManifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: wasm-checkpoint-state
data:
  checkpoint_seq: "%d"
  migrated_from: "%s"
  timestamp: "%s"
`, checkpointSeq, sourceRegion, time.Now().UTC().Format(time.RFC3339))

	if err := kubectlApply(ctx, cmManifest); err != nil {
		return 0, err
	}

	// 2. Deploy WasmEdge Pod in target region
	podManifest := `apiVersion: v1
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
	if err := kubectlApply(ctx, podManifest); err != nil {
		return 0, err
	}

	// 3. Wait until target Wasm pod is Running before draining source
	for i := 0; i < 30; i++ {
		out, err := exec.Command("kubectl", "--context", ctx, "get", "pod", "follow-the-moon-wasm", "-o", "jsonpath={.status.phase}").Output()
		if err == nil && strings.TrimSpace(string(out)) == "Running" {
			return time.Since(start), nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return time.Since(start), fmt.Errorf("timeout waiting for pod in %s", region)
}

func deleteWasmWorkload(region string) {
	ctx := "kind-" + region
	_ = exec.Command("kubectl", "--context", ctx, "delete", "pod", "follow-the-moon-wasm", "--ignore-not-found=true", "--wait=false").Run()
}

func main() {
	activeRegion := "us-east"
	checkpointSeq := 1
	migrationCount := 0

	var totalCarbonSavedGrams float64
	var totalCostSavedUSD float64

	log.Printf("[Controller] Initializing workload in default region: %s\n", activeRegion)
	if dur, err := deployWasmWorkload(activeRegion, checkpointSeq, "init"); err != nil {
		log.Fatalf("Initial deployment failed: %v", err)
	} else {
		log.Printf("[Controller] Initial Wasm pod Running in %s (cold-start: %v)\n", activeRegion, dur.Round(time.Millisecond))
	}

	for {
		resp, err := http.Get("http://localhost:8080/api/telemetry")
		if err != nil {
			log.Printf("[Warning] Waiting for Telemetry Simulator on :8080... (%v)\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		var snap TelemetrySnapshot
		_ = json.NewDecoder(resp.Body).Decode(&snap)
		resp.Body.Close()

		currentMetrics := snap.Regions[activeRegion]
		currentScore := computeUtilityCost(currentMetrics)

		// Find best candidate region
		bestRegion := activeRegion
		bestScore := currentScore
		for reg, m := range snap.Regions {
			score := computeUtilityCost(m)
			if score < bestScore {
				bestScore = score
				bestRegion = reg
			}
		}

		// Accumulate benchmark savings vs. static us-east baseline (per simulated 45-min tick)
		baseline := snap.Regions["us-east"]
		carbonDelta := (baseline.CarbonIntensity - currentMetrics.CarbonIntensity) * 0.15 // 150W node equivalent
		costDelta := (baseline.SpotPriceUSD - currentMetrics.SpotPriceUSD) * 0.75
		totalCarbonSavedGrams += carbonDelta
		totalCostSavedUSD += costDelta

		log.Printf("[%s] Active=%s (U=%.3f | %0.f gCO2 | $%.4f | Evict=%.2f) | Best=%s (U=%.3f) | Net CO2 Saved=%.1fg\n",
			snap.SimUTC, activeRegion, currentScore, currentMetrics.CarbonIntensity,
			currentMetrics.SpotPriceUSD, currentMetrics.EvictionRisk, bestRegion, bestScore, totalCarbonSavedGrams)

		// Check migration triggers: Pareto hysteresis improvement OR spot eviction panic
		evictionTriggered := currentMetrics.EvictionRisk >= EvictionPanic && bestRegion != activeRegion
		paretoTriggered := (currentScore-bestScore) > HysteresisMargin && bestRegion != activeRegion

		if evictionTriggered || paretoTriggered {
			reason := "PARETO_OPTIMAL_SHIFT"
			if evictionTriggered {
				reason = "SPOT_EVICTION_PREEMPTION"
			}
			checkpointSeq++
			migrationCount++

			log.Printf(">>> [MIGRATION #%d | %s] %s -> %s (Checkpoint #%d)...\n",
				migrationCount, reason, activeRegion, bestRegion, checkpointSeq)

			cutoverTime, err := deployWasmWorkload(bestRegion, checkpointSeq, activeRegion)
			if err != nil {
				log.Printf("!!! Migration failed: %v\n", err)
			} else {
				oldRegion := activeRegion
				activeRegion = bestRegion
				deleteWasmWorkload(oldRegion)
				log.Printf(">>> [MIGRATION COMPLETE] Cutover in %v | Active Region is now %s | Cumulative Savings: %.1fg CO2, $%.4f\n",
					cutoverTime.Round(time.Millisecond), activeRegion, totalCarbonSavedGrams, totalCostSavedUSD)
			}
		}

		time.Sleep(3 * time.Second)
	}
}
