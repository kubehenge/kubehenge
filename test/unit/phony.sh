#!/bin/sh
# Unit test of the Makefile's .PHONY lists: every name in them is a target the Makefile defines. A
# name two targets were joined into declares neither phony, so a file of either name would stop make
# from running it.
#
#   test/unit/phony.sh
set -eu

makefile=$(dirname "$0")/../../Makefile

# A rule's target may be a variable, such as $(UNIT_SHELL), named the same way in .PHONY.
targets=$(sed -n 's/^\([A-Za-z0-9_.%-]*\|[$][(][A-Z_]*[)]\):.*/\1/p' "${makefile}")
phony=$(sed -n 's/^\.PHONY:\(.*\)/\1/p' "${makefile}" | tr -s '[:blank:]' '\n' | sed '/^$/d')
missing=$(printf '%s\n' "${phony}" | grep -vxF -e "${targets}" || true)

if [ -n "${missing}" ]; then
    printf '%s\n' "${missing}" | sed 's/.*/FAIL: .PHONY names &, which no rule defines/'
    exit 1
fi
echo 'phony: ok'
