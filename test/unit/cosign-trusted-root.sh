#!/bin/sh
# Every Sigstore client of the build, the CI and the node images reads trust material fetched once,
# since a client that fetches its own fails on one dropped connection to Sigstore's TUF server:
# cosign verify a trusted root, cosign sign that and a signing config, gh attestation verify a custom
# trusted root. A command continued over several lines is read as one. Each workflow job that runs
# the fetch also checks out the file that does it.
#
#   test/unit/cosign-trusted-root.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "${root}"
helper=pkg/templates/node/sigstore-root.sh

found=0 bad=0
# fail <message>
fail() {
    printf 'FAIL: %s\n' "$1"
    bad=$((bad + 1))
}

# The workflows, and the shell scripts of every folder the shell linters read, at any depth.
files=$({
    find .github/workflows -name '*.yml' -type f
    find appliance charts pkg scripts test website -name node_modules -prune -o -name '*.sh' -type f -print
} | sed 's|^\./||' | sort)
[ -n "${files}" ] || fail "no file to read"

while read -r file; do
    [ "${file}" != test/unit/cosign-trusted-root.sh ] || continue
    commands=$(awk '
        { line = acc $0; acc = "" }
        /\\$/ { acc = substr(line, 1, length(line) - 1); next }
        line ~ /^[[:space:]]*#/ { next }
        line ~ /cosign"? (verify|sign) / || line ~ /gh attestation verify / { print line }' "${file}")
    [ -n "${commands}" ] || continue
    while read -r command; do
        found=$((found + 1))
        case ${command} in
            *"gh attestation verify "*)
                case ${command} in
                    *--custom-trusted-root*) ;;
                    *) fail "${file}: gh reads no custom trusted root: ${command}" ;;
                esac
                ;;
            *" sign "*)
                case ${command} in
                    *--trusted-root*--signing-config* | *--signing-config*--trusted-root*) ;;
                    *) fail "${file}: cosign signs without a trusted root and signing config: ${command}" ;;
                esac
                ;;
            *)
                case ${command} in
                    *--trusted-root*) ;;
                    *) fail "${file}: cosign verifies with no trusted root: ${command}" ;;
                esac
                ;;
        esac
    done << EOF
${commands}
EOF
done << EOF
${files}
EOF

# Each workflow job with a Sigstore client runs the fetch, or downloads the artifact of a job that
# ran it. Each job that runs the fetch checks the file out: a sparse checkout lists it, or the job
# checks out the whole repository.
for file in .github/workflows/*.yml; do
    jobs=$(awk -v helper="${helper}" '
        /^jobs:/ { in_jobs = 1; next }
        in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { finish(); job = $1; sub(/:$/, "", job); next }
        job == "" { next }
        index($0, "actions/checkout@") { checkout = 1 }
        /sparse-checkout:/ { sparse = 1 }
        # The sparse checkout lists the file alone on its line, and a step that runs it passes a file.
        NF == 1 && $1 == helper { listed = 1 }
        NF > 1 && index($0, helper) { runs = 1 }
        /name: sigstore-trusted-root$/ { downloads = 1 }
        /cosign"? (verify|sign) / || /gh attestation verify / { clients = 1 }
        function finish() {
            if (job != "" && runs && (!checkout || (sparse && !listed))) print job ":checkout"
            if (job != "" && clients && !runs && !downloads) print job ":fetch"
            job = ""; checkout = 0; sparse = 0; listed = 0; runs = 0; downloads = 0; clients = 0
        }
        END { finish() }' "${file}")
    for job in ${jobs}; do
        case ${job} in
            *:checkout) fail "${file}: job ${job%:*} runs ${helper} without checking it out" ;;
            *) fail "${file}: job ${job%:*} runs a Sigstore client without fetching its trust root" ;;
        esac
    done
done

printf '%d Sigstore client commands, %d failed\n' "${found}" "${bad}"
[ "${found}" -gt 0 ] && [ "${bad}" -eq 0 ]
