#!/bin/sh
# The verdict on an end-to-end lane (e2e-lane.yml), from its tries' results and outputs as the
# workflow's needs context holds them, in JSON on stdin:
#
#   test/e2e/ci/lane.sh <lane> < needs.json
#
# Prints each try's result, and the test that failed, into the run's summary too when
# GITHUB_STEP_SUMMARY names it. Succeeds when the last try that ran ran the test and passed. A try
# that hands the test back succeeds without running it. A try that lost its runner never ran its
# last step, so it lacks the ended output, and the next try runs the test again. A try that failed
# with its runner still there is the last.
set -eu

lane=${1:?usage: $0 <lane> < needs.json}
tries=$(jq -c 'to_entries | sort_by(.key) | map(.value + {try: .key})')

printf '%s' "${tries}" | jq -r '.[] | "\(.try): \(.result)"
    + (if .result == "failure" and .outputs.ended != "true" then ", its runner lost" else "" end)
    + (.outputs.failed // "" | if . == "" then "" else ", \(.) failed" end)'
# GitHub names the runner's loss in the try's log alone, if it keeps that log at all.
printf '%s' "${tries}" | jq -r '[.[] | select(.result != "skipped")] as $ran | $ran | to_entries[]
    | select(.value.result == "failure" and .value.outputs.ended != "true")
    | if .key < ($ran | length) - 1
      then "::warning title=\(.value.try) lost its runner::\(.value.try) lost its runner, so \($ran[.key + 1].try) ran the test again"
      else "::error title=\(.value.try) lost its runner::\(.value.try) lost its runner, and no try was left"
      end'
# A lane of two names the test that failed.
failed=$(printf '%s' "${tries}" | jq -r --arg l "${lane}" '.[] | .outputs.failed // empty
    | select(. != "") | "\(.) failed, in the lane \($l)."')
if [ -n "${failed}" ]; then
    printf '%s\n' "${failed}"
    [ -z "${GITHUB_STEP_SUMMARY:-}" ] || printf '%s\n' "${failed}" >> "${GITHUB_STEP_SUMMARY}"
fi
printf '%s' "${tries}" | jq -e '[.[] | select(.result != "skipped")] | last
    | . != null and .result == "success" and .outputs["handed-back"] != "true"' > /dev/null
