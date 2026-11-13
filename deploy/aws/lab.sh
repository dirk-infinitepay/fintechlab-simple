#!/usr/bin/env bash
# The lab on AWS: one CloudFormation stack (deploy/aws/fintechlab.yaml), one
# host, one podman pod -- the cloud profile, without the lab clock.
# docs/deploy-cloud.md is the walkthrough.
#
#   deploy/aws/lab.sh infra      create or update the stack
#   deploy/aws/lab.sh deploy     build the cloud bundle, hand it to the host, play it
#   deploy/aws/lab.sh status     the stack, the host, and what it is running
#   deploy/aws/lab.sh allow [cidr] [note]  let an address in (default: yours)
#   deploy/aws/lab.sh allow --console [cidr] [note]
#                                          the console and the vendors' pages only
#   deploy/aws/lab.sh deny <cidr>          take one off whichever list holds it
#   deploy/aws/lab.sh allowed              who may reach the lab
#   deploy/aws/lab.sh env [svc]  download the platform's .env from the console
#   deploy/aws/lab.sh token      print the lab API's token (FINTECH_SIM_LAB_TOKEN)
#   deploy/aws/lab.sh e2e        run the e2e suite inside the deployed pod
#   deploy/aws/lab.sh console    forward the console to 127.0.0.1:8090 over SSM
#   deploy/aws/lab.sh tunnel <port>...  this machine's ports, reachable from the lab
#                                as host.containers.internal:<port+10000>
#   deploy/aws/lab.sh shell      a root-capable shell on the host, over SSM
#   deploy/aws/lab.sh logs <svc> the last lines of one container's log
#   deploy/aws/lab.sh stop|start the host (state in memory goes with a stop)
#   deploy/aws/lab.sh destroy    delete the stack and everything in it
#
# Configuration: deploy/aws/lab.env (copy lab.env.example), or the file
# LAB_CONFIG names -- another lab, say LAB_CONFIG=deploy/aws/lab-sbx.env --
# or the same variables in the environment, which win. Nothing here touches
# anything the stack did not create.
set -euo pipefail

