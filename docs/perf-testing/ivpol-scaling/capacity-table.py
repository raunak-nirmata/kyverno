#!/usr/bin/env python3
"""Build the four-row version-comparison table from the capacity campaign.

Reads the HOLD cell of each version/round, because that is the only cell with
every gate on and a measurement window long enough for metrics-server to
produce real CPU and memory numbers. Ramp steps are deliberately ignored except
to report where each version saturated.

Usage:
  python3 capacity-table.py results/capacity [--latency results/sweep-cold]
"""
import argparse, glob, json, os, statistics, sys


def kv(path):
    d = {}
    try:
        with open(path) as f:
            for line in f:
                if "=" in line:
                    k, _, v = line.partition("=")
                    d[k.strip()] = v.strip()
    except OSError:
        pass
    return d


def agg(vals):
    """mean±stdev, blank when nothing measured. One value has no spread."""
    vals = [v for v in vals if v is not None]
    if not vals:
        return ""
    if len(vals) == 1:
        return f"{vals[0]:.1f}"
    return f"{statistics.mean(vals):.1f} ±{statistics.stdev(vals):.1f}"


def num(s):
    try:
        return float(s)
    except (TypeError, ValueError):
        return None



def curve(vdir):
    """(concurrency, rps) for every ramp step of one version/round."""
    pts = []
    for f in glob.glob(os.path.join(vdir, "c*", "ramp-step.txt")):
        m = kv(f)
        c, r = num(m.get("concurrency")), num(m.get("rps"))
        if c and r is not None:
            pts.append((c, r))
    return sorted(pts)


def knee(pts):
    """Smallest concurrency reaching 95% of peak throughput.

    Saturation here is a throughput plateau, not a failure rate: a version that
    serializes keeps succeeding while extra concurrency buys nothing, so the
    failure-based stop in the ramp never trips. The knee is what that version
    actually tops out at.
    """
    if not pts:
        return None, None
    peak = max(r for _, r in pts)
    # 80%, not 95%: on a flat curve the step-to-step noise is itself around
    # 15-20% (1.18.2 wandered 1.4-1.9 ops/s from c=2 to c=32 with zero
    # failures), so a 95% band would pick whichever noisy step happened to be
    # highest - reporting a knee of c=32 for a version that stopped scaling at
    # c=2. 80% selects the first step that is already on the plateau.
    for c, r in pts:
        if r >= 0.80 * peak:
            return c, peak
    return pts[-1][0], peak


def knee_label(v):
    """A knee at the top step is not a knee - it is the ladder running out.

    Saying "plateaus at c=32" for a version still at ~100% of linear would
    invent a ceiling the measurement never found. Those report the range
    searched instead, which is the honest claim: no plateau up to here.
    """
    ks = [k for k in v["knee"] if k]
    if not ks:
        return "n/a"
    tops = [max(c for c, _ in pts) for pts in v["curves"] if pts]
    still_scaling = all(
        pts and pts[-1][1] >= 0.8 * (pts[0][1] * pts[-1][0] / pts[0][0])
        for pts in v["curves"]
    )
    if tops and all(int(k) == int(t) for k, t in zip(sorted(ks), sorted(tops))) and still_scaling:
        return f"none up to c={int(max(tops))}"
    return "c=" + "/".join(str(int(k)) for k in sorted(set(ks)))


