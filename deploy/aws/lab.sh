#!/usr/bin/env bash
# The lab on AWS: one CloudFormation stack (deploy/aws/fintechlab.yaml), one
# host, one podman pod -- the cloud profile, without the lab clock.
# docs/deploy-cloud.md is the walkthrough.
#
#   deploy/aws/lab.sh infra      create or update the stack
#   deploy/aws/lab.sh deploy     build the cloud bundle, hand it to the host, play it
#   deploy/aws/lab.sh status     the stack, the host, and what it is running
#   deploy/aws/lab.sh env [svc]  download the platform's .env from the console
#   deploy/aws/lab.sh token      print the lab API's token (FINTECH_SIM_LAB_TOKEN)
#   deploy/aws/lab.sh e2e        run the e2e suite inside the deployed pod
#   deploy/aws/lab.sh console    forward the console to 127.0.0.1:8090 over SSM
#   deploy/aws/lab.sh shell      a root-capable shell on the host, over SSM
#   deploy/aws/lab.sh logs <svc> the last lines of one container's log
#   deploy/aws/lab.sh stop|start the host (state in memory goes with a stop)
#   deploy/aws/lab.sh destroy    delete the stack and everything in it
#
# Configuration: deploy/aws/lab.env (copy lab.env.example), or the same
# variables in the environment, which win. Nothing here touches anything the
# stack did not create.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"

# The environment wins over the file: remember what was set before sourcing.
conf="${LAB_CONFIG:-deploy/aws/lab.env}"
if [ -f "$conf" ]; then
  saved="$(export -p | grep -E '^declare -x (AWS_PROFILE|AWS_REGION|LAB_[A-Z_]+|REGENERATE_KEYS|ROTATE_TOKEN|E2E_[A-Z]+)=' || true)"
  set -a
  # shellcheck disable=SC1090
  . "$conf"
  set +a
  eval "$saved"
fi

export AWS_REGION="${AWS_REGION:-eu-west-1}"
[ -n "${AWS_PROFILE:-}" ] && export AWS_PROFILE
export AWS_PAGER=""
STACK="${LAB_STACK:-fintechlab-dev}"
TEMPLATE=deploy/aws/fintechlab.yaml
# The template's VpcCidr default: "nobody outside the lab's own network".
CLOSED_CIDR=10.77.0.0/24
UBUNTU_AMI_PARAM=/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The header comment above, however long it grows.
usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; exit "${1:-0}"; }

account() {
  local acct
  acct="$(aws sts get-caller-identity --query Account --output text 2>/dev/null)" ||
    die "no usable AWS credentials (aws sso login, or set AWS_PROFILE in $conf)"
  if [ -n "${LAB_ACCOUNT_ID:-}" ] && [ "$acct" != "$LAB_ACCOUNT_ID" ]; then
    die "these credentials are for account $acct; LAB_ACCOUNT_ID says $LAB_ACCOUNT_ID. Switch AWS_PROFILE."
  fi
  say "account $acct · $AWS_REGION · stack $STACK"
}

stack_exists() { aws cloudformation describe-stacks --stack-name "$STACK" >/dev/null 2>&1; }

output() {
  aws cloudformation describe-stacks --stack-name "$STACK" \
    --query "Stacks[0].Outputs[?OutputKey=='$1'].OutputValue" --output text
}

need_stack() { stack_exists || die "no stack $STACK in $AWS_REGION: run 'lab.sh infra' first"; }

# "a, b,a" -> "a,b": CloudFormation names one rule per entry, so a repeat
# would be two resources with one name.
# shellcheck disable=SC2020 # two characters, each to a newline: intended
csv() { tr ', ' '\n\n' <<<"$1" | awk 'NF && !seen[$0]++' | paste -sd, -; }

instance_state() {
  aws ec2 describe-instances --instance-ids "$1" \
    --query 'Reservations[0].Instances[0].State.Name' --output text
}

wait_ssm() {
  local id="$1" state ping i
  state="$(instance_state "$id")"
  [ "$state" = running ] || die "the host is $state: 'lab.sh start' first"
  for i in $(seq 1 72); do
    ping="$(aws ssm describe-instance-information --filters "Key=InstanceIds,Values=$id" \
      --query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || true)"
    [ "$ping" = Online ] && return 0
    [ "$i" = 1 ] && say "waiting for the host to report to SSM (a new instance takes a minute or two)"
    sleep 5
  done
  die "the host never came online in SSM: check it has outbound internet access"
}

