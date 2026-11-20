package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

type RegionMetrics struct {
	Region          string  `json:"region"`
	LocalHour       float64 `json:"local_hour"`
	CarbonIntensity float64 `json:"carbon_gco2_kwh"` // 50 to 500 gCO2eq/kWh
	SpotPriceUSD    float64 `json:"spot_price_usd"`  // $0.015 to $0.095/hr
	EvictionRisk    float64 `json:"eviction_risk"`   // 0.0 to 1.0
	NetworkRTTMs    float64 `json:"network_rtt_ms"`  // Matches tc netem setup
}

type TelemetrySnapshot struct {
	SimUTC       string                   `json:"sim_utc"`
	SimHourUTC   float64                  `json:"sim_hour_utc"`
	Regions      map[string]RegionMetrics `json:"regions"`
}

var (
	mu       sync.RWMutex
	snapshot TelemetrySnapshot
	start    = time.Now()
)

// computeRegionMetrics generates diurnal solar/carbon curves and spot volatility
func computeRegionMetrics(region string, utcHour float64, tzOffset float64, baseCarbon float64, rttMs float64) RegionMetrics {
	localHour := math.Mod(utcHour+tzOffset+24.0, 24.0)

	// Solar generation peaks at 13:00 local time -> lowest carbon intensity
	solarFactor := math.Cos((localHour - 13.0) * math.Pi / 12.0) // +1 at 13:00, -1 at 01:00
	carbon := baseCarbon - (140.0 * solarFactor) + (rand.Float64()*15.0 - 7.5)
	if carbon < 45.0 {
		carbon = 45.0
	}

	// Business demand peaks at 15:00 local time -> highest spot price & eviction risk
	demandFactor := (math.Cos((localHour-15.0)*math.Pi/12.0) + 1.0) / 2.0 // 0.0 to 1.0
	spotPrice := 0.018 + (0.065 * demandFactor) + (rand.Float64() * 0.005)
	evictionRisk := 0.08 + (0.65 * demandFactor)

	// Inject occasional spot reclamation spike (10% chance)
	if rand.Float64() < 0.10 {
		evictionRisk = math.Min(0.96, evictionRisk+0.35)
	}

	return RegionMetrics{
		Region:          region,
		LocalHour:       math.Round(localHour*100) / 100,
		CarbonIntensity: math.Round(carbon*100) / 100,
		SpotPriceUSD:    math.Round(spotPrice*10000) / 10000,
		EvictionRisk:    math.Round(evictionRisk*1000) / 1000,
		NetworkRTTMs:    rttMs,
	}
}

func updateLoop() {
	for {
		// 1 real second = 15 simulated minutes (0.25 hours)
		elapsedSimHours := time.Since(start).Seconds() * 0.25
		utcHour := math.Mod(elapsedSimHours, 24.0)

		regions := map[string]RegionMetrics{
			"us-east":  computeRegionMetrics("us-east", utcHour, -5.0, 340.0, 10.0),
			"eu-north": computeRegionMetrics("eu-north", utcHour, 1.0, 210.0, 90.0),
			"ap-south": computeRegionMetrics("ap-south", utcHour, 5.5, 420.0, 180.0),
		}

		mu.Lock()
		snapshot = TelemetrySnapshot{
			SimUTC:     fmt.Sprintf("%02d:%02d UTC", int(utcHour), int((utcHour-float64(int(utcHour)))*60)),
			SimHourUTC: math.Round(utcHour*100) / 100,
			Regions:    regions,
		}
		mu.Unlock()

		time.Sleep(1 * time.Second)
	}
}

func telemetryHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

func prometheusHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP cloud_region_carbon_intensity Grid carbon intensity in gCO2eq/kWh")
	fmt.Fprintln(w, "# TYPE cloud_region_carbon_intensity gauge")
	for _, m := range snapshot.Regions {
		fmt.Fprintf(w, "cloud_region_carbon_intensity{region=\"%s\"} %.2f\n", m.Region, m.CarbonIntensity)
		fmt.Fprintf(w, "cloud_region_spot_price_usd{region=\"%s\"} %.4f\n", m.Region, m.SpotPriceUSD)
		fmt.Fprintf(w, "cloud_region_eviction_risk{region=\"%s\"} %.3f\n", m.Region, m.EvictionRisk)
	}
}

func main() {
	go updateLoop()
	http.HandleFunc("/api/telemetry", telemetryHandler)
	http.HandleFunc("/metrics", prometheusHandler)
	log.Println("[Telemetry Simulator] Listening on :8080 (/api/telemetry and /metrics)")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
