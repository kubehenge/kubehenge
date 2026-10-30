#!/bin/sh
# Unit test of how the EL template builds leave a mirror that serves a broken repomd.xml
# (pkg/templates/el/mirrors.sh): a mirror service and two mirrors on an HTTP server on loopback, the
# first mirror serving AppStream's repomd.xml as a mirror does while it syncs, and a stand-in for dnf
# that reads each repository from the first mirror of its list and fails on that file, without
# trying the next, as dnf does. Needs Python 3 and curl, and no EL host.
#
#   test/unit/el-mirrors.sh
# shellcheck disable=SC2016 # dnf's variables, which the repository files and lists hold unfilled.
set -eu

here=$(dirname "$0")
root=$(mktemp -d)
bin=${root}/bin www=${root}/www repos=${root}/repos lists=${root}/lists
mkdir "${bin}" "${www}" "${repos}" "${lists}"
server=''
finish() {
    [ -z "${server}" ] || kill "${server}" 2> /dev/null || true
    [ -z "${server}" ] || wait "${server}" 2> /dev/null || true
    rm -rf "${root}"
}
trap finish EXIT

passes=0 failures=0
pass() { passes=$((passes + 1)); }
# has <what> <file> <text>: whether the file holds the text.
has() {
    if [ -f "$2" ] && grep -qF -- "$3" "$2"; then
        pass
    else
        said='(no file)'
        [ ! -f "$2" ] || said=$(cat "$2")
        printf 'FAIL: %s\n  wanted %s to say: %s\n  it said: %s\n' "$1" "$2" "$3" "${said}"
        failures=$((failures + 1))
    fi
}
# lacks <what> <file> <text>: whether the file, if there is one, leaves the text out.
lacks() {
    if [ -f "$2" ] && grep -qF -- "$3" "$2"; then
        printf 'FAIL: %s\n  wanted %s not to say: %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    else
        pass
    fi
}
# expect <what> <wanted> <got>: whether the two are the same.
expect() {
    if [ "$2" = "$3" ]; then
        pass
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}
# holds <what> <file> <wanted>: whether the file holds exactly what is wanted.
holds() {
    got=$(cat "$2")
    expect "$1" "$3" "${got}"
}
# lines <what> <file> <count>: whether the file has that many lines.
lines() {
    got=$(wc -l < "$2")
    expect "$1" "$3" "$((got))"
}

port=$((20000 + $$ % 20000))
# Not $url, which fetch_mirrors sets.
site=http://127.0.0.1:${port}
# repomd <mirror> <repository> <content>: the repository's repomd.xml on that mirror.
repomd() {
    mkdir -p "${www}/$1/$2/x86_64/os/repodata"
    printf '%s\n' "$3" > "${www}/$1/$2/x86_64/os/repodata/repomd.xml"
}
good='<?xml version="1.0" encoding="UTF-8"?><repomd xmlns="http://linux.duke.edu/metadata/repo"/>'
repomd one AppStream 'mirror sync in progress'
repomd one BaseOS "${good}"
repomd two AppStream "${good}"
repomd two BaseOS "${good}"
# The service's lists as AlmaLinux's come: dnf's variables to fill in and no newline at the end.
mkdir "${www}/mirrorlist"
printf '# the nearest mirrors\n%s/one/AppStream/$basearch/os/\n%s/two/AppStream/$basearch/os/' "${site}" "${site}" \
    > "${www}/mirrorlist/appstream"
printf '%s/one/BaseOS/$basearch/os/\n%s/two/BaseOS/$basearch/os/' "${site}" "${site}" > "${www}/mirrorlist/baseos"

python3 -m http.server --bind 127.0.0.1 --directory "${www}" "${port}" > /dev/null 2>&1 &
server=$!
for _ in $(seq 1 50); do
    curl -fsS -o /dev/null "${site}/mirrorlist/baseos" 2> /dev/null && break
    sleep 0.1
done

# The family's repository files as they come, a disabled repository among them, and one of Rocky's.
cat > "${repos}/almalinux-appstream.repo" << 'EOF'
[appstream]
name=AlmaLinux $releasever - AppStream
mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/appstream?arch=x86_64_v2
enabled=1
gpgcheck=1

[appstream-debuginfo]
name=AlmaLinux $releasever - AppStream - Debug
mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/appstream-debug?arch=x86_64_v2
enabled=0
gpgcheck=1
EOF
cat > "${repos}/almalinux-baseos.repo" << 'EOF'
[baseos]
name=AlmaLinux $releasever - BaseOS
mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos?arch=x86_64_v2
enabled=1
gpgcheck=1
EOF
cat > "${repos}/rocky.repo" << 'EOF'
[rocky-appstream]
name=Rocky Linux $releasever - AppStream
mirrorlist=https://mirrors.rockylinux.org/mirrorlist?arch=$basearch&repo=AppStream-$releasever$rltype
gpgcheck=1
enabled=0
EOF

