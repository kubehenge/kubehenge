#!/bin/sh
# Unit test of how the EL template builds leave a mirror that serves a broken repomd.xml
# (pkg/templates/el/mirrors.sh): a mirror service and two mirrors on an HTTP server on loopback, the
# first mirror serving AppStream's repomd.xml as a mirror does while it syncs, and a stand-in for dnf
# that reads each repository from the first mirror of its list and fails on that file, without
# trying the next, as dnf does. Then how they pin the repositories to the image's minor release and
# read its vault once its own server drops it. Needs Python 3 and curl, and no EL host.
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

# The server sends a request for the mirror "moved" on to the mirror "behind", as a mirror that
# moved does.
cat > "${root}/server.py" << 'EOF'
import functools
import http.server
import sys


class Handler(http.server.SimpleHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/moved/"):
            self.send_response(301)
            self.send_header("Location", self.path.replace("/moved/", "/behind/", 1))
            self.end_headers()
            return
        super().do_GET()


handler = functools.partial(Handler, directory=sys.argv[1])
http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[2])), handler).serve_forever()
EOF
python3 "${root}/server.py" "${www}" "${port}" > /dev/null 2>&1 &
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
if [ "\${2:-}" = fill ]; then
    [ -z "\${FILL_FAILS:-}" ] || exit 1
    sed 's/\\\$basearch/x86_64/g' "\$3"
    exit
fi
cat > /dev/null
printf 'appstream %s\nbaseos %s\n' '${site}/mirrorlist/appstream' '${site}/mirrorlist/baseos'
EOF
printf '#!/bin/sh\necho "$1" >> "%s/slept"\n' "${root}" > "${bin}/sleep"
# The clock, which a case sets by writing ${root}/clock: each read of it then gives 25 s more.
clock=$(command -v date)
cat > "${bin}/date" << EOF
#!/bin/sh
[ -f "${root}/clock" ] || exec "${clock}" "\$@"
read -r now < "${root}/clock"
echo "\$((now + 25))" > "${root}/clock"
echo "\${now}"
EOF
chmod +x "${bin}/python3" "${bin}/sleep" "${bin}/date"
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
from_repositories "${repos}" "${lists}" dnf_reads "${repos}" 2> "${root}/said" && status=0 || status=$?
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
from_repositories "${repos}" "${lists}" dnf_reads "${repos}" 2> /dev/null
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
from_repositories "${repos}" "${lists}" dnf_reads "${repos}" 2> /dev/null && status=0 || status=$?
expect "with every mirror broken the step fails" 1 "${status}"
lines "after three attempts" "${root}/read" 3

# Every step reads the minor release of the bake's image, where dnf's $releasever is the major one.
pinned=${root}/pinned
mkdir "${pinned}"
cat > "${pinned}/rocky.repo" << 'EOF'
[baseos]
name=Rocky Linux $releasever - BaseOS
mirrorlist=https://mirrors.rockylinux.org/mirrorlist?arch=$basearch&repo=BaseOS-$releasever$rltype
enabled=1

[epel]
baseurl=https://dl.fedoraproject.org/pub/epel/$releasever_major${releasever_minor:+.$releasever_minor}/Everything/$basearch/
enabled=1
EOF
printf '[baseos]\nmirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos?arch=x86_64_v2\nenabled=1\n' \
    > "${pinned}/almalinux-baseos.repo"
use_metalinks "${pinned}"
pin_release "${pinned}" 10.2
has "Rocky's metalink names the minor release" "${pinned}/rocky.repo" \
    'metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=BaseOS-10.2$rltype'
has "AlmaLinux's mirror list names it" "${pinned}/almalinux-baseos.repo" \
    'mirrorlist=https://mirrors.almalinux.org/mirrorlist/10.2/baseos?arch=x86_64_v2'
has "and so do the names" "${pinned}/rocky.repo" 'name=Rocky Linux 10.2 - BaseOS'
has "dnf's other variables stay" "${pinned}/rocky.repo" '/epel/$releasever_major${releasever_minor:+.$releasever_minor}/'

# A minor release its own server no longer holds comes from the vault. One still there, or a server
# that does not answer, leaves the repository on its mirrors.
repomd vault AppStream "${good}"
vaulted=${root}/vaulted
mkdir "${vaulted}"
printf '%s\n' "baseos ${site}/one/BaseOS/x86_64/os/ ${site}/vault/BaseOS/x86_64/os/" \
    "appstream ${site}/gone/AppStream/x86_64/os/ ${site}/vault/AppStream/x86_64/os/" \
    "extras ${site}/moved/ ${site}/vault/extras/" \
    "kubehenge-containerd http://127.0.0.1:1/epel/ ${site}/vault/epel/" > "${root}/vault"
