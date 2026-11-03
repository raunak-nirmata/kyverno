#!/usr/bin/env python3
"""Version-comparison summary: fills in the scaling-ladder table, one row per
(Kyverno version, signature mode, cell shape), averaged across rounds.

Walks every round*/<version>/[<mode>/]img*-d*/ cell under the given results
roots - the layout ivpol-run.sh and ladder-ivpol-run.sh already write - and
reports, per (version, mode, shape):

  rounds              distinct round numbers contributing to the average
  admission p50/p99   k6's client-side iteration_duration (PRIMARY, see below)
  kyverno hist mean/p99 Kyverno's own admission_review_duration histogram
  registry calls/img  total registry requests / iterations / images_per_pod,
                       plus a breakdown by request kind (ping/manifest/blob/
                       referrers/sig_tag) in a separate table
  cache cold vs warm  the same ratio, split by cache_mode=off vs warm
  cpu / mem           peak (worst observed) and mean, from kubectl-top.csv

Primary latency instrument: k6's iteration_duration, not Kyverno's histogram.
They disagree, and the size of the gap is itself diagnostic. On an unsaturated
1.19.x cell it is about 15 ms - the apiserver and etcd round trip the histogram
does not see. A far larger gap means something else: queueing, on a cell driven
past its capacity, or more than one webhook invocation per Pod, which is what
1.18.2 does (it registers a mutate webhook for every ivpol, where 1.19.1 filters
to those needing it). In that case the histogram mean is per invocation, not per
admission, and the two columns are not comparable across versions - read
kyverno_admission_review_count against the iteration count before trusting it. k6 is picked as primary because it is what
the older published benchmarks used; the histogram is kept as its own column
rather than blended in, so neither number pretends to be the other. The
histogram's own p99 lands on Prometheus's default bucket boundaries far more
often than a real percentile would (2.485s, 4.975s, 9.95s recur across
unrelated cells), so it is bucket-quantised rather than a measured
percentile - treat "kyverno hist p99" as approximate for that reason, and
prefer k6's p99 when the two disagree.

A row is a (version, mode, shape) triple, where shape is images_per_pod,
registry_delay_ms, load_shape, reports_mode and policy_template - everything
that changes what a cell measures besides the version/mode under test.
Readings that share (version, mode) but differ in shape are never averaged
together: each distinct shape gets its own row, with the shape values printed
as columns, so a reader cannot mistake shape variance for noise. Also refuses
to average a group whose cells disagree on registry_flavor: a Referrers-
capable backend (zot) and a fallback-tag backend (distribution) produce
different exact call counts for the same policy, so averaging across them
would silently blend two different measurements into one number. That check
is also run once globally, across every reading regardless of group, since a
run that mixes backends end to end is worth flagging even where no single
(version, mode, shape) group happens to straddle both.

Usage:
  python3 version-table.py results/ladder
  python3 version-table.py results/ladder-run2 results/ladder-run3
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
import sys
from collections import defaultdict
from pathlib import Path

CELL = re.compile(r"^img(?P<images>\d+)-d(?P<delay>\d+)-(?P<cache>\w+)$")
ROUND = re.compile(r"^round(\d+)$")

# Request kinds delay-proxy.py classifies traffic into (registry/delay-proxy.py
# classify()). Broken out per-image here because 40-45% of "calls per image" is
# just /v2/ pings, and folding every kind into one number hides that.
CALL_KINDS = ("ping", "manifest", "blob", "referrers", "sig_tag")

# The full shape of a cell: everything besides (version, mode) that changes
# what is being measured. Two cells that differ in any of these are two
# different experiments and must never land in the same averaged row.
ShapeKey = tuple[str, str, str, str, str, str]


def parse_meta(path: Path) -> dict:
    return dict(
        line.split("=", 1) for line in path.read_text().splitlines() if "=" in line
    )


def as_float(meta: dict, key: str) -> float | None:
    v = meta.get(key, "").strip()
    if not v:
        return None
    try:
        return float(v)
    except ValueError:
        return None


def find_round(cell_dir: Path) -> str:
    """Nearest ancestor named round<N>, or '1' for a flat, round-less layout."""
    for parent in cell_dir.parents:
        m = ROUND.match(parent.name)
        if m:
            return m.group(1)
    return "1"


def read_cell(cell_dir: Path) -> dict | None:
    """One cell's figures, or None if any required input is missing."""
    meta_path = cell_dir / "meta.txt"
    stats_path = cell_dir / "registry-stats.json"
    iters_path = cell_dir / "iterations.txt"
    k6_path = cell_dir / "k6-summary.json"
    if not (meta_path.is_file() and stats_path.is_file() and iters_path.is_file() and k6_path.is_file()):
        return None

    meta = parse_meta(meta_path)
    version = meta.get("kyverno_version", "").strip()
    if not version:
        return None

    try:
        stats = json.loads(stats_path.read_text())
    except json.JSONDecodeError:
        return None
    try:
        iterations = int(iters_path.read_text().strip())
    except ValueError:
        return None
    images_per_pod = as_float(meta, "images_per_pod")
    total = stats.get("total")
    if not iterations or not images_per_pod or total is None:
        return None
    denom = iterations * images_per_pod
    calls_per_image = total / denom

    calls_by_kind = {}
    for kind in CALL_KINDS:
        count = stats.get(kind)
        calls_by_kind[kind] = (count / denom) if count is not None else None

    try:
        k6 = json.loads(k6_path.read_text())
    except json.JSONDecodeError:
        return None
    iteration = k6.get("metrics", {}).get("iteration_duration", {})
    k6_p50 = iteration.get("med")
    k6_p99 = iteration.get("p(99)")
    if k6_p50 is None or k6_p99 is None:
        return None

    # Kyverno's histogram is recorded in seconds; k6's own numbers are
    # milliseconds. Converting here keeps every latency column in the same
    # unit so the two are directly comparable in the printed table.
    hist_avg = as_float(meta, "kyverno_admission_review_duration_avg_sec")
    hist_p99 = as_float(meta, "kyverno_admission_review_duration_p99_sec")

    # Cache-mode is part of the shape too, but it is kept out of ShapeKey and
    # handled separately (cold_groups vs all_groups): the whole point of the
    # cold-vs-warm row below is to compare two cache_mode values for an
    # otherwise identical shape, which a key that included cache_mode would
    # prevent.
    shape: ShapeKey = (
        meta.get("images_per_pod", "").strip(),
        meta.get("registry_delay_ms", "").strip(),
        meta.get("load_shape", "").strip(),
        meta.get("reports_mode", "").strip(),
        meta.get("policy_template", "").strip(),
        # A floating tag like "main" is not a version. Rounds taken at
        # different commits must not average together, and the digest is the
        # only field that distinguishes them.
        meta.get("kyverno_image_id", "").strip(),
    )

    warm_evidence_path = cell_dir / "warm-cache-evidence.txt"
    warm_verdict = None
    if warm_evidence_path.is_file():
        wmeta = parse_meta(warm_evidence_path)
        warm_verdict = wmeta.get("verdict", "").strip() or None
    adm_count = as_float(meta, "kyverno_admission_review_count")
    rev_per_iter = (adm_count / iterations) if (adm_count and iterations) else None

    coverage_path = cell_dir / "verification-coverage-pct.txt"
    coverage_pct = None
    if coverage_path.is_file():
        try:
            coverage_pct = float(coverage_path.read_text().strip())
        except ValueError:
            coverage_pct = None

    try:
        iterations_count = int(iterations)
    except (TypeError, ValueError):
        iterations_count = 0

    return {
        "version": version,
        "mode": meta.get("mode", "unset").strip() or "unset",
        "cache_mode": meta.get("cache_mode", "off").strip() or "off",
        "registry_flavor": meta.get("registry_flavor", "").strip() or "distribution",
        "shape": shape,
        "round": find_round(cell_dir),
        "k6_p50_ms": float(k6_p50),
        "k6_p99_ms": float(k6_p99),
        "hist_mean_ms": hist_avg * 1000 if hist_avg is not None else None,
        "hist_p99_ms": hist_p99 * 1000 if hist_p99 is not None else None,
        "calls_per_image": calls_per_image,
        "calls_by_kind": calls_by_kind,
        "cpu_peak": as_float(meta, "cpu_millicores_peak"),
        "cpu_mean": as_float(meta, "cpu_millicores_mean"),
        "mem_peak": as_float(meta, "memory_mib_peak"),
        "mem_mean": as_float(meta, "memory_mib_mean"),
        "iterations": iterations_count,
        "warm_verdict": warm_verdict,
        "coverage_pct": coverage_pct,
        "rev_per_iter": rev_per_iter,
    }


