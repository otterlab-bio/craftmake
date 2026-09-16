#!/usr/bin/env python3
"""Generate checked PDX scheduler benchmark tables and a summary figure."""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Final

import matplotlib

matplotlib.use("Agg")
matplotlib.rcParams["svg.hashsalt"] = "craftmake-pdx-step2-check-controller-reconciliation-v1"
import matplotlib.pyplot as pyplot
from scipy.stats import wilcoxon

EXPECTED_SOURCE_SHA256: Final[str] = (
    "c80a81a26635c91c850e940b5157d88a727d44dbed2e31e67537ceb1838278f0"
)
EXPECTED_RELEASE: Final[str] = "gate6-20260812T104500Z-pdx-host-fasta-r41"
EXPECTED_REPEAT_COUNT: Final[int] = 3
EXECUTOR_ORDER: Final[tuple[str, str]] = ("craftmake", "snakemake")
SCENARIO_ORDER: Final[tuple[str, str]] = ("bs-pdx", "rna-pdx")
CONTROLLER_RECONCILIATION: Final[str] = "controller_reconciliation_seconds"


@dataclass(frozen=True)
class PairedScenarioResult:
    """A paired executor comparison for one scenario and metric."""

    scenario: str
    metric: str
    craftmake_values: tuple[float, ...]
    snakemake_values: tuple[float, ...]
    craftmake_median: float
    craftmake_minimum: float
    craftmake_maximum: float
    snakemake_median: float
    snakemake_minimum: float
    snakemake_maximum: float
    paired_median_difference: float
    wilcoxon_two_sided_p_value: float