# run_remote <instance> <comment> <script>: run a shell script on the host
# as root, follow it to the end, print what it printed. Non-zero if it failed.
run_remote() {
  local id="$1" comment="$2" script="$3" params cid status i
  params="$(python3 -c 'import json,sys; print(json.dumps({"commands": sys.argv[1].split("\n"), "executionTimeout": ["1800"]}))' "$script")"
  cid="$(aws ssm send-command --instance-ids "$id" --document-name AWS-RunShellScript \
    --comment "${comment:0:100}" --timeout-seconds 60 --parameters "$params" \
    --query Command.CommandId --output text)"
  status=""
  for i in $(seq 1 900); do
    status="$(aws ssm get-command-invocation --command-id "$cid" --instance-id "$id" \
      --query Status --output text 2>/dev/null || echo Pending)"
    case "$status" in
      Success | Failed | Cancelled | TimedOut | Cancelling) break ;;
    esac
    sleep 2
  done
  aws ssm get-command-invocation --command-id "$cid" --instance-id "$id" \
    --query StandardOutputContent --output text
  local err
  err="$(aws ssm get-command-invocation --command-id "$cid" --instance-id "$id" \
    --query StandardErrorContent --output text)"
  [ -n "$err" ] && [ "$err" != None ] && printf '%s\n' "$err" >&2
  [ "$status" = Success ] || { say "($comment: $status, command $cid)" >&2; return 1; }
}

# --- commands -------------------------------------------------------------------

cmd_infra() {
  account
  local params=() ami
  if ! stack_exists || [ "${LAB_REFRESH_IMAGE:-}" = 1 ]; then
    # Resolved once, then kept: an image that changed under the stack would
    # replace the host. LAB_REFRESH_IMAGE=1 takes the current one on purpose
    # (the data volume keeps the keys).
    ami="${LAB_IMAGE_ID:-$(aws ssm get-parameter --name "$UBUNTU_AMI_PARAM" --query Parameter.Value --output text)}"
    params+=("ImageId=$ami")
    say "host image $ami"
  fi
  local cidrs
  cidrs="$(csv "${LAB_ALLOWED_CIDRS:-}")"
  if [ -z "$cidrs" ]; then
    say "LAB_ALLOWED_CIDRS is empty: nothing is reachable from outside; use 'lab.sh console'"
    cidrs="$CLOSED_CIDR"
  fi
  params+=(
    "AllowedCidrs=$cidrs"
    "InstanceType=${LAB_INSTANCE_TYPE:-t3.small}"
    "DomainName=${LAB_DOMAIN:-}"
    "HostedZoneId=${LAB_HOSTED_ZONE_ID:-}"
  )
  aws cloudformation deploy --stack-name "$STACK" --template-file "$TEMPLATE" \
    --capabilities CAPABILITY_IAM CAPABILITY_AUTO_EXPAND --no-fail-on-empty-changeset \
    --tags Project=fintechlab ManagedBy=fintechlab-simple \
    --parameter-overrides "${params[@]}"
  cmd_outputs
  say ""
  say "next: deploy/aws/lab.sh deploy"
}

cmd_outputs() {
  aws cloudformation describe-stacks --stack-name "$STACK" \
    --query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output text | column -t
}