# Where you ran it from: lab.sh env saves there, not in the checkout.
here="$PWD"
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
  local params=() ami status saved_allow="" saved_console="" n
  local allow_size="${LAB_ALLOWLIST_SIZE:-20}" console_size="${LAB_CONSOLE_LIST_SIZE:-0}" rules
  # The lists' sizes are what the security group's rules can hold: the
  # allowlist is named by three rules, the console list by one.
  [[ "$allow_size" =~ ^[1-9][0-9]*$ ]] || die "LAB_ALLOWLIST_SIZE is how many addresses the allowlist holds, at least 1: not '$allow_size'"
  [[ "$console_size" =~ ^[0-9]+$ ]] || die "LAB_CONSOLE_LIST_SIZE is how many addresses the console list holds, 0 for none: not '$console_size'"
  rules="$(rules_per_security_group)"
  [ $((3 * allow_size + console_size)) -le "$rules" ] ||
    die "3 x LAB_ALLOWLIST_SIZE ($allow_size) + LAB_CONSOLE_LIST_SIZE ($console_size) is $((3 * allow_size + console_size)) rules; a security group holds $rules here"
  # A stack whose first create failed is left in ROLLBACK_COMPLETE, holding
  # nothing, and can only be deleted: do that, and create it again.
  status="$(aws cloudformation describe-stacks --stack-name "$STACK" --query 'Stacks[0].StackStatus' --output text 2>/dev/null || true)"
  if [ "$status" = ROLLBACK_COMPLETE ]; then
    say "$STACK failed its first create and holds nothing: deleting it to start again"
    aws cloudformation delete-stack --stack-name "$STACK"
    aws cloudformation wait stack-delete-complete --stack-name "$STACK"
  fi
  if ! stack_exists || [ "${LAB_REFRESH_IMAGE:-}" = 1 ]; then
    # Resolved once, then kept: an image that changed under the stack would
    # replace the host. LAB_REFRESH_IMAGE=1 takes the current one on purpose
    # (the data volume keeps the keys).
    ami="${LAB_IMAGE_ID:-$(aws ssm get-parameter --name "$UBUNTU_AMI_PARAM" --query Parameter.Value --output text)}"
    params+=("ImageId=$ami")
    say "host image $ami"
  fi
  # The lists' entries are people's, not the template's: kept aside before
  # an update and put back after it, should the update touch them. A list
  # cannot shrink below what it holds, nor go with people still on it.
  if stack_exists; then
    saved_allow="$(entries_of AllowListId)"
    saved_console="$(entries_of ConsoleListId)"
    n="$(grep -c . <<<"$saved_allow" || true)"
    [ "$n" -le "$allow_size" ] || die "the allowlist holds $n addresses, more than LAB_ALLOWLIST_SIZE ($allow_size): 'lab.sh deny' some first"
    n="$(grep -c . <<<"$saved_console" || true)"
    [ "$n" -le "$console_size" ] || die "the console list holds $n addresses, more than LAB_CONSOLE_LIST_SIZE ($console_size): 'lab.sh deny' some first"
  fi
  params+=(
    "InstanceType=${LAB_INSTANCE_TYPE:-t3.small}"
    "DomainName=${LAB_DOMAIN:-}"
    "HostedZoneId=${LAB_HOSTED_ZONE_ID:-}"
    "AllowListSize=$allow_size"
    "ConsoleListSize=$console_size"
  )
  aws cloudformation deploy --stack-name "$STACK" --template-file "$TEMPLATE" \
    --capabilities CAPABILITY_IAM --no-fail-on-empty-changeset \
    --tags Project=fintechlab ManagedBy=fintechlab-simple \
    --parameter-overrides "${params[@]}"

  # LAB_ALLOWED_CIDRS seeds the allowlist: added when missing, never removed.
  local seed=() cidr
  for cidr in $(csv "${LAB_ALLOWED_CIDRS:-}" | tr , ' '); do
    cidr="$(normalize_cidr "$cidr")" || die "LAB_ALLOWED_CIDRS: $cidr"
    seed+=("$cidr")
  done
  restore_entries AllowListId "$saved_allow" "${seed[@]}"
  if [ "$console_size" -gt 0 ]; then
    restore_entries ConsoleListId "$saved_console"
  fi
  cmd_outputs
  say ""
  cmd_allowed
  say ""
  say "next: deploy/aws/lab.sh deploy"
}

# --- the allowlist ------------------------------------------------------------
# Managed prefix lists the template creates and never fills: entries are
# people's, edited here or in the VPC console (Managed prefix lists), and an
# edit is not drift. The allowlist (AllowListId) opens everything a platform
# needs; the console list (ConsoleListId, when LAB_CONSOLE_LIST_SIZE makes
# one) opens the console and the vendors' pages only. The stack's
# AllowListEditorsPolicyArn output is a policy that lets a role edit these
# lists and nothing else.

# allow_entries <list>: "cidr<TAB>description" per line.
allow_entries() {
  aws ec2 get-managed-prefix-list-entries --prefix-list-id "$1" \
    --query 'Entries[].[Cidr,Description]' --output text
}

# list_id <AllowListId|ConsoleListId>: the list's id, empty when the stack
# has no such list.
list_id() {
  local id
  id="$(output "$1")"
  [ "$id" = None ] && id=""
  printf '%s' "$id"
}

# list_label <AllowListId|ConsoleListId>: what to call it.
list_label() { [ "$1" = ConsoleListId ] && echo "console list" || echo allowlist; }

# entries_of <AllowListId|ConsoleListId>: its entries, nothing when absent.
entries_of() {
  local id
  id="$(list_id "$1")"
  [ -z "$id" ] || allow_entries "$id"
}

# has_entry <entries> <cidr>: whether the entries hold exactly that CIDR.
has_entry() { awk -F'\t' -v c="$2" '$1 == c { found = 1 } END { exit !found }' <<<"$1"; }

