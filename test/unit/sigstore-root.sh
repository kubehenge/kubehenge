#!/bin/sh
# Unit test of pkg/templates/node/sigstore-root.sh, with stub cosign, timeout and sleep on PATH that
# record their calls: which failures it retries, how often, what it writes and what it says. Needs
# no network.
#
#   test/unit/sigstore-root.sh
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=$(cd "${here}/../.." && pwd)/pkg/templates/node/sigstore-root.sh
tmp=$(mktemp -d)
trap 'rm -rf "${tmp}"' EXIT

passes=0 failures=0
# same <what> <wanted> <got>
same() {
    if [ "$2" = "$3" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}
# mentions <what> <needle> <text>
mentions() {
    case $3 in
        *"$2"*) passes=$((passes + 1)) ;;
        *)
            printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
            failures=$((failures + 1))
            ;;
    esac
}
# lacks <what> <needle> <text>
lacks() {
    case $3 in
        *"$2"*)
            printf 'FAIL: %s\n  unwanted: %s\n  in:       %s\n' "$1" "$2" "$3"
            failures=$((failures + 1))
            ;;
        *) passes=$((passes + 1)) ;;
    esac
}

# cosign fails the first ${STUB_INIT_FAIL} fetches with ${STUB_INIT_ERROR}, a dropped connection by
# default, and otherwise writes a trusted root over several lines and a signing config where cosign
# does, under a directory named for the mirror. timeout stands for a fetch that never answers in its
# first ${STUB_HANG} calls (exit 124), and runs the command otherwise. sleep returns at once.
mkdir "${tmp}/bin"
cat > "${tmp}/bin/cosign" << 'EOF'
#!/bin/sh
case $1 in
    initialize)
        echo "trust-root" >> "${CALLS}"
        done=$(grep -c '^trust-root$' "${CALLS}")
        if [ "${done}" -le "${STUB_INIT_FAIL:-0}" ]; then
            printf '%s\n' "${STUB_INIT_ERROR:-tuf refresh failed: dial tcp 34.117.62.14:443: i/o timeout}" >&2
            exit 1
        fi
        mkdir -p "${TUF_ROOT}/mirror/targets"
        printf '{\n  "mediaType": "root",\n  "tlogs": []\n}\n' > "${TUF_ROOT}/mirror/targets/trusted_root.json"
        echo '{"mediaType": "config"}' > "${TUF_ROOT}/mirror/targets/signing_config.v0.2.json"
        ;;
    *) exit 2 ;;
esac
EOF
cat > "${tmp}/bin/timeout" << 'EOF'
#!/bin/sh
echo "timeout $*" >> "${CALLS}"
done=$(grep -c '^timeout ' "${CALLS}")
[ "${done}" -gt "${STUB_HANG:-0}" ] || exit 124
shift
exec "$@"
EOF
cat > "${tmp}/bin/sleep" << 'EOF'
#!/bin/sh
echo "sleep $*" >> "${CALLS}"
EOF
chmod +x "${tmp}/bin/"*

# run [<signing config>]: the script's output in out, its status in status and the stubs' calls in
# calls, the number of fetches cosign ran in fetches, and what it wrote in trusted and config (none
# for no file).
run() {
    : > "${tmp}/calls"
    rm -f "${tmp}/root.json" "${tmp}/config.json"
    status=0
    out=$(PATH="${tmp}/bin:${PATH}" CALLS="${tmp}/calls" sh "${script}" "${tmp}/root.json" "$@" 2>&1) || status=$?
    calls=$(cat "${tmp}/calls")
    fetches=$(grep -c '^trust-root$' "${tmp}/calls" || true)
    trusted=none config=none
    [ ! -e "${tmp}/root.json" ] || trusted=$(cat "${tmp}/root.json")
    [ ! -e "${tmp}/config.json" ] || config=$(cat "${tmp}/config.json")
}

run
same "a good run exits 0" 0 "${status}"
same "one fetch" 1 "${fetches}"
same "the trusted root is one line, for gh" '{  "mediaType": "root",  "tlogs": []}' "${trusted}"
mentions "the fetch is bounded" "timeout 60 cosign initialize" "${calls}"
same "no signing config unless asked" none "${config}"

run "${tmp}/config.json"
same "with a signing config exits 0" 0 "${status}"
same "the signing config is written" '{"mediaType": "config"}' "${config}"
same "both from one fetch" 1 "${fetches}"

# Each way a connection fails before an answer, as Go and go-tuf print it.
for error in \
    'tuf refresh failed: Get "https://tuf-repo-cdn.sigstore.dev/15.root.json": dial tcp 34.117.62.14:443: i/o timeout' \
    'tuf refresh failed: Get "https://tuf-repo-cdn.sigstore.dev/15.root.json": EOF' \
    'Get "https://tuf-repo-cdn.sigstore.dev/timestamp.json": read tcp 10.1.0.4:5000->34.117.62.14:443: read: connection reset by peer' \
    'dial tcp 127.0.0.1:443: connect: connection refused' \
    'dial tcp: lookup tuf-repo-cdn.sigstore.dev on 127.0.0.53:53: no such host' \
    'net/http: TLS handshake timeout' \
    'failed to download timestamp.json: unexpected EOF' \
    'failed to download timestamp.json: http status code: 503'; do
    STUB_INIT_FAIL=1 STUB_INIT_ERROR=${error} run
    same "retried after: ${error}" "0 2" "${status} ${fetches}"
done

STUB_INIT_FAIL=99 STUB_INIT_ERROR='tuf refresh failed: Get "https://tuf-repo-cdn.sigstore.dev/15.root.json": EOF' run
same "a closed connection every time exits 1" 1 "${status}"
same "the fetch is capped at five attempts" 5 "${fetches}"
same "a wait between attempts" 4 "$(grep -c '^sleep ' "${tmp}/calls" || true)"
mentions "it names Sigstore as unreachable" "sigstore-root: Sigstore is unreachable: no trusted root after 5 attempts" "${out}"
lacks "a closed connection is no refusal" "refused" "${out}"
same "nothing is written" none "${trusted}"

STUB_HANG=1 run
same "a fetch that never answers once passes" 0 "${status}"
same "the second attempt ran cosign" 1 "${fetches}"
mentions "it says the first gave no answer" "Sigstore gave no answer within 60 s" "${out}"

STUB_HANG=99 run
same "a fetch that never answers exits 1" 1 "${status}"
same "the bound is tried five times" 5 "$(grep -c '^timeout ' "${tmp}/calls" || true)"
mentions "it names Sigstore as unreachable after the cap" "Sigstore is unreachable: no trusted root after 5 attempts" "${out}"

for error in "tuf: failed to decode root.json: expired at 2025-08-19 14:33:09 +0000 UTC" \
    "failed to download timestamp.json: http status code: 403" \
    "tuf: invalid signature on timestamp.json: EOF of the signed part"; do
    STUB_INIT_FAIL=99 STUB_INIT_ERROR=${error} run
    same "refused at once: ${error}" "1 1" "${status} ${fetches}"
    mentions "it prints cosign's message" "${error}" "${out}"
    mentions "it says cosign refused the root" "sigstore-root: cosign refused Sigstore's trusted root" "${out}"
    lacks "it does not call Sigstore unreachable" "unreachable" "${out}"
done

# A PATH that holds no cosign.
status=0
out=$(PATH="${tmp}/none" CALLS="${tmp}/calls" "$(command -v sh)" "${script}" "${tmp}/root.json" 2>&1) || status=$?
same "no cosign exits 1" 1 "${status}"
mentions "no cosign says so" "sigstore-root: cosign is not on PATH" "${out}"

printf '%d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
