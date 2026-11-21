#!/bin/sh
# Sourced by build.sh and node.sh, for every step that reads the family's repositories. dnf and
# rpm-ostree leave a mirror whose download fails or crawls for the next, but not one whose
# repomd.xml downloads and does not parse, as a mirror's does for minutes while it syncs: the step
# fails. And the families' mirror services keep giving the builder its mirrors in the same order,
# so a step tried again on the same list reads the same broken file.

# pin_release <repositories> <minor>: the family's repository files name their release by dnf's
# $releasever, which is the major release alone, so they read the newest minor release. Each step
# reads the one of the bake's image instead (el/release). $releasever_major and the like stay.
pin_release() {
    sed -i "s/\\\$releasever\\b/$2/g" "$1"/*.repo
}

# use_vault <folders> <lists>: a family moves a minor release to its vault once a newer one replaces
# it. Rocky Linux's mirror service then still names mirrors that dropped it. A repository whose own
# server no longer holds the minor release reads the vault, as the one mirror of its list in
# <lists>. Each line of <folders> (el/vault) is a repository's id, its folder on its own server and
# its folder in the vault. Any answer but 404 leaves the repository on its mirrors. One but 200
# is named in the log, as a dnf that fails on those mirrors later would not say why.
use_vault() {
    while read -r id folder vault; do
        code=$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 20 --max-time 30 --retry 3 --retry-delay 10 \
            "${folder}repodata/repomd.xml") || true
        case "${code}" in
            200) ;;
            404)
                echo "${id}: ${folder} is gone, reading ${vault}" >&2
                printf '%s\n' "${vault}" > "$2/${id}"
                ;;
            *) echo "${id}: ${folder} answered ${code}, staying on its mirrors" >&2 ;;
        esac
    done < "$1"
}

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
        curl -fsS --retry 8 --retry-delay 10 --retry-all-errors "${url}" > "$1/.fetched" || return 1
        # One mirror to a line, the last one too, which the service ends without a newline.
        awk 'NF && !/^#/' "$1/.fetched" > "$1/.mirrors"
        if [ ! -s "$1/.mirrors" ]; then
            echo "no mirrors for ${id} at ${url}" >&2
            return 1
        fi
        mv "$1/.mirrors" "$1/${id}"
    done < "$1/.repositories"
}

# repomd_revision <folder>: the revision in the folder's repomd.xml, read through a redirect as dnf
# reads it, empty where the folder does not answer in a few seconds or names none. dnf moves past a
# mirror that does not answer, so the caller keeps it.
repomd_revision() {
    curl -fsSL --connect-timeout 3 --max-time 8 "$1repodata/repomd.xml" 2> /dev/null |
        sed -n 's|.*<revision>\([0-9][0-9]*\)</revision>.*|\1|p' | head -n 1
}

# fill_variables <list>: each line of the mirror list with dnf's variables filled in, as dnf fills
# them, one line out for each line in.
fill_variables() {
    python3 - fill "$1" << 'EOF'
import sys

import dnf
from libdnf.conf import ConfigParser

base = dnf.Base()
base.conf.read()
base.conf.substitutions.update_from_etc(base.conf.installroot, varsdir=base.conf.varsdir)
substitutions = dict(base.conf.substitutions)
with open(sys.argv[2], encoding="utf-8") as lines:
    for line in lines:
        print(ConfigParser.substitute(line.rstrip("\n"), substitutions))
EOF
}

# drop_stale_mirrors <folders> <lists>: a mirror one sync behind serves older packages than the
# build before it, which the kernel check then keeps out of the builds clusters move to. So each
# list in <lists> loses the mirrors whose repomd.xml is older than the repository's folder on its
# own server (its line in el/vault). Rocky Linux has no list: its metalinks say which repomd.xml a
# mirror may serve (use_metalinks). A list use_vault set, a folder that gives no revision and a
# mirror that does not answer stay as they are. A list left empty becomes the own server's folder.
# The step takes about a minute, after which the mirrors not yet asked stay.
drop_stale_mirrors() {
    now=$(date +%s)
    deadline=$((now + 60))
    while read -r id folder vault; do
        list=$2/${id}
        [ -s "${list}" ] || continue
        read -r first < "${list}"
        [ "${first}" != "${vault}" ] || continue
        own=$(repomd_revision "${folder}")
        if [ -z "${own}" ]; then
            echo "${id}: ${folder} gives no revision, keeping its mirrors" >&2
            continue
        fi
        # Filling the variables reads nothing over the network, so a failure is this script's or
        # dnf's, and the build stops on it rather than read mirrors that may be behind.
        if ! fill_variables "${list}" > "${list}.filled"; then
            echo "${id}: dnf did not fill in the mirrors' variables" >&2
            return 1
        fi
        : > "${list}.kept"
        while IFS= read -r entry && IFS= read -r filled <&3; do
            theirs=''
            now=$(date +%s)
            [ "${now}" -ge "${deadline}" ] || theirs=$(repomd_revision "${filled}")
            if [ -n "${theirs}" ] && [ "${theirs}" -lt "${own}" ]; then
                echo "${id}: dropping ${entry}, its revision ${theirs} is behind ${own}" >&2
            else
                printf '%s\n' "${entry}" >> "${list}.kept"
            fi
        done < "${list}" 3< "${list}.filled"
        if [ -s "${list}.kept" ]; then
            mv "${list}.kept" "${list}"
        else
            echo "${id}: every mirror is behind ${folder}, reading it alone" >&2
            printf '%s\n' "${folder}" > "${list}"
        fi
        rm -f "${list}.filled" "${list}.kept"
    done < "$1"
}

# point_at_mirrors <repositories> <lists>: each repository with a list in <lists> reads it there,
# in place of its mirror list, metalink or folder.
point_at_mirrors() {
    for list in "$2"/*; do
        [ -f "${list}" ] || continue
        sed -i "/^\[${list##*/}\]\$/,/^\[/s#^\(mirrorlist\|metalink\|baseurl\)=.*#mirrorlist=file://${list}#" "$1"/*.repo
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