# list_size <list>: how many entries it holds at most.
list_size() {
  aws ec2 describe-managed-prefix-lists --prefix-list-ids "$1" \
    --query 'PrefixLists[0].MaxEntries' --output text
}

# rules_per_security_group: the account's VPC quota (60 unless raised).
rules_per_security_group() {
  local v
  v="$(aws service-quotas get-service-quota --service-code vpc --quota-code L-0EA8095F \
    --query Quota.Value --output text 2>/dev/null || true)"
  [[ "$v" =~ ^[0-9]+ ]] && echo "${BASH_REMATCH[0]}" || echo 60
}

# restore_entries <AllowListId|ConsoleListId> <saved entries> [cidr...]: put
# back what an update of the stack dropped from the list, and add each CIDR
# given that it lacks.
restore_entries() {
  local key="$1" saved="$2" id now add=() cidr desc
  shift 2
  id="$(list_id "$key")"
  [ -n "$id" ] || return 0
  now="$(allow_entries "$id")"
  while IFS=$'\t' read -r cidr desc; do
    [ -n "$cidr" ] || continue
    has_entry "$now" "$cidr" || add+=("$cidr" "${desc:-restored by lab.sh infra}")
  done <<<"$saved"
  for cidr in "$@"; do
    has_entry "$now" "$cidr" || has_entry "$saved" "$cidr" || add+=("$cidr" "$(entry_note "lab.env")")
  done
  if [ ${#add[@]} -gt 0 ]; then
    say "$(list_label "$key"): adding $(( ${#add[@]} / 2 )) entr$([ ${#add[@]} -eq 2 ] && echo y || echo ies)"
    modify_allowlist "$id" add "${add[@]}"
  fi
}

# normalize_cidr <addr|cidr>: an IPv4 CIDR, /32 for a bare address. Nothing
# broader than /16: the console asks for no login, so the list is its gate.
normalize_cidr() {
  local c="$1"
  [[ "$c" == */* ]] || c="$c/32"
  [[ "$c" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[12][0-9]|3[0-2])$ ]] || { echo "$1 is not an IPv4 address or CIDR" >&2; return 1; }
  [ "${c#*/}" -ge 16 ] || { echo "$c is broader than /16: the console has no login, so the allowlist is all that keeps it private" >&2; return 1; }
  printf '%s\n' "$c"
}

# entry_note <note>: who added an entry, when, and why -- what the next
# person reading the list needs to know whether it can go.
entry_note() {
  local who
  who="$(aws sts get-caller-identity --query Arn --output text 2>/dev/null | sed 's|.*/||')"
  printf '%s' "${who:-unknown} $(date -u +%Y-%m-%d) ${1:-}" | tr -c 'A-Za-z0-9 ._:/()#,@+=-' '_' | cut -c1-255
}

# modify_allowlist <list> add <cidr> <description> ... | remove <cidr> ...
# A prefix list is versioned: a change names the version it changes, and a
# change made meanwhile by someone else is retried on the new one.
modify_allowlist() {
  local list="$1" op="$2" json version state i err
  shift 2
  json="$(python3 -c '
import json, sys
op, rest = sys.argv[1], sys.argv[2:]
if op == "add":
    print(json.dumps([{"Cidr": c, "Description": d} for c, d in zip(rest[::2], rest[1::2])]))
else:
    print(json.dumps([{"Cidr": c} for c in rest]))' "$op" "$@")"
  for i in 1 2 3 4 5 6; do
    read -r version state <<<"$(aws ec2 describe-managed-prefix-lists --prefix-list-ids "$list" \
      --query 'PrefixLists[0].[Version,State]' --output text)"
    case "$state" in *in-progress) sleep 2; continue ;; esac
    if err="$(aws ec2 modify-managed-prefix-list --prefix-list-id "$list" --current-version "$version" \
      "--$op-entries" "$json" 2>&1 >/dev/null)"; then
      while :; do
        state="$(aws ec2 describe-managed-prefix-lists --prefix-list-ids "$list" --query 'PrefixLists[0].State' --output text)"
        case "$state" in
          modify-complete | create-complete) return 0 ;;
          *failed*) die "the allowlist change failed ($state)" ;;
        esac
        sleep 1
      done
    fi
    grep -qE 'IncorrectState|VersionMismatch|InvalidPrefixListModification.*version' <<<"$err" || die "$err"
    sleep 2
  done
  die "the allowlist kept changing under us; try again"
}

