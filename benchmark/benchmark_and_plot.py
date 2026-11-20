import csv
import json
import subprocess
import time
import urllib.request
from pathlib import Path

import matplotlib as mpl
import matplotlib.pyplot as plt
import numpy as np

mpl.rcParams.update({
    "font.family": "serif",
    "font.serif": ["Times New Roman", "DejaVu Serif"],
    "font.size": 8.5,
    "axes.titlesize": 9.5,
    "axes.labelsize": 8.5,
    "xtick.labelsize": 7.5,
    "ytick.labelsize": 7.5,
    "legend.fontsize": 7.5,
    "axes.linewidth": 0.6,
    "lines.linewidth": 1.2,
    "xtick.direction": "in",
    "ytick.direction": "in",
    "savefig.dpi": 600,
})

REGION_STYLES = {
    "us-east":  {"color": "#0072B2", "ls": "-",  "marker": "o", "label": "us-east (UTC-5)"},
    "eu-north": {"color": "#009E73", "ls": "--", "marker": "s", "label": "eu-north (UTC+1)"},
    "ap-south": {"color": "#D55E00", "ls": "-.", "marker": "^", "label": "ap-south (UTC+5:30)"},
}

TELEMETRY_URL = "http://localhost:8080/api/telemetry"
CLUSTERS = ["us-east", "eu-north", "ap-south"]
SAMPLE_INTERVAL_SEC = 2.0
TOTAL_SAMPLES = 50  # 100s total (~25 simulated hours)


def detect_active_region(fallback: str) -> str:
    for reg in CLUSTERS:
        try:
            out = subprocess.check_output(
                [
                    "kubectl", "--context", f"kind-{reg}",
                    "get", "pod", "follow-the-moon-wasm",
                    "-o", "jsonpath={.status.phase}",
                ],
                stderr=subprocess.DEVNULL,
                timeout=1.5,
            ).decode().strip()
            if out == "Running":
                return reg
        except Exception:
            continue
    return fallback


def record_live_trace():
    records = []
    last_active = "us-east"
    print(f"[Benchmark] Recording {TOTAL_SAMPLES} samples (~100s = 25h simulated UTC)...")

    for step in range(TOTAL_SAMPLES):
        try:
            with urllib.request.urlopen(TELEMETRY_URL, timeout=2.0) as resp:
                snap = json.loads(resp.read().decode())
        except Exception as exc:
            raise RuntimeError(
                f"Cannot reach Telemetry Simulator at {TELEMETRY_URL}. "
                f"Ensure Tab 1 (telemetry/main.go) is running. ({exc})"
            )

        active_reg = detect_active_region(last_active)
        migrated = (step > 0) and (active_reg != last_active)
        last_active = active_reg
        regs = snap["regions"]

        carbon_only_reg = min(CLUSTERS, key=lambda r: regs[r]["carbon_gco2_kwh"])
        spot_only_reg = min(
            CLUSTERS,
            key=lambda r: (regs[r]["spot_price_usd"] / 0.10) + regs[r]["eviction_risk"],
        )

        row = {
            "step": step,
            "sim_utc": snap["sim_utc"],
            "sim_hour": step * 0.5,
            "active_region": active_reg,
            "migrated": int(migrated),
            "carbon_only_region": carbon_only_reg,
            "spot_only_region": spot_only_reg,
        }
        for r in CLUSTERS:
            row[f"{r}_carbon"] = regs[r]["carbon_gco2_kwh"]
            row[f"{r}_spot"] = regs[r]["spot_price_usd"]
            row[f"{r}_evict"] = regs[r]["eviction_risk"]
            row[f"{r}_rtt"] = regs[r]["network_rtt_ms"]

        records.append(row)
        tag = f"  <-- MIGRATED to {active_reg}" if migrated else ""
        print(
            f"  [{step+1:02d}/{TOTAL_SAMPLES}] Sim={row['sim_utc']} | "
            f"Active={active_reg:<8} | "
            f"CO2(US/EU/AP)={row['us-east_carbon']:.0f}/{row['eu-north_carbon']:.0f}/{row['ap-south_carbon']:.0f}{tag}"
        )
        time.sleep(SAMPLE_INTERVAL_SEC)

    return records