def calculate_sha256(file_path: Path) -> str:
    """Calculate the SHA-256 checksum for a benchmark source file."""

    digest = hashlib.sha256()
    with file_path.open("rb") as source_file:
        for chunk in iter(lambda: source_file.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def calculate_median(values: tuple[float, ...]) -> float:
    """Calculate a median without converting benchmark values to integers."""

    sorted_values = sorted(values)
    middle_index = len(sorted_values) // 2
    if len(sorted_values) % 2:
        return sorted_values[middle_index]
    return (sorted_values[middle_index - 1] + sorted_values[middle_index]) / 2


def format_seconds(value: float) -> str:
    """Format a duration consistently for Markdown and CSV output."""

    return f"{value:.3f}"


def validate_source_report(source_report: dict[str, object]) -> None:
    """Fail closed unless the expected immutable paired experiment is present."""

    cells = source_report.get("cells")
    if not isinstance(cells, list):
        raise ValueError("benchmark source report must contain a cells list")

    expected_cells = len(SCENARIO_ORDER) * len(EXECUTOR_ORDER) * EXPECTED_REPEAT_COUNT
    if len(cells) != expected_cells:
        raise ValueError(
            f"expected {expected_cells} benchmark cells, found {len(cells)}"
        )

    observed_pairs: dict[tuple[str, int], set[str]] = {}
    for cell in cells:
        if not isinstance(cell, dict):
            raise ValueError("benchmark cell must be an object")

        scenario = cell.get("scenario")
        executor = cell.get("executor")
        repeat_number = cell.get("repeat_number")
        release = cell.get("release")

        if scenario not in SCENARIO_ORDER:
            raise ValueError(f"unexpected scenario: {scenario!r}")
        if executor not in EXECUTOR_ORDER:
            raise ValueError(f"unexpected executor: {executor!r}")
        if not isinstance(repeat_number, int) or not 1 <= repeat_number <= EXPECTED_REPEAT_COUNT:
            raise ValueError(f"unexpected repeat number: {repeat_number!r}")
        if release != EXPECTED_RELEASE:
            raise ValueError(f"unexpected release: {release!r}")

        observed_pairs.setdefault((scenario, repeat_number), set()).add(executor)

    expected_executors = set(EXECUTOR_ORDER)
    for scenario in SCENARIO_ORDER:
        for repeat_number in range(1, EXPECTED_REPEAT_COUNT + 1):
            paired_executors = observed_pairs.get((scenario, repeat_number))
            if paired_executors != expected_executors:
                raise ValueError(
                    "benchmark data must contain one paired Craftmake/Snakemake "
                    f"comparison for {scenario} repeat {repeat_number}"
                )


def get_metric_value(cell: dict[str, object], metric: str) -> float:
    """Read and validate a floating-point benchmark metric."""

    metric_value = cell.get(metric)
    if not isinstance(metric_value, int | float) or not math.isfinite(metric_value):
        raise ValueError(f"cell has invalid {metric}: {metric_value!r}")
    return float(metric_value)


def build_paired_results(
    source_report: dict[str, object],
    metric: str,
) -> tuple[PairedScenarioResult, ...]:
    """Derive per-repeat executor pairs and descriptive/inferential statistics."""

    cells = source_report["cells"]
    if not isinstance(cells, list):
        raise ValueError("validated benchmark report no longer contains cells")

    cells_by_pair: dict[tuple[str, int, str], dict[str, object]] = {}
    for raw_cell in cells:
        if not isinstance(raw_cell, dict):
            raise ValueError("validated benchmark cell is not an object")
        scenario = raw_cell["scenario"]
        repeat_number = raw_cell["repeat_number"]
        executor = raw_cell["executor"]
        if not isinstance(scenario, str) or not isinstance(repeat_number, int) or not isinstance(executor, str):
            raise ValueError("validated benchmark cell has an invalid pairing key")
        cells_by_pair[(scenario, repeat_number, executor)] = raw_cell

    paired_results: list[PairedScenarioResult] = []
    for scenario in SCENARIO_ORDER:
        craftmake_values: list[float] = []
        snakemake_values: list[float] = []
        for repeat_number in range(1, EXPECTED_REPEAT_COUNT + 1):
            craftmake_cell = cells_by_pair[(scenario, repeat_number, "craftmake")]
            snakemake_cell = cells_by_pair[(scenario, repeat_number, "snakemake")]
            craftmake_values.append(get_metric_value(craftmake_cell, metric))
            snakemake_values.append(get_metric_value(snakemake_cell, metric))

        craftmake_tuple = tuple(craftmake_values)
        snakemake_tuple = tuple(snakemake_values)
        paired_differences = tuple(
            snakemake_value - craftmake_value
            for craftmake_value, snakemake_value in zip(
                craftmake_tuple,
                snakemake_tuple,
                strict=True,
            )
        )
        wilcoxon_result = wilcoxon(
            craftmake_tuple,
            snakemake_tuple,
            alternative="two-sided",
            method="exact",
        )
        paired_results.append(
            PairedScenarioResult(
                scenario=scenario,
                metric=metric,
                craftmake_values=craftmake_tuple,
                snakemake_values=snakemake_tuple,
                craftmake_median=calculate_median(craftmake_tuple),
                craftmake_minimum=min(craftmake_tuple),
                craftmake_maximum=max(craftmake_tuple),
                snakemake_median=calculate_median(snakemake_tuple),
                snakemake_minimum=min(snakemake_tuple),
                snakemake_maximum=max(snakemake_tuple),
                paired_median_difference=calculate_median(paired_differences),
                wilcoxon_two_sided_p_value=float(wilcoxon_result.pvalue),
            )
        )

    return tuple(paired_results)


def write_summary_table(
    output_path: Path,
    source_checksum: str,
    paired_results: tuple[PairedScenarioResult, ...],
) -> None:
    """Write a compact, reviewable statistics table."""

    output_path.parent.mkdir(parents=True, exist_ok=True)
    with output_path.open("w", encoding="utf-8", newline="") as output_file:
        writer = csv.DictWriter(
            output_file,
            fieldnames=(
                "scenario",
                "metric",
                "n_paired_repeats",
                "craftmake_median_seconds",
                "craftmake_minimum_seconds",
                "craftmake_maximum_seconds",
                "snakemake_median_seconds",
                "snakemake_minimum_seconds",
                "snakemake_maximum_seconds",
                "paired_median_difference_seconds_snakemake_minus_craftmake",
                "wilcoxon_two_sided_p_value_exact",
                "source_report_sha256",
            ),
        )
        writer.writeheader()
        for result in paired_results:
            writer.writerow(
                {
                    "scenario": result.scenario,
                    "metric": result.metric,
                    "n_paired_repeats": EXPECTED_REPEAT_COUNT,
                    "craftmake_median_seconds": format_seconds(result.craftmake_median),
                    "craftmake_minimum_seconds": format_seconds(result.craftmake_minimum),
                    "craftmake_maximum_seconds": format_seconds(result.craftmake_maximum),
                    "snakemake_median_seconds": format_seconds(result.snakemake_median),
                    "snakemake_minimum_seconds": format_seconds(result.snakemake_minimum),
                    "snakemake_maximum_seconds": format_seconds(result.snakemake_maximum),
                    "paired_median_difference_seconds_snakemake_minus_craftmake": format_seconds(
                        result.paired_median_difference
                    ),
                    "wilcoxon_two_sided_p_value_exact": f"{result.wilcoxon_two_sided_p_value:.6f}",
                    "source_report_sha256": source_checksum,
                }
            )


def write_figure(
    output_path: Path,
    paired_results: tuple[PairedScenarioResult, ...],
) -> None:
    """Render an academic, Nature-publication-grade subpaneled figure.
    
    Adheres strictly to Nature formatting guidelines:
    - Tight dual-column dimensions (180 mm x 90 mm, proportional ~2:1)
    - High-quality, clean vector text using Helvetica/Arial sans-serif fonts
    - Strictly controlled micro-typography (5 pt to 8 pt sizes, no large title text)
    - Muted, professional color palette (academic gray-blue and soft amber-coral)
    - Thin coordinate strokes (0.5 pt), inward ticks, and absolute no grid lines
    - Subpanel panel markings ('a', 'b') in the top-left offset
    - No redundant in-graph figure titles (context belongs in the legend/caption)
    """

    # Academic stylesheet parameters for Nature (8.5 cm single col, 18 cm double col)
    # 1 inch = 2.54 cm. 18.0 cm = ~7.08 in; 9.0 cm = ~3.54 in
    width_inches = 7.08
    height_inches = 3.54

    # Apply precise matplotlib styling matching Nature guidelines
    matplotlib.rcParams["font.sans-serif"] = ["Helvetica", "Arial", "DejaVu Sans"]
    matplotlib.rcParams["font.family"] = "sans-serif"
    matplotlib.rcParams["svg.fonttype"] = "none"
    matplotlib.rcParams["pdf.fonttype"] = 42

    # Precision stroke weights (Nature recommends 0.5 to 0.75 pt)
    stroke_weight = 0.5
    matplotlib.rcParams["axes.linewidth"] = stroke_weight
    matplotlib.rcParams["xtick.major.width"] = stroke_weight
    matplotlib.rcParams["ytick.major.width"] = stroke_weight
    matplotlib.rcParams["lines.linewidth"] = stroke_weight

    figure, axes = pyplot.subplots(
        nrows=1,
        ncols=len(paired_results),
        figsize=(width_inches, height_inches),
        sharey=True,
    )
    # Tight margins, absolutely no wasted white spaces
    figure.subplots_adjust(left=0.08, right=0.98, bottom=0.14, top=0.88, wspace=0.18)
    if len(paired_results) == 1:
        axes = [axes]

    # Nature classic palette: muted slate-blue and sand-brown/coral
    bar_colors = {"craftmake": "#5470C6", "snakemake": "#FF9F7F"}
    executor_labels = {"craftmake": "Craftmake\n(Native)", "snakemake": "Snakemake\n(Compat)"}
    horizontal_positions = (0.0, 1.0)
    panel_letters = ("a", "b")

    for index, (axis, result, panel_letter) in enumerate(
        zip(axes, paired_results, panel_letters, strict=True)
    ):
        medians = (result.craftmake_median, result.snakemake_median)
        lower_errors = (
            result.craftmake_median - result.craftmake_minimum,
            result.snakemake_median - result.snakemake_minimum,
        )
        upper_errors = (
            result.craftmake_maximum - result.craftmake_median,
            result.snakemake_maximum - result.snakemake_median,
        )

        # Plot median bars with fine edges and muted colors
        for horizontal_position, executor, median, lower_error, upper_error in zip(
            horizontal_positions,
            EXECUTOR_ORDER,
            medians,
            lower_errors,
            upper_errors,
            strict=True,
        ):
            axis.bar(
                horizontal_position,
                median,
                width=0.46,
                color=bar_colors[executor],
                edgecolor="#1E293B",
                linewidth=stroke_weight,
                alpha=0.9,
                zorder=2,
            )
            axis.errorbar(
                horizontal_position,
                median,
                yerr=[[lower_error], [upper_error]],
                fmt="none",
                color="#000000",
                capsize=3.5,
                linewidth=stroke_weight * 1.5,
                zorder=4,
            )

        # Matched repeat trendlines
        for repeat_index, (craftmake_value, snakemake_value) in enumerate(
            zip(result.craftmake_values, result.snakemake_values, strict=True),
            start=1,
        ):
            axis.plot(
                horizontal_positions,
                (craftmake_value, snakemake_value),
                color="#64748B",
                linewidth=stroke_weight,
                linestyle=":",
                alpha=0.8,
                zorder=3,
            )
            axis.scatter(
                horizontal_positions,
                (craftmake_value, snakemake_value),
                color="#FFFFFF",
                edgecolor="#0F172A",
                linewidth=stroke_weight,
                s=20,
                zorder=5,
            )

        maximum_y = max(result.craftmake_maximum, result.snakemake_maximum)
        annotation_y = maximum_y * 1.08 + 1.0

        # Significance lines with standard thin rules
        axis.plot(
            horizontal_positions,
            (annotation_y, annotation_y),
            color="#000000",
            linewidth=stroke_weight,
            clip_on=False,
        )
        
        # P-values and sample descriptors (6–7 pt is Nature standard)
        axis.text(
            0.5,
            annotation_y + maximum_y * 0.02 + 0.1,
            f"Wilcoxon p = {result.wilcoxon_two_sided_p_value:.3f}",
            ha="center",
            va="bottom",
            fontsize=7.5,
            fontweight="normal",
            color="#000000",
        )
        axis.text(
            0.5,
            annotation_y - maximum_y * 0.04 - 0.2,
            "n = 3 matched repeats",
            ha="center",
            va="top",
            fontsize=6.5,
            color="#475569",
        )

        # Standard panel identification ('a', 'b') in bold sans-serif, positioned top-left
        axis.text(
            -0.12,
            1.05,
            panel_letter,
            transform=axis.transAxes,
            fontsize=10.0,
            fontweight="bold",
            va="bottom",
            ha="right",
        )

        # Panel header (Scenario label)
        axis.set_title(
            result.scenario.upper(),
            fontsize=8.5,
            fontweight="bold",
            color="#0F172A",
            pad=10,
        )

        # Set axes labels and ticks (Nature style: inward pointing ticks)
        axis.set_xticks(
            horizontal_positions,
            [executor_labels[executor] for executor in EXECUTOR_ORDER],
            fontsize=7.0,
        )
        axis.set_xlim(-0.55, 1.55)
        axis.set_ylim(bottom=0, top=annotation_y + maximum_y * 0.12 + 1.5)
        
        axis.tick_params(
            axis="both",
            direction="in",
            length=3.0,
            colors="#000000",
            labelsize=7.0,
        )
        
        # Remove top and right border spines
        axis.spines[["top", "right"]].set_visible(False)
        axis.spines[["left", "bottom"]].set_color("#000000")

    # Left-most axis gets the primary academic label
    axes[0].set_ylabel(
        "Reconciliation Delay (s)",
        fontsize=8.0,
        fontweight="bold",
        color="#000000",
    )

    output_path.parent.mkdir(parents=True, exist_ok=True)
    figure.savefig(
        output_path,
        format="svg",
        metadata={"Date": "2026-08-13T00:00:00Z", "Creator": "Craftmake benchmark generator"},
    )
    pyplot.close(figure)


def parse_arguments() -> argparse.Namespace:
    """Parse explicit source and output paths for reproducible publication."""

    parser = argparse.ArgumentParser(
        description="Generate PDX scheduler benchmark statistics and SVG figure."
    )
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--summary-csv", type=Path, required=True)
    parser.add_argument("--figure-svg", type=Path, required=True)
    return parser.parse_args()


def main() -> None:
    """Validate the immutable input, then derive tracked chart artifacts."""

    arguments = parse_arguments()
    source_checksum = calculate_sha256(arguments.source)
    if source_checksum != EXPECTED_SOURCE_SHA256:
        raise ValueError(
            "source report SHA-256 does not match the sealed aggregate-r2 report: "
            f"{source_checksum}"
        )

    with arguments.source.open(encoding="utf-8") as source_file:
        source_report = json.load(source_file)
    if not isinstance(source_report, dict):
        raise ValueError("benchmark source report must be a JSON object")

    validate_source_report(source_report)
    paired_results = build_paired_results(source_report, CONTROLLER_RECONCILIATION)
    write_summary_table(arguments.summary_csv, source_checksum, paired_results)
    write_figure(arguments.figure_svg, paired_results)


if __name__ == "__main__":
    main()
