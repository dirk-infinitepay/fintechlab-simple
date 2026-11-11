#!/bin/sh
# The pod's init container (deploy/pod/fintechlab.yaml): runs to completion,
# as root, before any service in the pod starts. It does for the pod's named
# volumes what `make certs sftp-dirs key-dirs console-dir b4b-dir bc-dir`
# does for a checkout's bind mounts.
#
# Podman runs it once per `kube play`, not on a pod restart. What exists is
# left alone, so the PKI and keys a platform was configured with survive a
# redeploy -- unless the play asked for new ones (REGENERATE_KEYS, below).
set -eu

mkdir -p /keys/certs /keys/b4b-keys /keys/wlsftp-keys \
  /sftp/out /sftp/staging /sftp/outbound /sftp/inbound /sftp/config /sftp/archive

# Every service runs as 65532 and a fresh named volume belongs to root.
# World-writable for the same reason the Makefile's targets are: the lab's
# own fake material, shared across a UID boundary (docs/security/ca-and-tls.md).
chmod 777 /keys/b4b-keys /keys/wlsftp-keys \
  /sftp /sftp/out /sftp/staging /sftp/outbound /sftp/inbound /sftp/config /sftp/archive \
  /data /b4b-data /bc-data /console-data

# The SFT channel's onboarding config, as a checkout has it in
# sftp/config/. Seeded once: worldline rewrites it when you PUT /config.
if [ ! -f /sftp/config/merchant-onboarding.json ]; then
  cp /seed/merchant-onboarding.json /sftp/config/merchant-onboarding.json
  chmod 666 /sftp/config/merchant-onboarding.json
fi

# REGENERATE_KEYS=true comes from deploy/pod/regenerate-keys.yaml, passed
# to the play with --configmap. Emptying the three directories here, before
# anything starts, is all it takes: generate.sh below issues a new PKI, and
# b4b and worldline generate their keypairs on start when theirs are gone.
# Once per play, so a pod restart or a reboot never quietly repeats it.
if [ "${REGENERATE_KEYS:-false}" = "true" ]; then
  echo "REGENERATE_KEYS=true: discarding every key and certificate in /keys"
  rm -f /keys/certs/* /keys/b4b-keys/* /keys/wlsftp-keys/*
fi

CERTS_DIR=/keys/certs /bin/sh /generate.sh