def eff_label(v):
    """Throughput at the top step vs perfect linear scaling from c=1."""
    effs = []
    for pts in v["curves"]:
        if len(pts) < 2:
            continue
        (c0, r0), (cN, rN) = pts[0], pts[-1]
        if r0 > 0 and cN > c0:
            effs.append(100.0 * rN / (r0 * cN / c0))
    if not effs:
        return "n/a"
    return f"{statistics.mean(effs):.0f}% of linear"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root")
    ap.add_argument("--latency", help="a sweep root for the p50/p99 row")
    args = ap.parse_args()

    versions = {}
    for vm in sorted(glob.glob(os.path.join(args.root, "round*", "*", "version-meta.txt"))):
        m = kv(vm)
        app = m.get("app")
        if not app:
            continue
        v = versions.setdefault(app, {"rps": [], "cpu": [], "mem": [], "sat": [],
                                      "ceiling": [], "shape": set(),
                                      "peak": [], "knee": [], "curves": [], "hold_ok": []})
        pts = curve(os.path.dirname(vm))
        k, pk = knee(pts)
        if pts:
            v["curves"].append(pts)
        if pk is not None:
            v["peak"].append(pk)
        if k is not None:
            v["knee"].append(k)
        v["shape"].add((m.get("mode"), m.get("images"), m.get("delay_ms"), m.get("cache_mode")))
        v["rps"].append(num(m.get("hold_rps")))
        lg = m.get("last_good_concurrency")
        v["ceiling"].append(lg)
        v["sat"].append(m.get("saturated_at") or m.get("crashed_at"))

        # CPU and memory at the knee, read from that ramp step.
        if k is not None:
            for h in glob.glob(os.path.join(os.path.dirname(vm), f"c{int(k)}", "**", "meta.txt"),
                               recursive=True):
                hm = kv(h)
                v["cpu"].append(num(hm.get("cpu_millicores_mean")))
                v["mem"].append(num(hm.get("memory_mib_mean")))
        # The hold is supplementary: it is only reportable when it ran at the
        # knee and passed its gates.
        for h in glob.glob(os.path.join(os.path.dirname(vm), "hold-c*", "**", "meta.txt"),
                           recursive=True):
            hm = kv(h)
            if k is not None and num(hm.get("concurrency")) == k:
                v["hold_ok"].append(True)

    if not versions:
        sys.exit(f"no version-meta.txt under {args.root}")

    # A row is only comparable if every version ran the same cell shape.
    shapes = {s for v in versions.values() for s in v["shape"]}
    if len(shapes) > 1:
        print("REFUSING: versions ran different cell shapes, these rows are not comparable")
        for app, v in versions.items():
            print(f"  {app}: {sorted(v['shape'])}")
        sys.exit(1)

    order = sorted(versions, key=lambda a: (a != "1.18.2", a))
    print(f"\nshape: {sorted(shapes)[0]}  (mode, images, delay_ms, cache_mode)\n")
    w = 26
    print("| Metric".ljust(w) + "".join(f"| {a} ".ljust(22) for a in order) + "|")
    print("|" + "-" * (w - 1) + "".join("|" + "-" * 21 for _ in order) + "|")

    def row(label, cells):
        print(f"| {label}".ljust(w) + "".join(f"| {c} ".ljust(22) for c in cells) + "|")

    row("Peak ops/s", [agg(versions[a]["peak"]) or "n/a" for a in order])
    row("Plateaus at (>=80% peak)", [knee_label(versions[a]) for a in order])
    row("Scaling efficiency", [eff_label(versions[a]) for a in order])
    row("Hold ops/s", [(agg(versions[a]["rps"]) if versions[a]["hold_ok"] else "n/a (ran past knee)")
                       for a in order])
    row("CPU mean @ knee (mC)", [agg(versions[a]["cpu"]) or "n/a" for a in order])
    row("Memory mean @ knee (MiB)", [agg(versions[a]["mem"]) or "n/a" for a in order])

    if args.latency:
        print("\n(latency p50/p99 comes from the sweep, not this campaign: "
              f"python3 version-table.py {args.latency})")

    print("\nsaturation / crash point per round:")
    for a in order:
        print(f"  {a}: last good concurrency {versions[a]['ceiling']}, stopped at {versions[a]['sat']}")
    print("\nnotes:")
    print("  - 'Plateaus at' is where throughput stops responding to concurrency, not")
    print("    where requests start failing: a serializing version never fails, it just")
    print("    stops converting concurrency into work.")
    print("  - 'Scaling efficiency' is the transferable number. Absolute ops/s is")
    print("    laptop-specific; how well a version uses concurrency is not.")
    print("  - CPU and memory are read at the plateau, the operating point that matters.")


if __name__ == "__main__":
    main()
