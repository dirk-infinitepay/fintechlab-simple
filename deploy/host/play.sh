#!/bin/sh
# Run one cloud bundle as the podman pod "fintechlab", on the machine this
# runs on, as root. It is the whole of what a lab host needs: deploy/aws/lab.sh
# sends it over SSM with the bundle, and on any other Linux box it is
#   scp dist/fintechlab-cloud.tgz host: && ssh host \
#     'mkdir -p r && tar -xzf fintechlab-cloud.tgz -C r && sudo sh r/play.sh'
# docs/deploy-cloud.md is the walkthrough.
#
# Next to this script (one directory, the bundle unpacked):
#   images.tar            every lab image and the e2e suite's, podman save -m
#   fintechlab.yaml       the manifest, rendered for this host (deploy/pod/render)
#   regenerate-keys.yaml  the ConfigMap that asks for new keys
#   e2e.sh                runs the e2e suite inside the pod (lab.sh e2e)
#   REVISION              what was built, for `lab.sh status`
#
# Environment:
#   DATA_VOLUME_ID   an EBS volume id: format it if blank, mount it at DATA_DIR
#                    and keep podman's storage there, so the keys and state
#                    survive a replaced instance. Unset: podman's default storage.
#   DATA_DIR         default /var/lib/fintechlab
#   REGENERATE_KEYS  "true" issues a new PKI and keypairs on this play
#   ROTATE_TOKEN     "true" issues a new lab API token on this play
#   POD_NETWORK      e.g. slirp4netns, for a host whose podman cannot make a
#                    bridge (docs/deploy-pod.md, The podman quirks)
#
# The lab API asks every caller for its token (LAB_API_TOKEN): a lab reached
# over a network is not one to leave open. It is made once, kept on the data
# volume at $DATA_DIR/lab-api-token (root only) and handed to the lab API and
# the console as the ConfigMap deploy/pod/lab-api-token.yaml names; the
# console adds it to what it forwards, so its page needs none.
#
# Idempotent: preparing the host is skipped once done, and a second play
# replaces the containers and keeps the volumes -- the keys a platform was
# configured with, and the token, included.
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
DATA_DIR="${DATA_DIR:-/var/lib/fintechlab}"
REGENERATE_KEYS="${REGENERATE_KEYS:-false}"
ROTATE_TOKEN="${ROTATE_TOKEN:-false}"

