#!/bin/sh
# Unit test of the Makefile's .PHONY lines. Every target named like a word rather than a file is
# phony, or a file of its name would stop make from running it. Each .PHONY line names one target
# and sits right above that target's rule, so two pull requests that add targets never edit the same
# line.
#
#   test/unit/phony.sh
set -eu

makefile=$(dirname "$0")/../../Makefile

# A rule's target may be a variable, such as $(UNIT_SHELL), named the same way in .PHONY.
# shellcheck disable=SC2016 # awk code, which awk expands
awk '
    function fail(why, line) {
        printf "FAIL: Makefile:%d: %s\n", line ? line : FNR, why
        bad = 1
    }
    # The first pass reads the .PHONY lines, the second the rules, as a rule may come first in a
    # conditional, such as unit-tofu after unit-go.
    NR == FNR {
        if (expect != "" && index($0, expect ":") != 1) fail(".PHONY: " expect " is not right above the rule of " expect)
        expect = ""
        if (!/^\.PHONY:/) next
        if (NF != 2) fail("a .PHONY line names one target, beside its rule: " $0)
        else {
            phony[$2] = 1
            expect = $2
            at = FNR
        }
        next
    }
    FNR == 1 && expect != "" { fail(".PHONY: " expect " has no rule below it", at) }
    /^[A-Za-z0-9_-]+:([^=]|$)/ {
        name = $0
        sub(/:.*/, "", name)
        if (!(name in phony) && !(name in reported)) {
            fail(name " is not a file, and no .PHONY line right above its rule names it")
            reported[name] = 1
        }
    }
    END { exit bad }
' "${makefile}" "${makefile}"
echo 'phony: ok'
