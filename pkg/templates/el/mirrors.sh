#!/bin/sh
# Sourced by build.sh and node.sh, for every step that reads the family's repositories. dnf and
# rpm-ostree leave a mirror whose download fails or crawls for the next, but not one whose
# repomd.xml downloads and does not parse, as a mirror's does for minutes while it syncs: the step
# fails. And the families' mirror services keep giving the builder its mirrors in the same order,
# so a step tried again on the same list reads the same broken file.

# use_metalinks <repositories>: Rocky Linux's mirror service, MirrorManager, serves each mirror
# list as a metalink too, which carries the checksums of the repository's current and recent
# repomd.xml. dnf and rpm-ostree refuse a repomd.xml that matches none, broken or stale alike, and
# take the next mirror in the same run.
use_metalinks() {
    sed -i 's|^mirrorlist=https://mirrors.rockylinux.org/mirrorlist?|metalink=https://mirrors.rockylinux.org/metalink?|' "$1"/*.repo
}

# fetch_mirrors <lists>: AlmaLinux's mirror service has no metalinks, so each enabled repository
# that reads a mirror list gets it once, kept in <lists> under the repository's id, where
# next_mirrors turns it after each failed attempt. dnf itself names the repositories and the
# addresses of their lists, with its variables filled in.
fetch_mirrors() {
    python3 - > "$1/.repositories" << 'EOF'
import dnf

base = dnf.Base()
base.conf.read()
base.conf.substitutions.update_from_etc(base.conf.installroot, varsdir=base.conf.varsdir)
base.read_all_repos()
for repo in base.repos.iter_enabled():
    if repo.mirrorlist and not repo.metalink:
        print(repo.id, repo.mirrorlist)
EOF
    while read -r id url; do
        [ ! -s "$1/${id}" ] || continue
        curl -fsS --retry 5 --retry-delay 10 --retry-all-errors "${url}" > "$1/.fetched" || return 1
        # One mirror to a line, the last one too, which the service ends without a newline.
        awk 'NF && !/^#/' "$1/.fetched" > "$1/.mirrors"
        if [ ! -s "$1/.mirrors" ]; then
            echo "no mirrors for ${id} at ${url}" >&2
            return 1
        fi
        mv "$1/.mirrors" "$1/${id}"
    done < "$1/.repositories"
}

# point_at_mirrors <repositories> <lists>: each repository with a list in <lists> reads it there.
point_at_mirrors() {
    for list in "$2"/*; do
        [ -f "${list}" ] || continue
        sed -i "/^\[${list##*/}\]\$/,/^\[/s|^mirrorlist=.*|mirrorlist=file://${list}|" "$1"/*.repo
    done
}

# next_mirrors <lists>: each list from its second mirror on, with its first last.
next_mirrors() {
    for list in "$1"/*; do
        [ -f "${list}" ] || continue
        { tail -n +2 "${list}" && head -n 1 "${list}"; } > "$1/.next"
        mv "$1/.next" "${list}"
    done
}

# from_repositories <lists> <command>...: the command up to three times, each attempt after a
# failed one on the next mirror of every list, which every later step keeps. The pause is for a
# fault of the network, which a mirror's sync is not.
from_repositories() {
    lists=$1
    shift
    for attempt in 1 2 3; do
        "$@" && return 0
        [ "${attempt}" -lt 3 ] || return 1
        next_mirrors "${lists}"
        echo "trying again in 10 s, on the next mirrors: $*" >&2
        sleep 10
    done
}
