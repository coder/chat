# scripts

## Overview

This directory holds `clawpatch-autofix.sh`, a Bash driver behind the
`mise run clawpatch:autofix` task. The script asks Clawpatch for the next open
finding, runs `clawpatch fix` until Clawpatch revalidates the finding as
`fixed`, runs two Codex cleanup passes (`$simplify` and `$deslop`), checks the
finding again after each pass, and then asks Codex to commit the result.

CAUTION: The script runs Codex with `--dangerously-bypass-approvals-and-sandbox`
and tells Codex to "commit everything". It does not check for a clean working
tree first, so uncommitted changes that exist before the run go into the commit.

## Interface

### Entry points

- `mise run clawpatch:autofix` runs `scripts/clawpatch-autofix.sh` with no
  arguments. Its description is "Fix Clawpatch findings, run Codex cleanup
  passes, and commit". [mise.toml:tasks."clawpatch:autofix"]
- `scripts/clawpatch-autofix.sh` takes no positional arguments and no flags.
  The top level never reads `$@`. [clawpatch-autofix.sh:190-197]

### Environment variables

| Variable | Default | Effect | Source |
| --- | --- | --- | --- |
| `ROOT` | `$(git rev-parse --show-toplevel)` | Repository root. Passed to `clawpatch --root`, `codex exec -C`, and `git -C`. | [clawpatch-autofix.sh:ROOT] |
| `MAX_FINDINGS` | `1` | Maximum number of findings processed in one run. | [clawpatch-autofix.sh:MAX_FINDINGS] |
| `MAX_FIX_ATTEMPTS` | `3` | Maximum `clawpatch fix` attempts per call to `fix_until_fixed`. | [clawpatch-autofix.sh:MAX_FIX_ATTEMPTS] |
| `MAX_HYGIENE_ROUNDS` | `2` | Maximum fix, simplify, and deslop rounds per finding. | [clawpatch-autofix.sh:MAX_HYGIENE_ROUNDS] |
| `CODEX_MODEL` | unset | When set, the script adds `--model "$CODEX_MODEL"` to every Codex call. | [clawpatch-autofix.sh:codex_one_shot] |

### Exit codes

- `0`: `next_finding` returned no ID. The script prints `no open findings`.
  [clawpatch-autofix.sh:192-195]
- `0`: the main loop finished `MAX_FINDINGS` findings.
  [clawpatch-autofix.sh:190-197]
- Non-zero: `fix_until_fixed`, `process_finding`, or `commit_current_changes`
  returned `1`, or another unguarded command failed under `set -Eeuo pipefail`.
  [clawpatch-autofix.sh:2] [clawpatch-autofix.sh:34]
  [clawpatch-autofix.sh:154] [clawpatch-autofix.sh:187]

### Functions

- `claw`: runs `clawpatch --root "$ROOT" "$@"`. [clawpatch-autofix.sh:claw]
- `codex_one_shot <prompt>`: runs `codex exec -C "$ROOT"` with
  `--dangerously-bypass-approvals-and-sandbox`, an optional
  `--model "$CODEX_MODEL"`, and the prompt. Stdin comes from `/dev/null`.
  [clawpatch-autofix.sh:codex_one_shot]
- `commit_current_changes`: sends Codex the prompt "commit everything to git.
  Do not push or open a pull request." Returns `1` if `git status --porcelain`
  still shows changes. [clawpatch-autofix.sh:commit_current_changes]
- `finding_id`: reads JSON on stdin and prints `.finding.id // .id`.
  [clawpatch-autofix.sh:finding_id]
- `finding_status`: reads JSON on stdin and prints
  `.outcome // .status // .finding.status`. [clawpatch-autofix.sh:finding_status]
- `finding_details_file <id>`: builds or fetches the finding details JSON once
  per run and prints its path. [clawpatch-autofix.sh:finding_details_file]
- `finding_title <id>`: prints the finding title, or `untitled finding`.
  [clawpatch-autofix.sh:finding_title]
- `print_finding_summary <id>`: prints title, severity, feature, description,
  recommendation, and evidence lines. [clawpatch-autofix.sh:print_finding_summary]
- `next_finding`: runs `clawpatch next --json` and prints the finding ID.
  [clawpatch-autofix.sh:next_finding]
- `revalidate <id>`: runs `clawpatch revalidate --finding <id> --json` and
  prints the status. [clawpatch-autofix.sh:revalidate]
- `fix_until_fixed <id>`: runs `clawpatch fix` and `revalidate` up to
  `MAX_FIX_ATTEMPTS` times. [clawpatch-autofix.sh:fix_until_fixed]