def save_csv(records, out_dir: Path):
    csv_path = out_dir / "benchmark_trace.csv"
    with open(csv_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=list(records[0].keys()))
        writer.writeheader()
        writer.writerows(records)
    print(f"[Saved] Raw telemetry trace -> {csv_path}")


def generate_ieee_figures(records, out_dir: Path):
    hours = np.array([r["sim_hour"] for r in records])

    # Figure 1: Diurnal Carbon Intensity + Active Region Placement
    fig, (ax1, ax2) = plt.subplots(
        2, 1, figsize=(7.16, 3.6), sharex=True,
        gridspec_kw={"height_ratios": [3.2, 1.0], "hspace": 0.14},
    )
    for reg in CLUSTERS:
        st = REGION_STYLES[reg]
        vals = [r[f"{reg}_carbon"] for r in records]
        ax1.plot(
            hours, vals, color=st["color"], linestyle=st["ls"],
            marker=st["marker"], markevery=4, markersize=3.5, label=st["label"],
        )

    mig_hours = [r["sim_hour"] for r in records if r["migrated"] == 1]
    for idx, mh in enumerate(mig_hours):
        ax1.axvline(
            mh, color="#333333", linestyle=":", linewidth=0.9,
            label="Live Wasm Migration" if idx == 0 else "",
        )

    ax1.set_ylabel("Grid Carbon Intensity\n($\\mathrm{gCO_2eq/kWh}$)")
    ax1.set_title("Diurnal Carbon Intensity Dynamics and Follow-the-Moon Wasm Placement")
    ax1.grid(True, linestyle=":", linewidth=0.4, alpha=0.7)
    ax1.legend(loc="upper right", ncol=2, frameon=True, framealpha=0.9)

    reg_to_idx = {"us-east": 0, "eu-north": 1, "ap-south": 2}
    active_idx = [reg_to_idx[r["active_region"]] for r in records]
    ax2.step(hours, active_idx, where="post", color="#111111", linewidth=1.4)
    ax2.set_yticks([0, 1, 2])
    ax2.set_yticklabels(["us-east", "eu-north", "ap-south"])
    ax2.set_ylabel("Active\nCluster")
    ax2.set_xlabel("Simulated Time Elapsed (Hours)")
    ax2.set_ylim(-0.4, 2.4)
    ax2.grid(True, linestyle=":", linewidth=0.4, alpha=0.7)

    fig.tight_layout()
    fig.savefig(out_dir / "fig1_diurnal_carbon_and_placement.png")
    fig.savefig(out_dir / "fig1_diurnal_carbon_and_placement.pdf")
    plt.close(fig)

    # Figure 2: Cumulative Carbon & Cost Savings
    kwh_per_step = 0.075
    cum_co2_saved = np.cumsum([
        (r["us-east_carbon"] - r[f"{r['active_region']}_carbon"]) * kwh_per_step
        for r in records
    ])
    cum_cost_saved = np.cumsum([
        (r["us-east_spot"] - r[f"{r['active_region']}_spot"]) * 0.5
        for r in records
    ])

    fig, ax1 = plt.subplots(figsize=(3.5, 2.5))
    color1 = "#009E73"
    ax1.plot(hours, cum_co2_saved, color=color1, linestyle="-")
    ax1.fill_between(hours, 0, cum_co2_saved, color=color1, alpha=0.12)
    ax1.set_xlabel("Simulated Time Elapsed (Hours)")
    ax1.set_ylabel("Cumulative $\\mathrm{CO_2eq}$ Saved (g)", color=color1)
    ax1.tick_params(axis="y", labelcolor=color1)
    ax1.grid(True, linestyle=":", linewidth=0.4, alpha=0.7)

    ax2 = ax1.twinx()
    color2 = "#0072B2"
    ax2.plot(hours, cum_cost_saved, color=color2, linestyle="--")
    ax2.set_ylabel("Cumulative Cost Saved (USD)", color=color2)
    ax2.tick_params(axis="y", labelcolor=color2)

    ax1.set_title("Cumulative Operational Savings vs. Static Baseline")
    fig.tight_layout()
    fig.savefig(out_dir / "fig2_cumulative_savings.png")
    fig.savefig(out_dir / "fig2_cumulative_savings.pdf")
    plt.close(fig)

    # Figure 3: Multi-Baseline Normalized Comparison
    strategies = {
        "Static\n(us-east)": ["us-east" for _ in records],
        "Carbon-\nOnly": [r["carbon_only_region"] for r in records],
        "Spot-\nOnly": [r["spot_only_region"] for r in records],
        "Proposed\n(Pareto)": [r["active_region"] for r in records],
    }

    mean_carbon, mean_cost, mean_evict = [], [], []
    for chosen_regs in strategies.values():
        mean_carbon.append(np.mean([rec[f"{reg}_carbon"] for rec, reg in zip(records, chosen_regs)]))
        mean_cost.append(np.mean([rec[f"{reg}_spot"] for rec, reg in zip(records, chosen_regs)]))
        mean_evict.append(np.mean([rec[f"{reg}_evict"] for rec, reg in zip(records, chosen_regs)]))

    norm_carbon = np.array(mean_carbon) / mean_carbon[0] * 100.0
    norm_cost = np.array(mean_cost) / mean_cost[0] * 100.0
    norm_evict = np.array(mean_evict) / mean_evict[0] * 100.0

    x = np.arange(len(strategies))
    width = 0.25

    fig, ax = plt.subplots(figsize=(3.5, 2.6))
    ax.bar(x - width, norm_carbon, width, label="Carbon ($\\mathrm{gCO_2}$)", color="#009E73", edgecolor="black", linewidth=0.4, hatch="//")
    ax.bar(x,         norm_cost,   width, label="Spot Cost ($)",              color="#0072B2", edgecolor="black", linewidth=0.4, hatch="..")
    ax.bar(x + width, norm_evict,  width, label="Eviction Risk",              color="#E69F00", edgecolor="black", linewidth=0.4, hatch="xx")

    ax.axhline(100.0, color="#444444", linestyle="--", linewidth=0.7)
    ax.set_ylabel("Normalized Metric (% of Static Baseline)")
    ax.set_title("Multi-Objective Scheduler Comparison (Lower = Better)")
    ax.set_xticks(x)
    ax.set_xticklabels(list(strategies.keys()))
    ax.set_ylim(0, max(125, np.max([norm_carbon, norm_cost, norm_evict]) + 15))
    ax.legend(loc="upper right", frameon=True, framealpha=0.9)
    ax.grid(True, axis="y", linestyle=":", linewidth=0.4, alpha=0.7)

    fig.tight_layout()
    fig.savefig(out_dir / "fig3_baseline_comparison.png")
    fig.savefig(out_dir / "fig3_baseline_comparison.pdf")
    plt.close(fig)

    print("\n=== BENCHMARK SUMMARY (24h Simulated Cycle) ===")
    print(f"Total Live Migrations Executed : {sum(r['migrated'] for r in records)}")
    print(f"Mean Carbon Reduction vs Static: {100.0 - norm_carbon[3]:.1f}%")
    print(f"Mean Spot Cost Reduction       : {100.0 - norm_cost[3]:.1f}%")
    print(f"Mean Eviction Risk Reduction   : {100.0 - norm_evict[3]:.1f}%")
    print(f"[Saved] IEEE Figures (.png + .pdf) in: {out_dir}")


if __name__ == "__main__":
    output_dir = Path(__file__).resolve().parent
    trace = record_live_trace()
    save_csv(trace, output_dir)
    generate_ieee_figures(trace, output_dir)
