#!/bin/sh
# A hosted runner that loses its connection to GitHub leaves no log and no artifact, and with them
# goes the record of what its memory and CPUs were doing. Every minute until stopped, this copies
# that record to a check run of its own on the commit under test, which GitHub keeps:
#
#   test/e2e/ci/beacon.sh <check run name> <guard.sh record> <guests.sh record>
#
# It reads GH_TOKEN (checks: write), GITHUB_REPOSITORY and GITHUB_SHA. Stopped with TERM, it ends
# the check run as neutral. A lost runner leaves it in progress, with its last minute's report.
set -eu

name=$1
guard=$2
guests=$3
dir=$(dirname "$0")
api="repos/${GITHUB_REPOSITORY}/check-runs"

# Without the right to write checks, as in a fork's run, there is nothing to keep it in.
id=$(gh api "${api}" -f name="${name}" -f head_sha="${GITHUB_SHA}" -f status=in_progress \
    --jq .id) || exit 0

report() {
    date -u +%T
    uptime
    echo
    free -m
    echo
    tail -n 3 "${guard}" || true
    "${dir}/guard.sh" peaks "${guard}" || true
    echo
    "${dir}/guests.sh" summary "${guests}" || true
}

# send <jq filter>: a check run's text holds at most 65535 characters, so a long summary keeps its
# latest minutes.
send() {
    report 2>&1 | tail -c 60000 | jq -Rs "$1" | gh api -X PATCH "${api}/${id}" --input - > /dev/null ||
        true
}

# shellcheck disable=SC2016 # jq's filter, whose backticks fence the text as code.
update='{output: {title: "The runner, each minute", summary: "Load, memory and each VM by minute.", text: ("```\n" + . + "```")}}'
stop() {
    send "${update} + {status: \"completed\", conclusion: \"neutral\"}"
    exit 0
}
trap stop TERM

while :; do
    send "${update}"
    sleep 60 &
    wait $! || true
done