- `process_finding <id>`: runs fix, `$simplify`, revalidate, `$deslop`,
  revalidate, then commit, up to `MAX_HYGIENE_ROUNDS` times.
  [clawpatch-autofix.sh:process_finding]

### Subdirectories

- None.

## Invariants

- `set -Eeuo pipefail` is on. Unset variables and unguarded failing commands
  stop the script. [clawpatch-autofix.sh:2]
- A fix attempt succeeds only when `revalidate` prints exactly `fixed`.
  [clawpatch-autofix.sh:fix_until_fixed]
- `process_finding` calls `fix_until_fixed` without a guard. Under `set -e`,
  a finding that is still not `fixed` after `MAX_FIX_ATTEMPTS` ends the whole
  run with a non-zero exit, so no later hygiene round or finding runs.
  [clawpatch-autofix.sh:166] [clawpatch-autofix.sh:2]
- The script commits a finding only after `revalidate` prints `fixed` after the
  `$simplify` pass and again after the `$deslop` pass. Any other status starts
  the next hygiene round. [clawpatch-autofix.sh:process_finding]
- A commit counts as done only when `git status --porcelain` is empty after the
  Codex commit call. [clawpatch-autofix.sh:commit_current_changes]
- Codex always runs with stdin from `/dev/null`.
  [clawpatch-autofix.sh:codex_one_shot]
- The commit prompt tells Codex not to push and not to open a pull request.
  [clawpatch-autofix.sh:commit_current_changes]
- The temporary directory is removed on exit by `trap 'rm -rf "$tmpdir"' EXIT`.
  [clawpatch-autofix.sh:10]
- The script does not check for a clean working tree at start, and does not
  check that `clawpatch`, `codex`, `jq`, or `git` exist. A missing tool fails at
  its first use. [clawpatch-autofix.sh:1-10]

## Side Effects

- Creates a temporary directory with `mktemp -d` and writes `next.json`,
  `fix-<id>.json`, `revalidate-<id>.json`, and `finding-details-<id>.json` into
  it. [clawpatch-autofix.sh:9] [clawpatch-autofix.sh:next_finding]
  [clawpatch-autofix.sh:fix_until_fixed] [clawpatch-autofix.sh:revalidate]
  [clawpatch-autofix.sh:finding_details_file]
- Reads `$ROOT/.clawpatch/findings/<id>.json` and
  `$ROOT/.clawpatch/features/<featureId>.json` with `jq`. If the finding file
  is missing, runs `clawpatch show --finding <id> --json` instead.
  [clawpatch-autofix.sh:finding_details_file]
- Runs these Clawpatch commands against `$ROOT`:
  `clawpatch next --json`,
  `clawpatch fix --finding <id> --skip-git-repo-check --json`, and
  `clawpatch revalidate --finding <id> --json`.
  [clawpatch-autofix.sh:next_finding]
  [clawpatch-autofix.sh:fix_until_fixed] [clawpatch-autofix.sh:revalidate]
- Runs Codex in `$ROOT` with approvals and the sandbox disabled, using the
  literal prompts `$simplify` and `$deslop`. The single quotes stop shell
  expansion. [clawpatch-autofix.sh:168] [clawpatch-autofix.sh:175]
  [clawpatch-autofix.sh:codex_one_shot]
- Creates git commits through Codex. The script itself runs only
  `git rev-parse`, `git status --porcelain`, and `git status --short`.
  [clawpatch-autofix.sh:ROOT] [clawpatch-autofix.sh:commit_current_changes]
- Leaves edits in the working tree when a finding fails. The script contains no
  `git reset`, `git checkout`, or `git stash`. [clawpatch-autofix.sh:1-197]
- Prints to stdout: progress lines that start with `==>`, the finding summary
  from `print_finding_summary` in the first hygiene round, and
  `no open findings`. Prints failure messages plus `git status --short` output
  to stderr.
  [clawpatch-autofix.sh:fix_until_fixed] [clawpatch-autofix.sh:process_finding]
  [clawpatch-autofix.sh:print_finding_summary] [clawpatch-autofix.sh:193]
  [clawpatch-autofix.sh:commit_current_changes]
- Makes no direct network calls. Network use by `clawpatch` and `codex` is not
  visible in this script. [clawpatch-autofix.sh:1-197]

## Verification

- Run: `bash -n scripts/clawpatch-autofix.sh` (syntax check only). The
  repository has no tests and no shell linter task for this script.
- Runner: `mise run clawpatch:autofix`. CAUTION: This runs Clawpatch and Codex
  with the sandbox disabled, edits files, and creates git commits. Run it only
  on a branch where those commits are wanted.