use_vault "${root}/vault" "${vaulted}" 2> "${root}/said"
holds "AppStream's one mirror is the vault" "${vaulted}/appstream" "${site}/vault/AppStream/x86_64/os/"
has "it says why" "${root}/said" "appstream: ${site}/gone/AppStream/x86_64/os/ is gone"
for id in baseos extras kubehenge-containerd; do
    listed=no
    [ ! -e "${vaulted}/${id}" ] || listed=yes
    expect "${id} stays on its mirrors" no "${listed}"
done
has "an answer but 200 or 404 is named" "${root}/said" "extras: ${site}/moved/ answered 301, staying on its mirrors"
has "and so is no answer" "${root}/said" "kubehenge-containerd: http://127.0.0.1:1/epel/ answered 000, staying on its mirrors"
lacks "a folder that is there is not" "${root}/said" "baseos:"

# The vault's list replaces a metalink and a folder alike.
printf '[appstream]\nmetalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=AppStream-10.1\nenabled=1\n' \
    > "${pinned}/rocky-appstream.repo"
printf '[kubehenge-containerd]\nbaseurl=https://dl.fedoraproject.org/pub/epel/10.1/Everything/x86_64/\nenabled=1\n' \
    > "${pinned}/kubehenge-containerd.repo"
printf '%s\n' "${site}/vault/epel/" > "${vaulted}/kubehenge-containerd"
point_at_mirrors "${pinned}" "${vaulted}"
has "the metalink gives way to the vault" "${pinned}/rocky-appstream.repo" "mirrorlist=file://${vaulted}/appstream"
lacks "and goes" "${pinned}/rocky-appstream.repo" "metalink="
has "the folder gives way to the vault" "${pinned}/kubehenge-containerd.repo" "mirrorlist=file://${vaulted}/kubehenge-containerd"
lacks "and goes" "${pinned}/kubehenge-containerd.repo" "baseurl="

# A mirror one sync behind the repository's own server serves older packages, so its repomd.xml's
# revision decides. Each case has a list of its own, under the id the vault file names.
# revision <mirror> <revision>: AppStream's repomd.xml on that mirror, XML with that revision.
revision() {
    repomd "$1" AppStream "<?xml version=\"1.0\"?><repomd><revision>$2</revision></repomd>"
}
# stale_case <id> <own folder> <mirror>...: the vault file of one repository and its list of mirrors,
# each of them the AppStream folder of the mirror with dnf's variable in it.
stale_case() {
    id=$1 own=$2
    shift 2
    mkdir -p "${root}/stale"
    printf '%s %s %s\n' "${id}" "${own}" "${site}/vault/AppStream/x86_64/os/" > "${root}/stale/vault"
    : > "${root}/stale/${id}"
    for m in "$@"; do
        case ${m} in
            http*) printf '%s\n' "${m}" >> "${root}/stale/${id}" ;;
            *) printf '%s/%s/AppStream/$basearch/os/\n' "${site}" "${m}" >> "${root}/stale/${id}" ;;
        esac
    done
}
# stale_run: the step on that vault file and list.
stale_run() {
    drop_stale_mirrors "${root}/stale/vault" "${root}/stale" 2> "${root}/said"
}
own_folder=${site}/own/AppStream/x86_64/os/
revision own 200
revision behind 199
revision same 200
revision newer 201
revision older 100

stale_case appstream "${own_folder}" behind same
stale_run
holds "a mirror behind the own server is dropped, one as new stays first" "${root}/stale/appstream" \
    "${site}/same/AppStream/\$basearch/os/"
has "and the log names it" "${root}/said" \
    "appstream: dropping ${site}/behind/AppStream/\$basearch/os/, its revision 199 is behind 200"

stale_case appstream "${own_folder}" newer same
stale_run
holds "a mirror newer than the own server stays" "${root}/stale/appstream" "${site}/newer/AppStream/\$basearch/os/
${site}/same/AppStream/\$basearch/os/"

