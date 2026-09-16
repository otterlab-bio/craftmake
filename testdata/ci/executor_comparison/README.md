# Craftmake vs Snakemake Executor Comparison Fixture

This fixture establishes an executor parity test where the executor is the only differing
variable.

## Structure

- `workflow.yaml`: Craftmake standalone workflow definition with sample-level and global aggregation jobs.
- `Snakefile`: Snakemake counterpart with identical rules, wildcards, shell commands, and output targets.
- `config.yaml`: Shared configuration with sample IDs `sampleA` and `sampleB`.
- `compare_executors.py`: Verifies byte-for-byte SHA-256 equivalence of all produced artifacts.

## Invariant

Both executors:
1. Process the exact same samples under identical parameters.
2. Execute on the local backend with controlled parallelism (`cores: 2`).
3. Produce `output/samples/{sampleA,sampleB}.tsv` and `output/summary.tsv`.
4. Snakemake runs strictly through `enva run --name otter-snakemake -- snakemake ...`; no direct conda/mamba actions are used.
