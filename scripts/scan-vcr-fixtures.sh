#!/usr/bin/env bash
#
# scan-vcr-fixtures.sh checks the recorded kuberecord fixtures (*.vcr.yaml) for
# material that must not be published: bearer tokens, authorization headers
# with values, plaintext private keys, private keys inside JSON Secret bodies,
# service account tokens, kubeconfig key data, and internal hostnames or
# cluster identifiers. It exits non-zero if any category has hits.
#
# Recordings of protobuf responses are base64-encoded and line-wrapped by the
# recorder, so a key inside one is not visible to a line-based grep. gitleaks
# can decode those, but .gitleaks.toml allowlists the recordings because the
# keys in them are the deterministic test key. After re-recording, run gitleaks
# with that allowlist entry removed and check that every hit is a key from
# crypto/deterministic_keys.pem.
#
# Fixtures are recorded from throwaway local k3d clusters with the
# deterministic test signer, so private keys in them are expected. The key
# check passes keys from crypto/deterministic_keys.pem and the fake signer's
# embedded CA key, and fails on any other key. Any other hit is almost always
# a scrubbing gap rather than a real credential. Re-record the fixture rather
# than allowlisting here.

set -uo pipefail

cd "$(dirname "$0")/.."

fixtures=()
while IFS= read -r -d '' f; do fixtures+=("$f"); done < <(git ls-files -z '*.vcr.yaml')
if [ "${#fixtures[@]}" -eq 0 ]; then
  echo "no fixtures found"
  exit 0
fi

status=0

report() {
  local label=$1 hits=$2
  if [ -n "$hits" ]; then
    echo "FAIL: $label"
    echo "$hits" | head -20 | cut -c1-160
    status=1
  else
    echo "ok:   $label"
  fi
}

report "bearer tokens" \
  "$(grep -H -n -E -o -e 'Bearer [A-Za-z0-9._~+/=-]{16,}' "${fixtures[@]}")"

report "authorization header values" \
  "$(grep -H -n -A1 -E -e '^[[:space:]]*Authorization:' "${fixtures[@]}" | grep -E -e '-[[:space:]]+- .{8,}' -e 'Authorization:[[:space:]]+[A-Za-z]+ .{8,}')"

# Private keys show up as plain PEM or as base64 Secret data. LS0tLS1CRUdJTi is
# the base64 prefix of "-----BEGIN". Every key found is compared against the
# known test keys, and only unknown keys are reported.
unknown_private_keys() {
  python3 - "$@" <<'EOF'
import base64, re, sys

PEM = re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----(.*?)-----END [A-Z ]*PRIVATE KEY-----", re.S)
B64 = re.compile(r'"[A-Za-z0-9._-]*key":\s*"(LS0tLS1CRUdJTi[A-Za-z0-9+/=]*)"')

def bodies(text):
    text = text.replace("\\n", "\n")
    return {re.sub(r"[^A-Za-z0-9+/=]", "", m) for m in PEM.findall(text)}

known = set()
for path in ("crypto/deterministic_keys.pem", "crypto/fake_signer.go"):
    known |= bodies(open(path).read())

for path in sys.argv[1:]:
    for n, line in enumerate(open(path, errors="replace"), 1):
        found = bodies(line)
        for value in B64.findall(line):
            try:
                decoded = base64.b64decode(value + "==").decode(errors="replace")
            except ValueError:
                print(f"{path}:{n}: base64 Secret value could not be decoded")
                continue
            found |= bodies(decoded)
        if found - known:
            print(f"{path}:{n}: private key not from the test signers")
EOF
}

# The scanner must fail closed. If it errors, for example because python3 is
# missing or a key file moved, its output is empty and would read as clean.
if key_hits=$(unknown_private_keys "${fixtures[@]}"); then
  report "PEM private keys not from the test signers" "$key_hits"
else
  echo "FAIL: PEM private keys not from the test signers (scanner errored)"
  status=1
fi

report "service account tokens in Secret data" \
  "$(grep -H -n -E -o -e '"token":"[A-Za-z0-9+/=]{16,}' "${fixtures[@]}")"

report "kubeconfig client key data" \
  "$(grep -H -n -E -o -e 'client-key-data|client-certificate-data' "${fixtures[@]}")"

# Public documentation URLs appear inside CRD descriptions, so only internal
# domains and identifiers are flagged. The managed-by label value that existing
# clusters carry is excluded on purpose. It is listed first in the pattern so
# that -o prints the whole label and the grep -v below can drop it.
report "internal hosts or identifiers" \
  "$(grep -H -n -E -o -e 'certificates\.yollout\.cockroachlabs\.cloud|cockroachlabs\.cloud|crl-(prod|staging)-[a-z0-9]+|atlassian\.net|slack\.com|iam\.gserviceaccount\.com|teamcity' "${fixtures[@]}" | grep -v -e 'certificates\.yollout\.cockroachlabs\.cloud')"

exit $status