# dnf's Python API, which names the enabled repositories that read a mirror list and the addresses
# of their lists, here those of the service on loopback.
cat > "${bin}/python3" << EOF
#!/bin/sh
cat > /dev/null
printf 'appstream %s\nbaseos %s\n' '${site}/mirrorlist/appstream' '${site}/mirrorlist/baseos'
EOF
printf '#!/bin/sh\necho "$1" >> "%s/slept"\n' "${root}" > "${bin}/sleep"
chmod +x "${bin}/python3" "${bin}/sleep"
PATH=${bin}:${PATH}

# dnf_reads <repositories>: each enabled repository's repomd.xml from the first mirror of its list,
# failing where that file is not XML, as dnf does.
dnf_reads() {
    awk -F= 'function done() { if (enabled == "1" && list ~ /^file:\/\//) print substr(list, 8) }
        /^\[/ { done(); list = ""; enabled = "" }
        $1 == "mirrorlist" { list = $2 }
        $1 == "enabled" { enabled = $2 }
        END { done() }' "$1"/*.repo > "${root}/lists-read"
    while read -r list; do
        read -r mirror < "${list}"
        repomd_url=$(printf '%s' "${mirror}" | sed 's/\$basearch/x86_64/')repodata/repomd.xml
        echo "${repomd_url}" >> "${root}/read"
        curl -fsS "${repomd_url}" > "${root}/repomd.xml" || return 1
        read -r start < "${root}/repomd.xml" || true
        case "${start}" in
            '<?xml'*) ;;
            *)
                echo "repomd.xml parser error: Parse error at line: 1 (Start tag expected, '<' not found)" >&2
                return 1
                ;;
        esac
    done < "${root}/lists-read"
}

# shellcheck source=pkg/templates/el/mirrors.sh
. "${here}/../../pkg/templates/el/mirrors.sh"

use_metalinks "${repos}"
has "Rocky's repositories read its metalinks" "${repos}/rocky.repo" \
    'metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=AppStream-$releasever$rltype'
lacks "nor their mirror lists" "${repos}/rocky.repo" 'mirrorlist='
has "AlmaLinux's keep their mirror lists" "${repos}/almalinux-appstream.repo" \
    'mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/appstream?arch=x86_64_v2'

fetch_mirrors "${lists}"
holds "AppStream's list, a mirror to a line" "${lists}/appstream" "${site}/one/AppStream/\$basearch/os/
${site}/two/AppStream/\$basearch/os/"
lines "the list keeps its last line" "${lists}/baseos" 2

point_at_mirrors "${repos}" "${lists}"
has "AppStream reads its list" "${repos}/almalinux-appstream.repo" "mirrorlist=file://${lists}/appstream"
has "BaseOS reads its list" "${repos}/almalinux-baseos.repo" "mirrorlist=file://${lists}/baseos"
has "a repository without a list keeps the service's" "${repos}/almalinux-appstream.repo" \
    'mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/appstream-debug?arch=x86_64_v2'

# shellcheck disable=SC2310 # the status is the answer looked for.
from_repositories "${lists}" dnf_reads "${repos}" 2> "${root}/said" && status=0 || status=$?
expect "the step succeeds on its second attempt" 0 "${status}"
has "the first attempt read the broken mirror" "${root}/read" "${site}/one/AppStream/x86_64/os/repodata/repomd.xml"
has "the second read the next mirror" "${root}/read" "${site}/two/AppStream/x86_64/os/repodata/repomd.xml"
lines "it read AppStream, then AppStream and BaseOS" "${root}/read" 3
holds "it paused once" "${root}/slept" 10
has "it says why" "${root}/said" "Start tag expected"
has "and that it tries again" "${root}/said" "trying again in 10 s, on the next mirrors: dnf_reads"

# A later step, the composition or the image's build, starts on the mirrors that worked.
rm "${root}/read" "${root}/slept"
fetch_mirrors "${lists}"
read -r first < "${lists}/appstream"
expect "fetching again keeps the lists as they turned" "${site}/two/AppStream/\$basearch/os/" "${first}"
from_repositories "${lists}" dnf_reads "${repos}" 2> /dev/null
lacks "a later step leaves the broken mirror alone" "${root}/read" "${site}/one/"
lacks "and does not pause" "${root}/slept" 10

# node.sh points its own copies of the family's files at the same lists.
mkdir "${root}/image"
printf '[appstream]\nmirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/appstream?arch=x86_64_v2\nenabled=1\n' \
    > "${root}/image/almalinux-appstream.repo"
point_at_mirrors "${root}/image" "${lists}"
has "the image's build reads the same list" "${root}/image/almalinux-appstream.repo" "mirrorlist=file://${lists}/appstream"

# Every mirror broken: three attempts, and the step fails.
repomd two AppStream 'mirror sync in progress'
rm "${root}/read"
# shellcheck disable=SC2310 # the status is the answer looked for.
from_repositories "${lists}" dnf_reads "${repos}" 2> /dev/null && status=0 || status=$?
expect "with every mirror broken the step fails" 1 "${status}"
lines "after three attempts" "${root}/read" 3

if [ "${failures}" -ne 0 ]; then
    echo "el-mirrors: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "el-mirrors: all ${passes} checks passed"