stale_case appstream "${own_folder}" behind older
stale_run
holds "with every mirror behind the list is the own server alone" "${root}/stale/appstream" "${own_folder}"
has "and the log says so" "${root}/said" "appstream: every mirror is behind ${own_folder}, reading it alone"
stale_repos=${root}/stale-repos
mkdir "${stale_repos}"
printf '[appstream]\nmirrorlist=https://mirrors.almalinux.org/mirrorlist/10.2/appstream?arch=x86_64_v2\nenabled=1\n' \
    > "${stale_repos}/almalinux-appstream.repo"
point_at_mirrors "${stale_repos}" "${root}/stale"
rm -f "${root}/read"
# shellcheck disable=SC2310 # the status is the answer looked for.
dnf_reads "${stale_repos}" 2> /dev/null && status=0 || status=$?
expect "dnf reads the own server's folder" 0 "${status}"
has "and no other" "${root}/read" "${site}/own/AppStream/x86_64/os/repodata/repomd.xml"

stale_case appstream "${own_folder}" http://127.0.0.1:1/AppStream/os/ behind
stale_run
holds "a mirror that does not answer stays" "${root}/stale/appstream" "http://127.0.0.1:1/AppStream/os/"

stale_case appstream http://127.0.0.1:1/own/AppStream/ behind same
stale_run
holds "an own server that does not answer leaves the list as it is" "${root}/stale/appstream" "${site}/behind/AppStream/\$basearch/os/
${site}/same/AppStream/\$basearch/os/"
has "and says so" "${root}/said" "appstream: http://127.0.0.1:1/own/AppStream/ gives no revision, keeping its mirrors"

stale_case appstream "${own_folder}" vault
printf '%s\n' "${site}/vault/AppStream/x86_64/os/" > "${root}/stale/appstream"
revision vault 100
stale_run
holds "a list use_vault set to the vault is left alone" "${root}/stale/appstream" "${site}/vault/AppStream/x86_64/os/"

stale_case rocky-appstream "${own_folder}"
rm "${root}/stale/rocky-appstream"
stale_run
listed=no
[ ! -e "${root}/stale/rocky-appstream" ] || listed=yes
expect "a repository on a metalink gets no list" no "${listed}"
printf '[rocky-appstream]\nmetalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=AppStream-10.1\nenabled=1\n' \
    > "${stale_repos}/rocky.repo"
point_at_mirrors "${stale_repos}" "${root}/stale"
has "and keeps its metalink" "${stale_repos}/rocky.repo" 'metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=AppStream-10.1'

# The list's entries hold dnf's variables, so the stand-in for dnf fills them in before the read. The
# service has the filled path only, and the entry in the list stays as it came.
stale_case appstream "${own_folder}" behind
stale_run
lacks "a mirror's variables are filled in before its revision is read" "${root}/stale/appstream" "${site}/behind/"
has "and the list's entries keep their variables" "${root}/said" "dropping ${site}/behind/AppStream/\$basearch/os/"

# dnf failing to fill in the variables is a fault of the build, never of a mirror, so the step stops
# rather than keep mirrors that may be behind.
stale_case appstream "${own_folder}" behind same
export FILL_FAILS=1
# shellcheck disable=SC2310 # the failure is what is checked.
if stale_run; then stopped=no; else stopped=yes; fi
unset FILL_FAILS
expect "dnf failing to fill in the variables stops the build" yes "${stopped}"
has "and says so" "${root}/said" "appstream: dnf did not fill in the mirrors' variables"

# dnf follows a mirror's redirect, so the step judges what the redirect leads to.
stale_case appstream "${own_folder}" moved same
stale_run
holds "a mirror that redirects to one behind is dropped" "${root}/stale/appstream" "${site}/same/AppStream/\$basearch/os/"

# The step's minute ends before the third mirror here, at 25 s a read, so the last two stay unasked.
revision late 150
revision later 160
stale_case appstream "${own_folder}" behind older late later
echo 0 > "${root}/clock"
stale_run
rm "${root}/clock"
holds "the mirrors not asked within the minute stay" "${root}/stale/appstream" "${site}/late/AppStream/\$basearch/os/
${site}/later/AppStream/\$basearch/os/"

# After a failed attempt, a repository on Rocky Linux's metalink reads the publisher's own server.
own=${root}/own
mkdir "${own}" "${own}/lists"
cat > "${own}/rocky.repo" << 'EOF'
[baseos]
name=Rocky Linux $releasever - BaseOS
#baseurl=http://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/
metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=BaseOS-$releasever$rltype
enabled=1
gpgcheck=1

