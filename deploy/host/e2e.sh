#!/bin/sh
# Run the e2e suite (cmd/e2e) against the lab on this host, from inside its
# pod, as root: deploy/aws/lab.sh e2e sends it over SSM; on another host,
#   sudo sh <bundle>/e2e.sh
#
# Inside the pod, because the suite plays the platform on every vendor's own
# surface *and* hands B4B its callback on the settlement stand-in: the lab
# has to reach what the suite names, which from inside is the pod's own
# service names -- the same addresses make e2e-docker uses on the compose
# network. Nothing has to be opened to the operator's address. It steers the
# lab through the lab API with its token, like any agent.
#
# Environment: E2E_ONLY (a comma-separated scenario list), E2E_TIMEOUT, as
# cmd/e2e reads them; DATA_DIR as play.sh's.
set -eu

DATA_DIR="${DATA_DIR:-/var/lib/fintechlab}"
[ "$(id -u)" = 0 ] || { echo "error: run as root, like play.sh" >&2; exit 1; }
podman pod exists fintechlab || { echo "error: no pod fintechlab here: play the bundle first" >&2; exit 1; }

LAB_API_TOKEN="$(cat "$DATA_DIR/lab-api-token" 2>/dev/null || true)"
export LAB_API_TOKEN

# -e NAME with no value passes this process's own, so the token never
# appears on a command line.
exec podman run --rm --pod fintechlab \
  -v fintechlab-keys:/keys:ro \
  -e LAB_KEYS_DIR=/keys \
  -e PAYMENT_API_URL=http://payment-api:8080 \
  -e BANK_URL=http://bank:8081 \
  -e NOTIFIER_URL=http://notifier:8082 \
  -e RECEIVER_URL=https://receiver:8443 \
  -e RECEIVER_DELIVERY_URL=https://receiver:8443 \
  -e SETTLEMENT_URL=http://settlement:8083 \
  -e BANKING_CIRCLE_URL=https://banking-circle:8085 \
  -e B4B_URL=http://b4b:8086 \
  -e LAB_API_URL=http://lab-api:8097 \
  -e LAB_API_TOKEN \
  -e B4B_JWT_PRIVATE_KEY_PATH=/keys/b4b-keys/private.pem \
  -e B4B_JWT_KEY_ID=b4b-mock-1 \
  -e CA_FILE=/keys/certs/ca.pem \
  -e CLIENT_CERT=/keys/certs/client.pem \
  -e CLIENT_KEY=/keys/certs/client-key.pem \
  -e WORLDLINE_SFTP_HOST=worldline \
  -e WORLDLINE_SFTP_PASSWORD=sim-sftp-dev-only \
  -e WORLDLINE_PGP_PRIVATE_KEY_PATH=/keys/wlsftp-keys/worldline_private.asc \
  ${E2E_ONLY:+-e E2E_ONLY="$E2E_ONLY"} \
  ${E2E_TIMEOUT:+-e E2E_TIMEOUT="$E2E_TIMEOUT"} \
  localhost/fintechlab/e2e:v1