# cmd_allow [--console] [cidr] [note]: onto the allowlist, or with --console
# onto the console list. An address on the console list that is allowed
# fully moves to the allowlist; one already on the allowlist reaches the
# console anyway.
cmd_allow() {
  need_stack
  local key=AllowListId other=ConsoleListId cidr list now held count size
  if [ "${1:-}" = --console ]; then
    key=ConsoleListId other=AllowListId
    shift
  fi
  list="$(list_id "$key")"
  [ -n "$list" ] || die "this lab has no console list: set LAB_CONSOLE_LIST_SIZE and run 'lab.sh infra'"
  cidr="${1:-}"
  if [ -z "$cidr" ]; then
    cidr="$(curl -fsS --max-time 5 https://checkip.amazonaws.com)" || die "could not tell your address; give it: lab.sh allow <ip>"
  fi
  cidr="$(normalize_cidr "$cidr")" || exit 1
  [ $# -gt 0 ] && shift
  now="$(allow_entries "$list")"
  if has_entry "$now" "$cidr"; then
    say "$cidr is already on the $(list_label "$key"): $(awk -F'\t' -v c="$cidr" '$1 == c { print $2 }' <<<"$now")"
    return 0
  fi
  held="$(entries_of "$other")"
  if [ "$key" = ConsoleListId ] && has_entry "$held" "$cidr"; then
    say "$cidr is on the allowlist, which reaches the console already"
    return 0
  fi
  count="$(grep -c . <<<"$now" || true)"
  size="$(list_size "$list")"
  [ "$count" -lt "$size" ] || die "the $(list_label "$key") is full ($count of $size): 'lab.sh deny' one, or resize the lists (LAB_ALLOWLIST_SIZE, LAB_CONSOLE_LIST_SIZE) with 'lab.sh infra'"
  modify_allowlist "$list" add "$cidr" "$(entry_note "$*")"
  say "allowed $cidr on the $(list_label "$key") -- in effect within seconds"
  if has_entry "$held" "$cidr"; then
    modify_allowlist "$(list_id "$other")" remove "$cidr"
    say "  (and took it off the $(list_label "$other"))"
  fi
}

cmd_deny() {
  [ -n "${1:-}" ] || die "which address? lab.sh allowed lists them"
  need_stack
  local cidr key list found=""
  cidr="$(normalize_cidr "$1")" || exit 1
  for key in AllowListId ConsoleListId; do
    list="$(list_id "$key")"
    [ -n "$list" ] && has_entry "$(allow_entries "$list")" "$cidr" || continue
    modify_allowlist "$list" remove "$cidr"
    say "removed $cidr from the $(list_label "$key")"
    found=1
  done
  [ -n "$found" ] || say "$cidr is on no list"
}

cmd_allowed() {
  need_stack
  show_list AllowListId "the vendors, the console (and on https), the lab API and SFTP" \
    "nobody -- 'lab.sh allow' adds your address; 'lab.sh console' works without one"
  if [ -n "$(list_id ConsoleListId)" ]; then
    say ""
    show_list ConsoleListId "the console (and on https) and the vendors' pages (8080-8094) only" \
      "nobody -- 'lab.sh allow --console <ip> <note>' adds someone"
  fi
}

# show_list <AllowListId|ConsoleListId> <what it opens> <what to say when empty>
show_list() {
  local list entries
  list="$(list_id "$1")"
  entries="$(allow_entries "$list")"
  say "$(list_label "$1") $list ($(grep -c . <<<"$entries" || true) of $(list_size "$list")): $2"
  if [ -n "$entries" ]; then
    printf '%s\n' "$entries" | column -t -s $'\t' | sed 's/^/  /'
  else
    say "  $3"
  fi
}

# check_https <name>: whether https://<name>/health answers (the console, or
# a vendor at its own name), from here. A first deploy waits for Let's Encrypt, which takes seconds; an
# address on neither list cannot tell, and says so.
check_https() {
  local i code
  for i in $(seq 1 20); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "https://$1/health" 2>/dev/null || true)"
    [ "$code" = 200 ] && { say "https:    https://$1 answers, with a certificate browsers trust"; return 0; }
    sleep 3
  done
  if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://$1:8090/health" 2>/dev/null || true)" != 200 ]; then
    say "https:    not checked -- this address is on neither list ('lab.sh allow')"
  else
    say "https:    https://$1 does not answer yet (curl says $code): 'lab.sh logs front' shows how its certificate is going" >&2
  fi
}

