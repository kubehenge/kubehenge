#!/bin/sh
# Unit test of keep_management_credentials in test/e2e/common.sh, against a stand-in curl that
# answers as the appliance does: it keeps the credentials and leaves the setup's status for
# job_was, as test/e2e/appliance.sh calls them in turn.
#
#   test/unit/keep-credentials.sh
set -eu

here=$(dirname "$0")
root=$(mktemp -d)
trap 'rm -rf "${root}"' EXIT
mkdir "${root}/bin" "${root}/work"
# shellcheck source=../e2e/common.sh
. "${here}/../e2e/common.sh"

passes=0 failures=0
# same <what> <wanted> <got>. Not expect, which common.sh's setup_status calls.
same() {
    if [ "$3" = "$2" ]; then
        passes=$((passes + 1))
    else
        printf 'FAIL: %s\n  wanted: %s\n  got:    %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}

# A curl that answers /api/setup with a finished setup, and /api/setup/credential with the asked
# credential's YAML, or with ${root}/refused's status and an error when that file exists. Each
# credential asked for is a line of ${root}/asked.
cat > "${root}/bin/curl" << EOF
#!/bin/sh
out= url= data=
while [ \$# -gt 0 ]; do
    case \$1 in
        -o) out=\$2; shift ;;
        -d) data=\$2; shift ;;
        -b | -c | -w | -H | -X | --cacert) shift ;;
        -*) ;;
        *) url=\$1 ;;
    esac
    shift
done
case \$url in
    */api/setup)
        echo '{"job":{"kind":"apply","state":"succeeded"},"ready":true,"allowed":true}' > "\${out}"
        printf 200
        ;;
    */api/setup/credential)
        echo "\${data}" >> "${root}/asked"
        if [ -f "${root}/refused" ]; then
            echo '{"error":"no management cluster"}' > "\${out}"
            cat "${root}/refused"
        else
            printf 'context: %s\n' "\$(echo "\${data}" | jq -r .name)" > "\${out}"
            printf 200
        fi
        ;;
esac
EOF
chmod +x "${root}/bin/curl"
PATH="${root}/bin:${PATH}"
work=${root}/work ui=https://appliance.example ca=/dev/null admin_csrf=t passphrase=p
KUBEHENGE_E2E_KEEP=${root}/keep

# after_setup: job_ended, keep_management_credentials and job_was, as appliance.sh runs them,
# leaving their status in status, their errors in errors and the credentials asked for in asked.
after_setup() {
    rm -f "${root}/asked"
    status=0
    # shellcheck disable=SC2310 # its status is what is checked.
    (job_ended && keep_management_credentials && job_was apply succeeded true) 2> "${root}/errors" || status=$?
    errors=$(cat "${root}/errors")
    asked=$(cat "${root}/asked" 2> /dev/null || true)
    asked=$(echo "${asked}" | jq -rs 'map(.name) | join(" ")')
}

after_setup
kubeconfig=$(cat "${KUBEHENGE_E2E_KEEP}/kubeconfig")
talosconfig=$(cat "${KUBEHENGE_E2E_KEEP}/talosconfig")
same "job_was reads the setup's status after the credentials are kept" "0 kubeconfig talosconfig" "${status}${errors} ${asked}"
same "both credentials kept" "context: kubeconfig context: talosconfig" "${kubeconfig} ${talosconfig}"

printf 403 > "${root}/refused"
after_setup
kept=$(ls "${KUBEHENGE_E2E_KEEP}")
same "a refused credential leaves job_was its status too" "0 kubeconfig talosconfig" "${status}${errors} ${asked}"
same "a refused credential is not kept" "" "${kept}"

unset KUBEHENGE_E2E_KEEP
after_setup
same "without a folder to keep them in, nothing is asked" "0 " "${status}${errors} ${asked}"

printf 'keep-credentials: %d passed, %d failed\n' "${passes}" "${failures}"
[ "${failures}" -eq 0 ]
