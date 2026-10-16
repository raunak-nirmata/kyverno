#!/usr/bin/env python3
"""Aggregate the shuffled round-robin ClusterPolicy ladder into version-vs-version tables.

Correction basis (changed from the old pairwise flow): the old two-version report
took the geometric mean of a forward-order and reverse-order ratio to cancel the
second-run thermal penalty. Here the *shuffled round-robin* cancels ordering/
position drift, so per (version, cell) we take the geometric mean of that version's
metric across ALL rounds, then report each version RELATIVE TO the baseline
(default v1.13.6) — for admit and deny separately.

Emits every metric the regression tables track:
  - avg admission latency (headline)   - p99 admission latency
  - throughput (iterations/s)          - max Kyverno CPU (cores) / max memory (MiB)
plus a per-version round-coverage check.

Reads:  <results_root>/round<R>/<app>/{admit,deny}/r<reps>-<vus>x<iters>/
          k6-summary.json   (iteration_duration avg/max/p(99), iterations.rate)
          kubectl-top.csv   (max CPU / max memory over the cell)

Usage:  ladder-report.py <results_root> [--baseline 1.13.6]
"""
from __future__ import annotations
import json, math, re, sys
from pathlib import Path


def geomean(xs):
    xs = [x for x in xs if isinstance(x, (int, float)) and x > 0]
    return math.exp(sum(math.log(x) for x in xs) / len(xs)) if xs else None


def mean(xs):
    xs = [x for x in xs if isinstance(x, (int, float))]
    return sum(xs) / len(xs) if xs else None


def summ(cell: Path):
    """avg/max/p99 latency (ms) and throughput (iters/s) from k6-summary.json."""
    try:
        mm = json.loads((cell / "k6-summary.json").read_text())["metrics"]
        d = mm.get("iteration_duration", {})
        return {"avg": d.get("avg"), "max": d.get("max"), "p99": d.get("p(99)"),
                "tps": mm.get("iterations", {}).get("rate")}
    except Exception:
        return {}


def top_max(cell: Path):
    """Max total Kyverno CPU (millicores) and memory (MiB) over the cell, from kubectl-top.csv."""
    f = cell / "kubectl-top.csv"
    if not f.is_file():
        return None, None
    ts_cpu, ts_mem = {}, {}
    for line in f.read_text().splitlines()[1:]:
        p = line.split(",")
        if len(p) != 4:
            continue
        ts, _pod, cpu, mem = p
        try:
            cv = float(re.sub(r"m$", "", cpu.strip()))
        except ValueError:
            cv = 0.0
        mm = mem.strip()
        if mm.endswith("Gi"):
            mv = float(mm[:-2]) * 1024
        elif mm.endswith("Mi"):
            mv = float(mm[:-2])
        else:
            mv = float(re.sub(r"[^0-9.]", "", mm) or 0)
        ts_cpu[ts] = ts_cpu.get(ts, 0.0) + cv
        ts_mem[ts] = ts_mem.get(ts, 0.0) + mv
    return (max(ts_cpu.values()) if ts_cpu else None,
            max(ts_mem.values()) if ts_mem else None)


def cell_key(name):
    m = re.match(r"r(\d+)-(\d+)x(\d+)s?", name)
    return (int(m.group(1)), int(m.group(2)), int(m.group(3))) if m else (9, 9, 9)


def delta_table(title, note, cells, others, baseline, base_vals, ver_vals, unit, invert=False):
    """Print a %-delta-vs-baseline table for one metric. invert=True => negative delta is 'worse'."""
    print(f"#### {title}\n")
    hdr = ["Cell", f"{baseline} ({unit}, base)"] + [f"{v} Δ%" for v in others]
    print("| " + " | ".join(hdr) + " |")
    print("|" + "---|" * len(hdr))
    for c in cells:
        b = geomean(base_vals.get(c, []))
        row = [c, f"{b:.1f}" if b else "-"]
        for v in others:
            g = geomean(ver_vals[v].get(c, []))
            row.append(f"{(g/b-1)*100:+.0f}%" if (g and b) else "-")
        print("| " + " | ".join(row) + " |")
    print(f"\n_{note}_\n")