cmd_outputs() {
  aws cloudformation describe-stacks --stack-name "$STACK" \
    --query 'Stacks[0].Outputs[].[OutputKey,OutputValue]' --output text | column -t
}

cmd_deploy() {
  account
  need_stack
  local id host ip bucket vol sans rev key url https=""
  id="$(output InstanceId)" host="$(output PublicHost)" ip="$(output PublicIp)"
  # A lab with a name serves its console on https too: the pod's front gets
  # a Let's Encrypt certificate for the name (the stack opens 80 for that).
  [[ "$host" =~ [a-z] ]] && https="$host"
  bucket="$(output ReleaseBucket)" vol="$(output DataVolumeId)"
  # The Elastic IP, and any names LAB_SANS adds (DNS:<name>,IP:<address>).
  sans="IP:$ip${LAB_SANS:+,$LAB_SANS}"

  # cloud (no lab clock) unless lab.env asks for local: a lab driven by a
  # runner that follows its clock (docs/deploy-cloud.md, A runner on your
  # machine).
  say "building the ${LAB_PROFILE:-cloud} bundle for $host"
  make --no-print-directory cloud-bundle LAB_PROFILE="${LAB_PROFILE:-cloud}" \
    LAB_PUBLIC_HOST="$host" LAB_SANS="$sans" \
    LAB_WEBHOOK_ALLOW="${LAB_WEBHOOK_ALLOW:-}" LAB_SET="${LAB_SET:-}" LAB_SEEDER_IMAGE="${LAB_SEEDER_IMAGE:-}" \
    LAB_HTTPS_HOST="$https"
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
  if [ -n "$https" ]; then
    check_https "$https"
    check_https "b4b.$https"
    say "console:  https://$https   (and http://$host:8090; from the allowlists -- 'lab.sh allow' adds you; otherwise 'lab.sh console')"
    say "vendors:  https://<service>.$https -- b4b, banking-circle (mTLS), verification, ...; SFTP stays $host:2222"
    say "lab API:  https://lab-api.$https/v1 (and http://$host:8097/v1) with its token ('lab.sh token'): FINTECH_SIM_LAB_URL, FINTECH_SIM_LAB_TOKEN"
  else
    say "console:  http://$host:8090   (from the allowlist -- 'lab.sh allow' adds you; otherwise 'lab.sh console')"
    say "lab API:  http://$host:8097/v1 with its token ('lab.sh token'): FINTECH_SIM_LAB_URL, FINTECH_SIM_LAB_TOKEN"
  fi
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
  # It holds keys: saved where you ran lab.sh from, never into the checkout
  # by accident.
  name="$here/fintechlab${1:+-$1}.env"
  # The file names $host however it is fetched: the console knows its own
  # address (CONSOLE_CONNECT_HOST).
  if curl -fsS --max-time 8 -o "$name" "http://$host:8090$path" 2>/dev/null; then
    say "saved $name (addresses: $host)"
    return 0
  fi
  # Not reachable from here (LAB_ALLOWED_CIDRS): ask the host for it over
  # SSM. Compressed, because SSM returns at most 24,000 characters of
  # output; a cut-off answer fails gunzip rather than writing half a key.
  say "the console at $host:8090 is not reachable from here (not on the allowlist?); fetching it over SSM"
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

# tunnel <port>...: a platform on this machine, called back by the lab. Each
# local port becomes host.containers.internal:<port+10000> inside the lab --
# a host the lab's webhook allowlists already name -- through SSH inside an
# SSM session: no port opened, no third party. A throwaway key per
# connection (EC2 Instance Connect, valid a minute), and a new connection
# whenever one drops, until Ctrl-C.
cmd_tunnel() {
  [ $# -gt 0 ] || die "which ports? e.g. lab.sh tunnel 3000 3114 3109"
  need_stack
  command -v session-manager-plugin >/dev/null || die "needs the AWS Session Manager plugin"
  local id dir p fwd=() map=()
  id="$(output InstanceId)"
  for p in "$@"; do
    [[ "$p" =~ ^[0-9]+$ ]] && [ "$p" -ge 1 ] && [ "$p" -le 55535 ] || die "$p is not a port"
    fwd+=(-R "0.0.0.0:$((p + 10000)):localhost:$p")
    map+=("localhost:$p <- host.containers.internal:$((p + 10000))")
  done
  wait_ssm "$id"
  # The pod reaches its host on the pod network's gateway, not on the
  # host's loopback, so a forward has to listen beyond it (GatewayPorts);
  # the security group keeps those ports inside the host. A session that
  # dies quietly frees its ports within a minute and a half (ClientAlive).
  run_remote "$id" "fintechlab tunnel: sshd" "set -e
f=/etc/ssh/sshd_config.d/fintechlab-tunnel.conf
if [ ! -f \$f ]; then
  printf 'GatewayPorts clientspecified\nClientAliveInterval 30\nClientAliveCountMax 3\n' > \$f
  systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
fi" >/dev/null
  dir="$(mktemp -d)"
  trap 'rm -rf "$dir"' EXIT
  ssh-keygen -q -t ed25519 -N '' -C fintechlab-tunnel -f "$dir/key"
  printf '%s\n' "${map[@]}" | sed 's/^/  /'
  while :; do
    aws ec2-instance-connect send-ssh-public-key --instance-id "$id" --instance-os-user ubuntu \
      --ssh-public-key "file://$dir/key.pub" >/dev/null
    # The transport is the SSM session, authenticated by AWS; the host key
    # is taken on first use, per run.
    ssh -F /dev/null -N -i "$dir/key" -o IdentitiesOnly=yes -o ExitOnForwardFailure=yes \
      -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
      -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$dir/known_hosts" \
      -o ProxyCommand="aws ssm start-session --target %h --document-name AWS-StartSSHSession --parameters portNumber=%p" \
      "${fwd[@]}" "ubuntu@$id" || true
    [ "${LAB_TUNNEL_ONCE:-}" = 1 ] && break
    say "tunnel closed at $(date -u +%H:%M:%SZ); again in 5s (Ctrl-C to stop)"
    sleep 5
  done
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
  say "stopping $id. Keys and state files survive -- Banking Circle's balances, subscriptions"
  say "and tokens among them; what the other vendors hold only in memory does not."
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
  allow) cmd_allow "$@" ;;
  deny) cmd_deny "$@" ;;
  allowed) cmd_allowed ;;
  e2e) cmd_e2e ;;
  console) cmd_console ;;
  tunnel) cmd_tunnel "$@" ;;
  shell) cmd_shell ;;
  logs) cmd_logs "$@" ;;
  stop) cmd_stop ;;
  start) cmd_start ;;
  destroy) cmd_destroy ;;
  -h | --help | help | "") usage 0 ;;
  *) say "unknown command: $cmd" >&2; usage 1 ;;
esac