def collect(roots: list[Path]) -> list[dict]:
    readings = []
    for root in roots:
        for cell_dir in sorted(root.rglob("img*-d*")):
            if not cell_dir.is_dir() or not CELL.match(cell_dir.name):
                continue
            reading = read_cell(cell_dir)
            if reading is not None:
                readings.append(reading)
    return readings


def mean(values: list[float]) -> float | None:
    usable = [v for v in values if v is not None]
    return statistics.fmean(usable) if usable else None


def spread(values: list[float]) -> float | None:
    """Sample stdev, or None when there's nothing to spread (n<2)."""
    usable = [v for v in values if v is not None]
    return statistics.stdev(usable) if len(usable) >= 2 else None


def fmt(v: float | None, nd: int = 1) -> str:
    return "" if v is None else f"{v:.{nd}f}"


def fmt_spread(values: list[float], nd: int = 1) -> str:
    m, s = mean(values), spread(values)
    if m is None:
        return ""
    return f"{m:.{nd}f}" if s is None else f"{m:.{nd}f}±{s:.{nd}f}"


def shape_cols(shape: ShapeKey) -> str:
    images, delay, load_shape, reports, policy, image_id = shape
    # The digest is shown only in its short form, and only when present: it
    # matters for "main", where the version string alone is ambiguous, and is
    # noise for a released tag.
    digest = f" img_id={image_id.rsplit(':', 1)[-1][:12]}" if image_id else ""
    return (f"img={images or '?'} d={delay or '?'}ms load={load_shape or '?'} "
            f"reports={reports or '?'} policy={policy or '?'}{digest}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("roots", nargs="+", type=Path)
    args = parser.parse_args()

    for root in args.roots:
        if not root.is_dir():
            sys.exit(f"not a directory: {root}")

    readings = collect(args.roots)
    if not readings:
        sys.exit(f"no completed cells under {', '.join(str(r) for r in args.roots)}")

    # Grouping key: version, mode, and the full shape. A cell's cache_mode is
    # deliberately not part of this key - see read_cell's shape comment - so
    # cold_groups below is a cache_mode=off filter over the same key, not a
    # separate axis of grouping.
    GroupKey = tuple[str, str, ShapeKey]
    all_groups: dict[GroupKey, list[dict]] = defaultdict(list)
    cold_groups: dict[GroupKey, list[dict]] = defaultdict(list)
    for r in readings:
        key = (r["version"], r["mode"], r["shape"])
        all_groups[key].append(r)
        if r["cache_mode"] == "off":
            cold_groups[key].append(r)

    print("ivpol version comparison")
    print(f"roots: {', '.join(str(r) for r in args.roots)}")
    print()
    print("Primary latency column is k6's client-side iteration_duration (what the")
    print("older published benchmarks used); Kyverno's own admission-review histogram")
    print("is shown separately because the two disagree, sometimes by up to 3x, and its")
    print("p99 is bucket-quantised rather than a measured percentile.")
    print()
    print("Each row is a distinct (version, mode, shape) combination. Rows that share")
    print("version and mode but differ in shape are never averaged together.")
    print()

    header = (
        f"{'version':<12} {'mode':<14} {'shape':<48} {'rounds':>6} "
        f"{'n':>6} {'k6 p50 ms':>14} {'k6 p99 ms':>14} "
        f"{'hist mean ms':>16} {'hist p99 ms':>14} {'rev/iter':>9} "
        f"{'calls/img':>12} {'cov%':>6} "
        f"{'cpu peak/mean':>16} {'mem MiB peak/mean':>20}"
    )
    print(header)
    print("-" * len(header))

    warnings = []
    mixed_flavor: set[GroupKey] = set()
    for key in sorted(all_groups):
        flavors = {e["registry_flavor"] for e in all_groups[key]}
        if len(flavors) > 1:
            mixed_flavor.add(key)
            warnings.append(
                f"{key[0]}/{key[1]} [{shape_cols(key[2])}]: mixed registry_flavor "
                f"{sorted(flavors)} — refusing to average (a different backend shifts "
                f"call counts by ~1 request/image)"
            )

    # Global flavor check: unlike the per-group check above, this looks at
    # every reading at once regardless of version/mode/shape. A run that mixed
    # backends end to end (e.g. the registry was swapped mid-run) is worth
    # flagging even when no single group happens to straddle both flavors.
    all_flavors = {r["registry_flavor"] for r in readings}
    if len(all_flavors) > 1:
        warnings.append(
            f"registry_flavor varies across the whole run: {sorted(all_flavors)} — "
            f"some groups may be comparing different backends even where no single "
            f"group mixes them"
        )

    low_iter_warnings = []
    for key in sorted(cold_groups):
        if key in mixed_flavor:
            continue
        entries = cold_groups[key]
        version, mode, shape = key

        rounds = sorted({e["round"] for e in entries}, key=int)
        cpu_peak = max((e["cpu_peak"] for e in entries if e["cpu_peak"] is not None), default=None)
        mem_peak = max((e["mem_peak"] for e in entries if e["mem_peak"] is not None), default=None)
        cpu_mean = mean([e["cpu_mean"] for e in entries])
        mem_mean = mean([e["mem_mean"] for e in entries])

        # A p99 computed over fewer than 100 iterations is, at best, the
        # maximum of a handful of samples rather than a percentile (a
        # concurrency-1 cell with 7 iterations has no 99th percentile worth
        # the name). Flagged per contributing cell, not silently averaged in.
        iter_counts = [e["iterations"] for e in entries if e["iterations"]]
        min_iters = min(iter_counts) if iter_counts else 0
        # Blanked rather than printed: a number on the page gets quoted, and a
        # footnote twenty lines below does not travel with it.
        p99_usable = bool(min_iters) and min_iters >= 100

        # If Kyverno counted more admission reviews than k6 ran iterations, its
        # histogram is averaging per invocation while k6 averages per admission,
        # and the two columns are not the same quantity. 1.18.2 does this: it
        # registers a mutate webhook for every ivpol where 1.19.1 filters to
        # those that need one. Mark it rather than letting the number pass as
        # comparable.
        revs = [e["rev_per_iter"] for e in entries if e["rev_per_iter"] is not None]
        rev_ratio = mean(revs) if revs else None
        hist_mean_col = fmt_spread([e["hist_mean_ms"] for e in entries])
        hist_suffix = ""
        if rev_ratio is not None and rev_ratio > 1.15:
            # More reviews than iterations: the mean is per invocation.
            hist_suffix = " per-inv"
        elif rev_ratio is not None and rev_ratio < 0.85:
            # Fewer: the scrape window missed samples. Not per-invocation -
            # the number is simply built on an undercount.
            hist_suffix = " undercount"
        hist_mean_col = f"{hist_mean_col}{hist_suffix}"
        for e in entries:
            if e["iterations"] and e["iterations"] < 100:
                low_iter_warnings.append(
                    f"{version}/{mode} [{shape_cols(shape)}] round {e['round']}: only "
                    f"{e['iterations']} iterations — p99 here is the max of a handful of "
                    f"samples, not a percentile"
                )

        print(
            f"{version:<12} {mode:<14} {shape_cols(shape):<48} {len(rounds):>6} "
            f"{min_iters if min_iters else '-':>6} "
            f"{fmt_spread([e['k6_p50_ms'] for e in entries]):>14} "
            f"{(fmt_spread([e['k6_p99_ms'] for e in entries]) if p99_usable else 'n<100'):>14} "
            f"{hist_mean_col:>16} "
            f"{((fmt_spread([e['hist_p99_ms'] for e in entries]) + hist_suffix) if p99_usable else 'n<100'):>14} "
            f"{(fmt(mean([e['rev_per_iter'] for e in entries if e['rev_per_iter'] is not None]), 2) if any(e['rev_per_iter'] is not None for e in entries) else '-'):>9} "
            f"{fmt_spread([e['calls_per_image'] for e in entries], 3):>12} "
            f"{(fmt_spread([e['coverage_pct'] for e in entries if e['coverage_pct'] is not None], 0) if any(e['coverage_pct'] is not None for e in entries) else '-'):>6} "
            f"{fmt(cpu_peak, 0) + '/' + fmt(cpu_mean, 0):>16} "
            f"{fmt(mem_peak, 0) + '/' + fmt(mem_mean, 0):>20}"
        )

    # Registry calls per image, broken out by request kind. ~40-45% of the
    # undifferentiated total above is /v2/ pings; this is the number that
    # actually says what the admission path did.
    kind_rows = []
    for key in sorted(cold_groups):
        if key in mixed_flavor:
            continue
        entries = cold_groups[key]
        version, mode, shape = key
        kind_rows.append((version, mode, shape, entries))

    if kind_rows:
        print()
        print("Registry calls per image by kind (cold):")
        kind_header = (
            f"{'version':<12} {'mode':<14} {'shape':<48} "
            + " ".join(f"{k:>10}" for k in CALL_KINDS)
        )
        print(kind_header)
        for version, mode, shape, entries in kind_rows:
            cells = " ".join(
                f"{fmt_spread([e['calls_by_kind'][k] for e in entries], 3):>10}"
                for k in CALL_KINDS
            )
            print(f"{version:<12} {mode:<14} {shape_cols(shape):<48} {cells}")

    # Cache cold vs warm is its own row per (version, mode, shape): a single
    # number above would be an average across cache states that nobody asked
    # for. Drawn from all_groups, not cold_groups, since this row's whole
    # point is the warm side cold_groups deliberately excludes.
    #
    # 1.18.2's ivpol engine has zero ivCache references even though it accepts
    # --imageVerifyCacheEnabled - the flag is parsed, nothing reads it. A warm
    # number for 1.18.x would therefore imply a cache that is not there; this
    # prints an explicit n/a instead, with the evidence it is based on.
    cache_rows = []
    for key, entries in sorted(all_groups.items()):
        if key in mixed_flavor:
            continue
        version, mode, shape = key
        cold = [e["calls_per_image"] for e in entries if e["cache_mode"] == "off"]
        warm_entries = [e for e in entries if e["cache_mode"] == "warm"]
        warm = [e["calls_per_image"] for e in warm_entries]
        if cold or warm:
            cache_rows.append((version, mode, shape, cold, warm, warm_entries))

    if cache_rows:
        print()
        print("Registry calls per image, cache cold vs warm:")
        print(f"{'version':<12} {'mode':<14} {'shape':<48} {'cold':>14} {'warm':>18} {'warm evidence':>16}")
        for version, mode, shape, cold, warm, warm_entries in cache_rows:
            no_cache_version = version.startswith("1.18")
            if no_cache_version:
                warm_col = "n/a (no cache in this version)"
            else:
                warm_col = fmt_spread(warm, 3)
            verdicts = sorted({e["warm_verdict"] for e in warm_entries if e["warm_verdict"]})
            evidence = ",".join(verdicts) if verdicts else ""
            print(
                f"{version:<12} {mode:<14} {shape_cols(shape):<48} "
                f"{fmt_spread(cold, 3):>14} {warm_col:>18} {evidence:>16}"
            )

    # Uneven rounds across groups is a correctness problem, not cosmetic -
    # mirrors ivpol-report.py's treatment of the same failure mode. Judged on
    # the cold groups: those are what the headline table actually printed.
    round_counts = {
        k: len({e["round"] for e in v}) for k, v in cold_groups.items() if k not in mixed_flavor
    }
    if round_counts:
        expected = max(round_counts.values())
        uneven = [
            f"{v}/{m} [{shape_cols(s)}] ({c}/{expected})"
            for (v, m, s), c in round_counts.items() if c != expected
        ]
        if uneven:
            print()
            print(f"WARNING: uneven rounds (expected {expected}); these are not directly comparable:")
            for item in uneven:
                print(f"  {item}")

    if low_iter_warnings:
        print()
        print("WARNING: fewer than 100 iterations (p99 is not a percentile here):")
        for item in low_iter_warnings:
            print(f"  {item}")

    if warnings:
        print()
        print("WARNING:")
        for w in warnings:
            print(f"  {w}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