def main():
    if len(sys.argv) < 2:
        print(__doc__); return 2
    root = Path(sys.argv[1])
    baseline = sys.argv[sys.argv.index("--baseline") + 1] if "--baseline" in sys.argv else "1.13.6"

    rounds = sorted(p for p in root.glob("round*") if p.is_dir())
    if not rounds:
        print(f"no round*/ dirs under {root}", file=sys.stderr); return 1
    versions = sorted({d.name for rd in rounds for d in rd.iterdir() if d.is_dir()})
    if baseline not in versions:
        print(f"baseline {baseline} not among versions {versions}", file=sys.stderr); return 1
    others = [v for v in versions if v != baseline]

    print(f"# ClusterPolicy regression ladder — {len(rounds)} rounds, "
          f"versions {', '.join(versions)}\n")
    print(f"Each version's metric is the geometric mean across all {len(rounds)} rounds "
          f"(shuffled round-robin cancels ordering/thermal-position drift), shown relative "
          f"to the **{baseline}** baseline. Admit and deny reported separately.\n")

    for path in ("admit", "deny"):
        avg = {v: {} for v in versions}; p99 = {v: {} for v in versions}
        tps = {v: {} for v in versions}; cpu = {v: {} for v in versions}
        mem = {v: {} for v in versions}
        cells = set()
        for rd in rounds:
            for v in versions:
                pdir = rd / v / path
                if not pdir.is_dir():
                    continue
                for cd in pdir.iterdir():
                    if not cd.is_dir():
                        continue
                    cells.add(cd.name)
                    s = summ(cd); cmax, mmax = top_max(cd)
                    if s.get("avg"): avg[v].setdefault(cd.name, []).append(s["avg"])
                    if s.get("p99"): p99[v].setdefault(cd.name, []).append(s["p99"])
                    if s.get("tps"): tps[v].setdefault(cd.name, []).append(s["tps"])
                    if cmax: cpu[v].setdefault(cd.name, []).append(cmax)
                    if mmax: mem[v].setdefault(cd.name, []).append(mmax)
        if not cells:
            print(f"## {path.upper()} path — no data\n"); continue
        cells = sorted(cells, key=cell_key)

        print(f"## {path.upper()} path\n")
        delta_table("Avg admission latency", f"Positive % = slower than {baseline}.",
                    cells, others, baseline, avg[baseline], avg, "ms")
        delta_table("p99 admission latency", f"Positive % = worse tail than {baseline}.",
                    cells, others, baseline, p99[baseline], p99, "ms")
        delta_table("Throughput (iterations/s)", f"Negative % = lower throughput than {baseline}.",
                    cells, others, baseline, tps[baseline], tps, "it/s")

        # absolute max CPU / mem per version (means of per-round maxima)
        print("#### Max Kyverno CPU (cores) / max memory (MiB) — mean of per-round maxima\n")
        hdr = ["Cell"] + [f"{v} cpu/mem" for v in versions]
        print("| " + " | ".join(hdr) + " |")
        print("|" + "---|" * len(hdr))
        for c in cells:
            row = [c]
            for v in versions:
                cavg = mean(cpu[v].get(c, [])); mavg = mean(mem[v].get(c, []))
                row.append(f"{cavg/1000:.2f} / {mavg:.0f}" if (cavg and mavg) else "-")
            print("| " + " | ".join(row) + " |")
        print()

    print("## Coverage (rounds with data per version, admit path)\n")
    for v in versions:
        n = sum(1 for rd in rounds if (rd / v / "admit").is_dir() and any((rd / v / "admit").iterdir()))
        print(f"- {v}: {n}/{len(rounds)} rounds")
    print("\n_Raw per-cell data (all trend stats, Kyverno admission-latency metric, etcd size, "
          "pod restarts) is in each round<R>/<version>/{admit,deny}/<cell>/ — k6-summary.json, "
          "meta.txt, kubectl-top.csv, and the resource/etcd reports._")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
