#!/usr/bin/env python3
"""Regenerate the benchmark charts. Numbers mirror ivpol-measurements.md."""
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

SURFACE = "#fcfcfb"
DARK, LIGHT = "#24508F", "#39A0C9"   # validated pair: CVD dE 32.8, normal 32.9
INK, MUTED, GRID = "#1a1a1a", "#5c5c5c", "#e3e3e0"
VERSIONS = ["1.18", "1.19", "main"]

def style(ax, ylabel, ymax):
    ax.set_facecolor(SURFACE)
    ax.set_ylabel(ylabel, color=MUTED, fontsize=10)
    ax.set_ylim(0, ymax)                      # bars always start at zero
    ax.yaxis.grid(True, color=GRID, lw=0.8)   # recessive horizontal grid only
    ax.set_axisbelow(True)
    ax.xaxis.grid(False)
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    for s in ("left", "bottom"):
        ax.spines[s].set_color(GRID)
    ax.tick_params(colors=MUTED, labelsize=10, length=0)

def chart(name, title, values, ylabel, fmt="{:g}", series2=None, labels=None):
    fig, ax = plt.subplots(figsize=(8, 4), dpi=160)
    fig.patch.set_facecolor(SURFACE)
    top = max(values + (series2 or []))
    if series2 is None:
        bars = ax.bar(VERSIONS, values, width=0.5, color=DARK)
        for b, v in zip(bars, values):
            ax.text(b.get_x() + b.get_width()/2, v + top*0.03, fmt.format(v),
                    ha="center", color=INK, fontsize=11, fontweight="bold")
    else:
        x = range(len(VERSIONS)); w = 0.32
        b1 = ax.bar([i - w/2 - 0.012 for i in x], values, w, color=DARK, label=labels[0])
        b2 = ax.bar([i + w/2 + 0.012 for i in x], series2, w, color=LIGHT, label=labels[1])
        ax.set_xticks(list(x)); ax.set_xticklabels(VERSIONS)
        for bars, vals in ((b1, values), (b2, series2)):
            for b, v in zip(bars, vals):
                ax.text(b.get_x() + b.get_width()/2, v + top*0.03, fmt.format(v),
                        ha="center", color=INK, fontsize=10, fontweight="bold")
        leg = ax.legend(frameon=False, loc="upper right", fontsize=10)
        for t in leg.get_texts():
            t.set_color(MUTED)
    style(ax, ylabel, top * 1.20)
    ax.set_title(title, color=INK, fontsize=13, fontweight="bold", pad=14)
    fig.tight_layout()
    fig.savefig(f"{name}.png", facecolor=SURFACE)
    plt.close(fig)
    print("wrote", name + ".png")

chart("latency", "Admission latency - lower is better", [990, 312, 96],
      "milliseconds", series2=[1170, 333, 107], labels=["p50", "p99"])
chart("throughput", "Sustained image ops/sec - higher is better", [1.0, 3.2, 10.4],
      "admissions per second", fmt="{:.1f}")
chart("memory", "Memory per replica - lower is better", [607, 463, 65], "MiB")
chart("cpu", "CPU per replica - lower is better", [44, 67, 34], "millicores")
chart("registry-calls", "Registry calls per admission - lower is better", [14, 10, 3], "calls")
chart("scaling", "Scaling efficiency at concurrency 32 - higher is better", [7, 100, 103],
      "percent of linear", fmt="{:g}%")