step() { echo "› $*"; }
fail() { echo "error: $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || fail "run as root: podman's pod here is a system one, started on boot"
for f in images.tar fintechlab.yaml regenerate-keys.yaml; do
  [ -f "$here/$f" ] || fail "$here/$f is missing: unpack the whole bundle (make cloud-bundle)"
done

# --- the host, once ---------------------------------------------------------

# A first boot may still be installing packages.
if command -v cloud-init >/dev/null 2>&1; then
  cloud-init status --wait >/dev/null 2>&1 || true
fi

if [ -n "${DATA_VOLUME_ID:-}" ]; then
  # Nitro instances (t3, m5, ...) name an EBS volume's NVMe device by its id.
  dev="/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_$(echo "$DATA_VOLUME_ID" | tr -d -)"
  i=0
  while [ ! -e "$dev" ]; do
    i=$((i + 1))
    [ $i -le 60 ] || fail "the data volume $DATA_VOLUME_ID never appeared at $dev"
    sleep 2
  done
  if ! blkid "$dev" >/dev/null 2>&1; then
    step "formatting the new data volume $DATA_VOLUME_ID"
    mkfs.ext4 -q -L fintechlab "$dev"
  fi
  mkdir -p "$DATA_DIR"
  uuid="$(blkid -s UUID -o value "$dev")"
  grep -q "UUID=$uuid " /etc/fstab || echo "UUID=$uuid $DATA_DIR ext4 defaults,nofail 0 2" >>/etc/fstab
  mountpoint -q "$DATA_DIR" || mount "$DATA_DIR"

  # Before podman is installed, so it never starts storage on the root
  # volume. Written once: a host that already has it is left alone.
  if [ ! -f /etc/containers/storage.conf ] || ! grep -q "$DATA_DIR/containers" /etc/containers/storage.conf; then
    command -v podman >/dev/null 2>&1 &&
      echo "warning: podman is already installed; images it stored before this are on the root volume" >&2
    mkdir -p /etc/containers "$DATA_DIR/containers/storage"
    cat >/etc/containers/storage.conf <<EOF
# fintechlab: podman's images, containers and named volumes live on the lab's
# data volume, so the keys and state survive a stop/start and a new instance.
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "$DATA_DIR/containers/storage"
EOF
  fi
else
  mkdir -p "$DATA_DIR"
fi

if ! command -v podman >/dev/null 2>&1; then
  step "installing podman"
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get -o DPkg::Lock::Timeout=300 update -q
    # --force-confold keeps the storage.conf above if the package ships one.
    apt-get -o DPkg::Lock::Timeout=300 install -y -q \
      -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold podman
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q podman
  else
    fail "no apt-get or dnf: install podman (4.9 or later) and run this again"
  fi
fi

# The pod's restartPolicy is Always; podman-restart starts it again after a
# reboot or a stop/start -- once the data volume is mounted.
if [ ! -f /etc/systemd/system/podman-restart.service.d/fintechlab.conf ]; then
  mkdir -p /etc/systemd/system/podman-restart.service.d
  printf '[Unit]\nRequiresMountsFor=%s\n' "$DATA_DIR" >/etc/systemd/system/podman-restart.service.d/fintechlab.conf
  systemctl daemon-reload
fi
systemctl enable podman-restart.service >/dev/null 2>&1 ||
  echo "warning: could not enable podman-restart.service; the pod will not come back after a reboot" >&2

podman --version

# --- the lab ------------------------------------------------------------------

step "loading images"
podman load -q -i "$here/images.tar"
rm -f "$here/images.tar" # the largest file; podman has it now

# The lab API's token: made once, kept with the lab's state.
token_file="$DATA_DIR/lab-api-token"
if [ "$ROTATE_TOKEN" = true ] || [ ! -s "$token_file" ]; then
  [ -s "$token_file" ] && step "with a new lab API token: every caller holding the old one is refused"
  (umask 077 && od -An -tx1 -N32 /dev/urandom | tr -d ' \n' >"$token_file")
fi
(umask 077 && cat >"$DATA_DIR/lab-api-token.yaml" <<EOF
# Written by play.sh from $token_file: the lab API asks for it.
apiVersion: v1
kind: ConfigMap
metadata:
  name: fintechlab-lab-api-token
data:
  lab-api-token: "$(cat "$token_file")"
EOF
)

# kube play looks in its working directory for a directory named like each
# image, even with --build=false (docs/deploy-pod.md). This one holds files.
cd "$here"
set -- --replace --build=false --configmap "$DATA_DIR/lab-api-token.yaml"
if [ "$REGENERATE_KEYS" = true ]; then
  step "with new keys: every copy of the old ones a platform holds stops working"
  set -- "$@" --configmap regenerate-keys.yaml
fi
if [ -n "${POD_NETWORK:-}" ]; then
  set -- "$@" --network "$POD_NETWORK"
fi
step "playing the pod"
podman kube play "$@" fintechlab.yaml

cp "$here/REVISION" "$DATA_DIR/current-revision" 2>/dev/null || true
cp "$here/fintechlab.yaml" "$DATA_DIR/current.yaml"
# Images no container uses any more: the previous play's.
podman image prune --force >/dev/null

step "waiting for the lab API and the console"
for svc in lab-api:8097 console:8090; do
  i=0
  until curl -fsS -o /dev/null "http://127.0.0.1:${svc#*:}/health" 2>/dev/null; do
    i=$((i + 1))
    [ $i -le 30 ] || fail "${svc%%:*} did not answer on :${svc#*:} within a minute: podman logs fintechlab-${svc%%:*}"
    sleep 2
  done
done
# The token is asked for: an answer without it is a lab left open.
code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8097/v1/services)"
[ "$code" = 401 ] || fail "the lab API answered $code without its token, want 401: it is not asking for one"

podman pod ps --filter name=^fintechlab$
podman ps --pod --filter pod=fintechlab --format '{{.Names}}\t{{.Status}}'
echo "fintechlab is up: $(cat "$DATA_DIR/current-revision" 2>/dev/null || echo 'unknown revision')"
