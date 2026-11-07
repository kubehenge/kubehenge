#!/bin/sh
# Unit test of test/e2e/ci/lane.sh, the verdict on an end-to-end lane from its tries: a test that
# passed passes the lane, also after tries that handed it back or lost their runner, and a test that
# failed, or a lane whose every try lost its runner, fails it.
#
#   test/unit/lane.sh
set -eu

here=$(dirname "$0")
lane=${here}/../e2e/ci/lane.sh

passes=0 failures=0
ok() { passes=$((passes + 1)); }
# bad <what> [<file>]: a failure, with the file the verdict wrote.
bad() {
    printf 'FAIL: %s\n' "$1"
    [ -z "${2:-}" ] || sed 's/^/  /' "$2"
    failures=$((failures + 1))
}

out=$(mktemp)
summary=$(mktemp)
trap 'rm -f "${out}" "${summary}"' EXIT

# Each try as the needs context holds it.
ran='{"result":"success","outputs":{"handed-back":"","failed":"","ended":"true"}}'
handed='{"result":"success","outputs":{"handed-back":"true","failed":"","ended":"true"}}'
failed='{"result":"failure","outputs":{"handed-back":"","failed":"rke2-etcd","ended":"true"}}'
# A runner stopped mid-test still sets the outputs of the steps it ran.
stopped='{"result":"failure","outputs":{"handed-back":"","failed":""}}'
# A runner that stops answering sets none.
silent='{"result":"failure","outputs":{}}'
skipped='{"result":"skipped","outputs":{}}'

# judge <what> <pass|fail> <needs>: whether lane.sh judges the lane so, its output in ${out}.
judge() {
    : > "${summary}"
    code=0
    printf '%s' "$3" | GITHUB_STEP_SUMMARY=${summary} sh "${lane}" rke2-etcd > "${out}" 2>&1 || code=$?
    case $2-${code} in
        pass-0) ok ;;
        fail-0) bad "$1: passed, want failed" "${out}" ;;
        pass-*) bad "$1: failed, want passed" "${out}" ;;
        *) ok ;;
    esac
}
# verdict <what> <pass|fail> <try 1> <try 2> <try 3> <try 4>: judge on the needs context of the
# lane's result job.
verdict() {
    judge "$1" "$2" "{\"try-1\":$3,\"try-2\":$4,\"try-3\":$5,\"try-4\":$6}"
}
# says <what> <text>: whether the last verdict printed the text.
says() {
    if grep -qF -- "$2" "${out}"; then ok; else bad "$1: no \"$2\" in" "${out}"; fi
}
# silent_on <what> <text>: whether the last verdict left the text out.
silent_on() {
    if grep -qF -- "$2" "${out}"; then bad "$1: \"$2\" in" "${out}"; else ok; fi
}

verdict "the first try passed" pass "${ran}" "${skipped}" "${skipped}" "${skipped}"
says "each try is listed" "try-1: success"
says "a try that did not run is listed" "try-4: skipped"
silent_on "a lane that lost nothing" "lost its runner"

verdict "handed back, then passed" pass "${handed}" "${ran}" "${skipped}" "${skipped}"
verdict "handed back three times, then passed" pass "${handed}" "${handed}" "${handed}" "${ran}"
verdict "every try handed back" fail "${handed}" "${handed}" "${handed}" "${handed}"

verdict "the test failed" fail "${failed}" "${skipped}" "${skipped}" "${skipped}"
says "the failed test is named" "try-1: failure, rke2-etcd failed"
says "and its lane" "rke2-etcd failed, in the lane rke2-etcd."
if grep -qxF "rke2-etcd failed, in the lane rke2-etcd." "${summary}"; then ok; else bad "the summary lacks the failed test" "${summary}"; fi
silent_on "a test that failed lost no runner" "lost its runner"

verdict "the runner stopped, then the test passed" pass "${stopped}" "${ran}" "${skipped}" "${skipped}"
says "the stopped runner is named" "try-1: failure, its runner lost"
says "as a warning" "::warning title=try-1 lost its runner::try-1 lost its runner, so try-2 ran the test again"
if [ -s "${summary}" ]; then bad "a lost runner put a failed test in the summary" "${summary}"; else ok; fi

verdict "the runner stopped answering, then the test passed" pass "${silent}" "${ran}" "${skipped}" "${skipped}"
says "the silent runner is named" "try-1: failure, its runner lost"

verdict "handed back, lost, then passed" pass "${handed}" "${stopped}" "${ran}" "${skipped}"
says "the warning names the try after the lost one" "try-2 lost its runner, so try-3 ran the test again"

verdict "lost, then the test failed" fail "${stopped}" "${failed}" "${skipped}" "${skipped}"
says "the failure is named after the loss" "try-2: failure, rke2-etcd failed"

verdict "every try lost its runner" fail "${stopped}" "${silent}" "${stopped}" "${stopped}"
says "the last loss is an error" "::error title=try-4 lost its runner::try-4 lost its runner, and no try was left"

verdict "a lost runner and no try after it" fail "${stopped}" "${skipped}" "${skipped}" "${skipped}"
says "a lost last try is an error" "::error title=try-1 lost its runner::"

unordered="{\"try-2\":${failed},\"try-1\":${ran},\"try-3\":${skipped},\"try-4\":${skipped}}"
judge "tries out of order are read in order" fail "${unordered}"
unordered="{\"try-2\":${ran},\"try-1\":${stopped},\"try-3\":${skipped},\"try-4\":${skipped}}"
judge "tries out of order, the last passed" pass "${unordered}"

verdict "a lane that ran nothing" fail "${skipped}" "${skipped}" "${skipped}" "${skipped}"

printf '%d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
