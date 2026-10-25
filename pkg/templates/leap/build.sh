#!/bin/sh
# Installs what a node needs into the family's immutable image as one new snapshot of its read-only
# root (node.sh, run by transactional-update), which the bake then reboots into. It works in the
# directory holding the bake's files, the parent of its own:
#
#   build.sh <distribution> [<Kubernetes version>]
set -eu

distribution=$1
kubernetes=${2:-}
cd "$(dirname "$0")/.."

# The new snapshot starts as a copy of the running root, /etc included, but sees nothing of /var.
staged=/etc/kubehenge-build
rm -rf "${staged}"
cp -a . "${staged}"
transactional-update --non-interactive run sh "${staged}/leap/node.sh" "${distribution}" "${kubernetes}"
rm -rf "${staged}"
touch built