cmd_deploy() {
  account
  need_stack
  local id host ip bucket vol sans rev key url
  id="$(output InstanceId)" host="$(output PublicHost)" ip="$(output PublicIp)"
  bucket="$(output ReleaseBucket)" vol="$(output DataVolumeId)"
  sans="IP:$ip"

  say "building the cloud bundle for $host"
  make --no-print-directory cloud-bundle LAB_PROFILE=cloud \
    LAB_PUBLIC_HOST="$host" LAB_SANS="$sans" \
    LAB_WEBHOOK_ALLOW="${LAB_WEBHOOK_ALLOW:-}" LAB_SET="${LAB_SET:-}"
  rev="$(head -1 dist/cloud/REVISION | cut -d' ' -f1)"
  key="bundles/$(date -u +%Y%m%dT%H%M%SZ)-$rev.tgz"

  say "uploading $(du -h dist/fintechlab-cloud.tgz | cut -f1) to s3://$bucket/$key"
  aws s3 cp --only-show-errors dist/fintechlab-cloud.tgz "s3://$bucket/$key"
  # The host fetches it by URL, so its role needs no S3 access at all.
  url="$(aws s3 presign "s3://$bucket/$key" --expires-in 3600)"

  wait_ssm "$id"
  say "playing $rev on $id${REGENERATE_KEYS:+ (REGENERATE_KEYS=$REGENERATE_KEYS)}${ROTATE_TOKEN:+ (ROTATE_TOKEN=$ROTATE_TOKEN)}"
  run_remote "$id" "fintechlab deploy $rev" "set -eu
d=/opt/fintechlab/bundle
rm -rf \"\$d\"
mkdir -p \"\$d\"
curl -fsS --retry 3 -o /opt/fintechlab/bundle.tgz '$url'
tar -xzf /opt/fintechlab/bundle.tgz -C \"\$d\"
rm -f /opt/fintechlab/bundle.tgz
DATA_VOLUME_ID='$vol' REGENERATE_KEYS='${REGENERATE_KEYS:-false}' ROTATE_TOKEN='${ROTATE_TOKEN:-false}' sh \"\$d/play.sh\""

  say ""
  say "console:  http://$host:8090   (from LAB_ALLOWED_CIDRS; otherwise 'lab.sh console')"
  say "lab API:  http://$host:8097/v1 with its token ('lab.sh token'): FINTECH_SIM_LAB_URL, FINTECH_SIM_LAB_TOKEN"
  say "platform: 'lab.sh env' downloads its .env, addressed to $host"
  say "check:    'lab.sh e2e' runs the e2e suite inside the pod"
  [ "${REGENERATE_KEYS:-}" = true ] && say "new keys: every copy a platform held stops working -- download its .env again"
  [ "${ROTATE_TOKEN:-}" = true ] && say "new token: every caller holding the old one is refused -- 'lab.sh token'"
  return 0
}

cmd_status() {
  account
  need_stack
  local id
  id="$(output InstanceId)"
  cmd_outputs
  say ""
  say "host $id: $(instance_state "$id")"
  wait_ssm "$id"
  run_remote "$id" "fintechlab status" "cat /var/lib/fintechlab/current-revision 2>/dev/null || echo 'nothing deployed yet'
command -v podman >/dev/null && podman ps --pod --filter pod=fintechlab --format '{{.Names}}\t{{.Status}}' || true"
}

cmd_env() {
  [ -z "${1:-}" ] || [[ "$1" =~ ^[a-z0-9-]+$ ]] || die "service names are lowercase, digits and dashes"
  need_stack
  local host path name id b64
  host="$(output PublicHost)"
  # Through the console, which adds the lab API's token: the operator needs
  # only what reaches :8090.
  path="/v1/connect/env${1:+/$1}"
  name="fintechlab${1:+-$1}.env"
  # The file names $host however it is fetched: the console knows its own
  # address (CONSOLE_CONNECT_HOST).
  if curl -fsS --max-time 8 -o "$name" "http://$host:8090$path" 2>/dev/null; then
    say "saved $name (addresses: $host)"
    return 0
  fi
  # Not reachable from here (LAB_ALLOWED_CIDRS): ask the host for it over
  # SSM. Compressed, because SSM returns at most 24,000 characters of
  # output; a cut-off answer fails gunzip rather than writing half a key.
  say "the console at $host:8090 is not reachable from here; fetching it over SSM"
  id="$(output InstanceId)"
  wait_ssm "$id"
  b64="$(run_remote "$id" "fintechlab env" "set -e
curl -fsS -o /tmp/fintechlab.env http://127.0.0.1:8090$path
gzip -9c /tmp/fintechlab.env | base64 -w0
rm -f /tmp/fintechlab.env")" || die "the console on the host did not answer: lab.sh status"
  if ! printf '%s' "$b64" | tr -d '\n ' | base64 -d 2>/dev/null | gunzip >"$name.part" 2>/dev/null || [ ! -s "$name.part" ]; then
    rm -f "$name.part"
    die "the .env did not arrive whole over SSM; 'lab.sh console' in another terminal, then curl -fsS -o $name http://127.0.0.1:8090$path"
  fi
  mv "$name.part" "$name"
  say "saved $name (addresses: $host)"
}

cmd_token() {
  need_stack
  local id
  id="$(output InstanceId)"
  wait_ssm "$id"
  run_remote "$id" "fintechlab token" "cat /var/lib/fintechlab/lab-api-token"
}