[baseos-debug]
name=Rocky Linux $releasever - BaseOS - Source
#baseurl=http://dl.rockylinux.org/$contentdir/$releasever/BaseOS/source/tree/
metalink=https://mirrors.rockylinux.org/metalink?arch=source&repo=BaseOS-$releasever$rltype
enabled=0

[nolink]
name=Rocky Linux $releasever - Plus
metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=plus-$releasever$rltype
enabled=1
EOF
cp "${repos}/almalinux-baseos.repo" "${own}/"
cp "${own}/rocky.repo" "${own}/rocky.orig"
cp "${own}/almalinux-baseos.repo" "${own}/almalinux.orig"

use_own_servers "${own}"
has "the metalink's repository reads one https folder of the own server" "${own}/rocky.repo" \
    'baseurl=https://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/'
lines "with the one active baseurl line of its section" "${own}/rocky.repo" 17
expect "and no active metalink or mirror list in it" 0 "$(sed -n '/^\[baseos\]/,/^\[baseos-debug\]/p' "${own}/rocky.repo" | grep -cE '^(metalink|mirrorlist)=')"
expect "and no active http folder" 0 "$(grep -c '^baseurl=http:' "${own}/rocky.repo")"
has "a disabled repository is switched too, and stays disabled" "${own}/rocky.repo" \
    'baseurl=https://dl.rockylinux.org/$contentdir/$releasever/BaseOS/source/tree/'
expect "a repository's own enabled line stays" 2 "$(grep -c '^enabled=1' "${own}/rocky.repo")"
has "a repository without a folder line stays on its metalink" "${own}/rocky.repo" \
    'metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=plus-$releasever$rltype'
expect "AlmaLinux's mirror list is untouched" "$(cat "${own}/almalinux.orig")" "$(cat "${own}/almalinux-baseos.repo")"
cp "${own}/rocky.repo" "${own}/rocky.once"
use_own_servers "${own}"
expect "the switch changes nothing the second time" "$(cat "${own}/rocky.once")" "$(cat "${own}/rocky.repo")"

# from_repositories switches only once an attempt has failed, and leaves the mark the image's build reads.
cp "${own}/rocky.orig" "${own}/rocky.repo"
from_repositories "${own}" "${own}/lists" true
has "a first attempt that succeeds leaves the metalink" "${own}/rocky.repo" \
    'metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=BaseOS-$releasever$rltype'
marked=no
[ ! -e "${own}/lists/.own-servers" ] || marked=yes
expect "and no mark" no "${marked}"
fails_once() {
    [ -e "${own}/tried" ] && return 0
    : > "${own}/tried"
    return 1
}
from_repositories "${own}" "${own}/lists" fails_once 2> /dev/null
lacks "a failed attempt moves the repository off the metalink" "${own}/rocky.repo" \
    'metalink=https://mirrors.rockylinux.org/metalink?arch=$basearch&repo=BaseOS-'
has "to the own server" "${own}/rocky.repo" 'baseurl=https://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/'
marked=no
[ ! -e "${own}/lists/.own-servers" ] || marked=yes
expect "and leaves the mark" yes "${marked}"

# node.sh's copies of the family's files, as that step makes them, read the own server on the mark.
node_repos=${root}/node-repos
mkdir "${node_repos}"
cat > "${node_repos}/rocky.repo" << 'EOF'
[baseos]
name=Rocky Linux $releasever - BaseOS
#baseurl=http://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/
mirrorlist=https://mirrors.rockylinux.org/mirrorlist?arch=$basearch&repo=BaseOS-$releasever$rltype
enabled=1
EOF
use_metalinks "${node_repos}"
point_at_mirrors "${node_repos}" "${own}/lists"
[ ! -e "${own}/lists/.own-servers" ] || use_own_servers "${node_repos}"
has "the image's build reads the own server" "${node_repos}/rocky.repo" \
    'baseurl=https://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/'
lacks "not the metalink" "${node_repos}/rocky.repo" 'metalink='
has "node.sh applies the mark" "${here}/../../pkg/templates/el/node.sh" \
    '[ ! -e "${files}/mirrors/.own-servers" ] || use_own_servers "${repos}"'

if [ "${failures}" -ne 0 ]; then
    echo "el-mirrors: ${failures} of $((passes + failures)) checks failed"
    exit 1
fi
echo "el-mirrors: all ${passes} checks passed"
