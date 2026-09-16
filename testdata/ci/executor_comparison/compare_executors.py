#!/usr/bin/env python3
"""Compare output equivalence between Craftmake and Snakemake execution runs."""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path


def hash_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--craftmake-dir", required=True, type=Path)
    parser.add_argument("--snakemake-dir", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    parser.add_argument(
        "--craftmake-wall-seconds",
        type=float,
        default=None,
        help="wall-clock seconds for the Craftmake run",
    )
    parser.add_argument(
        "--snakemake-wall-seconds",
        type=float,
        default=None,
        help="wall-clock seconds for the Snakemake run",
    )
    args = parser.parse_args()

    required_artifacts = [
        "output/samples/sampleA.tsv",
        "output/samples/sampleB.tsv",
        "output/summary.tsv",
    ]

    report = {
        "schema_version": "otter.executor-comparison/v1",
        "craftmake_dir": str(args.craftmake_dir),
        "snakemake_dir": str(args.snakemake_dir),
        "equal": True,
        "artifacts": [],
    }

    for rel in required_artifacts:
        cm_path = args.craftmake_dir / rel
        sm_path = args.snakemake_dir / rel

        if not cm_path.exists():
            report["equal"] = False
            report["artifacts"].append({"path": rel, "status": "missing_in_craftmake"})
            continue
        if not sm_path.exists():
            report["equal"] = False
            report["artifacts"].append({"path": rel, "status": "missing_in_snakemake"})
            continue

        cm_hash = hash_file(cm_path)
        sm_hash = hash_file(sm_path)
        matches = cm_hash == sm_hash
        if not matches:
            report["equal"] = False

        report["artifacts"].append({
            "path": rel,
            "craftmake_sha256": cm_hash,
            "snakemake_sha256": sm_hash,
            "matches": matches,
            "bytes": cm_path.stat().st_size,
        })

    # Runtime metrics. These are recorded so a reviewer can see the execution-cost relationship,
    # but they are deliberately not asserted: a single CI sample on a shared runner cannot support
    # a performance claim, and enforcing a ratio here would turn natural runner variance into a
    # spurious failure. The comparison states the measured pair and the derived ratio only.
    craftmake_wall_seconds = args.craftmake_wall_seconds
    snakemake_wall_seconds = args.snakemake_wall_seconds
    runtime: dict[str, object] = {
        "measured": (
            craftmake_wall_seconds is not None and snakemake_wall_seconds is not None
        ),
        "unit": "seconds",
        "craftmake_wall_seconds": craftmake_wall_seconds,
        "snakemake_wall_seconds": snakemake_wall_seconds,
        "delta_seconds": None,
        "ratio_craftmake_over_snakemake": None,
        "faster_executor": None,
        "asserted": False,
    }
    if runtime["measured"]:
        assert craftmake_wall_seconds is not None and snakemake_wall_seconds is not None
        runtime["delta_seconds"] = round(craftmake_wall_seconds - snakemake_wall_seconds, 3)
        if snakemake_wall_seconds > 0:
            runtime["ratio_craftmake_over_snakemake"] = round(
                craftmake_wall_seconds / snakemake_wall_seconds, 4
            )
        runtime["faster_executor"] = (
            "craftmake" if craftmake_wall_seconds < snakemake_wall_seconds else "snakemake"
        )
    report["runtime"] = runtime

    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")

    matched_artifacts = sum(1 for entry in report["artifacts"] if entry.get("matches"))
    if report["equal"]:
        print(
            f"Executor parity verified: all {len(required_artifacts)} artifacts match byte-for-byte."
        )
    else:
        print(
            f"Executor parity failure: {matched_artifacts}/{len(required_artifacts)} "
            "artifacts match byte-for-byte.",
            file=sys.stderr,
        )
    if runtime["measured"]:
        print(
            f"Runtime (single CI sample, not asserted): craftmake "
            f"{craftmake_wall_seconds:.3f}s vs snakemake {snakemake_wall_seconds:.3f}s; "
            f"ratio {runtime['ratio_craftmake_over_snakemake']}; "
            f"faster: {runtime['faster_executor']}"
        )
    else:
        print("Runtime: not measured for this run.")

    return 0 if report["equal"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
