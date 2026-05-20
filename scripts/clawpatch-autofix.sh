#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-$(git rev-parse --show-toplevel)}"
MAX_FINDINGS="${MAX_FINDINGS:-1}"
MAX_FIX_ATTEMPTS="${MAX_FIX_ATTEMPTS:-3}"
MAX_HYGIENE_ROUNDS="${MAX_HYGIENE_ROUNDS:-2}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

claw() {
	clawpatch --root "$ROOT" "$@"
}

codex_one_shot() {
	local prompt="$1"
	local args=(
		exec
		-C "$ROOT"
		--dangerously-bypass-approvals-and-sandbox
	)
	if [[ -n "${CODEX_MODEL:-}" ]]; then
		args+=(--model "$CODEX_MODEL")
	fi
	codex "${args[@]}" "$prompt" </dev/null
}

commit_current_changes() {
	codex_one_shot 'commit everything to git. Do not push or open a pull request.'
	if [[ -n "$(git -C "$ROOT" status --porcelain)" ]]; then
		echo "codex commit left uncommitted changes:" >&2
		git -C "$ROOT" status --short >&2
		return 1
	fi
}

finding_id() {
	jq -r 'if type == "array" then .[0] else . end | .finding.id // .id // empty'
}

finding_status() {
	jq -r '
		if type == "array" then .[0] else . end
		| .outcome // .status // (if (.finding | type) == "object" then .finding.status else empty end) // empty
	'
}

finding_details_file() {
	local finding="$1"
	local output="$tmpdir/finding-details-${finding}.json"
	local finding_file="$ROOT/.clawpatch/findings/${finding}.json"
	local feature_file
	local feature_id
	if [[ ! -s "$output" ]]; then
		if [[ -f "$finding_file" ]]; then
			feature_id="$(jq -r '.featureId // empty' "$finding_file")"
			feature_file="$ROOT/.clawpatch/features/${feature_id}.json"
			if [[ -n "$feature_id" && -f "$feature_file" ]]; then
				jq -s '
					.[0] as $finding
					| .[1] as $feature
					| {
						finding: ($finding + {
							id: ($finding.findingId // $finding.id // ""),
							feature: {
								id: ($finding.featureId // $feature.featureId // ""),
								title: ($feature.title // "")
							}
						}),
						feature: $feature
					}
				' "$finding_file" "$feature_file" >"$output"
			else
				jq '{finding: (. + {id: (.findingId // .id // "")})}' "$finding_file" >"$output"
			fi
		else
			claw show --finding "$finding" --json >"$output"
		fi
	fi
	printf '%s\n' "$output"
}

finding_title() {
	local details
	details="$(finding_details_file "$1")"
	jq -r '(.finding // .).title // "untitled finding"' "$details"
}

print_finding_summary() {
	local finding="$1"
	local details
	details="$(finding_details_file "$finding")"

	jq -r '
		def finding: .finding // .;
		def clean: tostring | gsub("[[:space:]]+"; " ");
		def line($label; $value):
			if ($value // "") == "" then empty
			else "    \($label): \($value | clean)"
			end;
		def evidence_line:
			select((.path // "") != "")
			| "      - \(.path)"
				+ (if .startLine then ":\(.startLine)" else "" end)
				+ (if .endLine and .startLine and .endLine != .startLine then "-\(.endLine)" else "" end)
				+ (if (.symbol // "") != "" then " (\(.symbol))" else "" end);

		finding as $finding
		| line("title"; $finding.title),
		  line("severity"; ([$finding.severity, $finding.category, $finding.confidence] | map(select(. != null and . != "")) | join(" / "))),
		  line("feature"; ($finding.feature.title // .feature.title // $finding.featureId)),
		  line("description"; ($finding.reasoning // $finding.description // $finding.summary)),
		  line("recommendation"; $finding.recommendation),
		  (if (($finding.evidence // []) | length) > 0 then
			"    evidence:",
			($finding.evidence[] | evidence_line)
		  else empty end)
	' "$details"
}

next_finding() {
	local output="$tmpdir/next.json"
	claw next --json >"$output"
	finding_id <"$output"
}

revalidate() {
	local finding="$1"
	local output="$tmpdir/revalidate-${finding}.json"
	claw revalidate --finding "$finding" --json >"$output"
	finding_status <"$output"
}

fix_until_fixed() {
	local finding="$1"
	local status
	local title

	title="$(finding_title "$finding")"

	for ((attempt = 1; attempt <= MAX_FIX_ATTEMPTS; attempt++)); do
		echo "==> clawpatch fix $finding attempt $attempt/$MAX_FIX_ATTEMPTS: $title"
		claw fix --finding "$finding" --skip-git-repo-check --json >"$tmpdir/fix-${finding}.json"

		status="$(revalidate "$finding")"
		echo "==> revalidate $finding: $status"
		if [[ "$status" == "fixed" ]]; then
			return 0
		fi
	done

	echo "finding $finding did not validate as fixed after $MAX_FIX_ATTEMPTS attempts" >&2
	return 1
}

process_finding() {
	local finding="$1"
	local status

	for ((round = 1; round <= MAX_HYGIENE_ROUNDS; round++)); do
		echo "==> processing $finding hygiene round $round/$MAX_HYGIENE_ROUNDS"
		if ((round == 1)); then
			print_finding_summary "$finding"
		fi
		fix_until_fixed "$finding"

		codex_one_shot '$simplify'
		status="$(revalidate "$finding")"
		echo "==> after simplify revalidate $finding: $status"
		if [[ "$status" != "fixed" ]]; then
			continue
		fi

		codex_one_shot '$deslop'
		status="$(revalidate "$finding")"
		echo "==> after deslop revalidate $finding: $status"
		if [[ "$status" != "fixed" ]]; then
			continue
		fi

		commit_current_changes
		return 0
	done

	echo "finding $finding did not stay fixed after $MAX_HYGIENE_ROUNDS hygiene rounds" >&2
	return 1
}

for ((processed = 0; processed < MAX_FINDINGS; processed++)); do
	finding="$(next_finding)"
	if [[ -z "$finding" ]]; then
		echo "no open findings"
		exit 0
	fi
	process_finding "$finding"
done