cmd_e2e() {
  need_stack
  local id
  id="$(output InstanceId)"
  wait_ssm "$id"
  [ -n "${E2E_ONLY:-}" ] && [[ ! "$E2E_ONLY" =~ ^[a-z0-9,_-]+$ ]] && die "E2E_ONLY is scenario names, comma-separated"
  [ -n "${E2E_TIMEOUT:-}" ] && [[ ! "$E2E_TIMEOUT" =~ ^[0-9]+[smh]$ ]] && die "E2E_TIMEOUT is a duration, e.g. 10m"
  say "running the e2e suite inside the pod${E2E_ONLY:+ ($E2E_ONLY)}"
  run_remote "$id" "fintechlab e2e" "E2E_ONLY='${E2E_ONLY:-}' E2E_TIMEOUT='${E2E_TIMEOUT:-}' sh /opt/fintechlab/bundle/e2e.sh"
}

cmd_console() {
  need_stack
  command -v session-manager-plugin >/dev/null ||
    die "needs the AWS Session Manager plugin: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"
  local port="${LAB_CONSOLE_PORT:-8090}"
  say "console on http://127.0.0.1:$port -- Ctrl-C to close"
  aws ssm start-session --target "$(output InstanceId)" --document-name AWS-StartPortForwardingSession \
    --parameters "portNumber=8090,localPortNumber=$port"
}

cmd_shell() {
  need_stack
  command -v session-manager-plugin >/dev/null || die "needs the AWS Session Manager plugin"
  say "on the host: sudo podman ps --pod; sudo podman logs fintechlab-<service>"
  aws ssm start-session --target "$(output InstanceId)"
}

cmd_logs() {
  [ -n "${1:-}" ] || die "which service? e.g. lab.sh logs banking-circle"
  [[ "$1" =~ ^[a-z0-9-]+$ ]] || die "service names are lowercase, digits and dashes"
  need_stack
  local id
  id="$(output InstanceId)"
  wait_ssm "$id"
  run_remote "$id" "fintechlab logs $1" "podman logs --tail ${LAB_TAIL:-200} fintechlab-$1 2>&1"
}

cmd_stop() {
  need_stack
  local id
  id="$(output InstanceId)"
  say "stopping $id. Keys and files survive; what the vendors hold in memory does not --"
  say "Banking Circle's subscriptions and payments among it: restart the platform after 'start'."
  aws ec2 stop-instances --instance-ids "$id" >/dev/null
  aws ec2 wait instance-stopped --instance-ids "$id"
  say "stopped"
}

cmd_start() {
  need_stack
  local id
  id="$(output InstanceId)"
  aws ec2 start-instances --instance-ids "$id" >/dev/null
  aws ec2 wait instance-running --instance-ids "$id"
  say "running; the pod starts on boot (podman-restart). 'lab.sh status' to check."
}

cmd_destroy() {
  account
  need_stack
  local bucket answer
  bucket="$(output ReleaseBucket)"
  if [ "${CONFIRM:-}" != yes ]; then
    printf 'Delete stack %s: the host, its data volume (every key the lab issued), the bucket and the network?\nType the stack name to confirm: ' "$STACK"
    read -r answer
    [ "$answer" = "$STACK" ] || die "nothing deleted"
  fi
  # CloudFormation will not delete a bucket that still holds anything.
  [ -n "$bucket" ] && aws s3 rm --recursive --only-show-errors "s3://$bucket" || true
  aws cloudformation delete-stack --stack-name "$STACK"
  say "deleting $STACK..."
  aws cloudformation wait stack-delete-complete --stack-name "$STACK"
  say "deleted"
}

cmd="${1:-}"
[ $# -gt 0 ] && shift
case "$cmd" in
  infra) cmd_infra ;;
  deploy) cmd_deploy ;;
  status) cmd_status ;;
  outputs) need_stack && cmd_outputs ;;
  env) cmd_env "$@" ;;
  token) cmd_token ;;
  e2e) cmd_e2e ;;
  console) cmd_console ;;
  shell) cmd_shell ;;
  logs) cmd_logs "$@" ;;
  stop) cmd_stop ;;
  start) cmd_start ;;
  destroy) cmd_destroy ;;
  -h | --help | help | "") usage 0 ;;
  *) say "unknown command: $cmd" >&2; usage 1 ;;
esac
