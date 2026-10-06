#!/usr/bin/env bash
# calnode-fly.sh — every Fly.io operation for a Calnode instance, from the command line,
# driven by deploy/fly/.env (or exported variables). Nothing here needs a browser.
#
#   calnode-fly.sh init              create app + volume, set secrets, deploy, allocate IPs
#   calnode-fly.sh deploy            build the current checkout and deploy it
#   calnode-fly.sh secrets           push every secret from .env (restarts the machine)
#   calnode-fly.sh status            app, machine, version and health
#   calnode-fly.sh logs [--tail]     recent logs (or follow)
#   calnode-fly.sh domain add HOST   request a certificate + print the DNS records
#   calnode-fly.sh domain check HOST certificate status
#   calnode-fly.sh domain set HOST   point BASE_URL at https://HOST (secrets + restart)
#   calnode-fly.sh backup enable     push LITESTREAM_* secrets (turns continuous backup on)
#   calnode-fly.sh backup verify     restore the replica to /tmp inside the machine and check it
#   calnode-fly.sh backup download [FILE]   copy the live database to your machine (consistent snapshot)
#   calnode-fly.sh restore FILE      replace the database with FILE (stops the app meanwhile)
#   calnode-fly.sh reset --yes       delete the database: next boot shows first-run setup
#   calnode-fly.sh scale [--memory 2gb] [--min N]
#   calnode-fly.sh settings apply    push SETTINGS_* through the admin API (needs CALNODE_API_KEY)
#   calnode-fly.sh settings show     print the current workspace settings
#   calnode-fly.sh teardown --yes    destroy the app and its volume (irreversible)
#   calnode-fly.sh env check         validate .env and show what would change
#
# Requires: flyctl (https://fly.io/docs/flyctl/install/), curl, openssl, git, python3.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
ENV_FILE="${CALNODE_ENV_FILE:-$HERE/.env}"

# ── env loading: file first, exported variables win ────────────────────────────────
if [[ -f "$ENV_FILE" ]]; then
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"; line="${line%"${line##*[![:space:]]}"}"
    [[ -z "$line" || "$line" != *=* ]] && continue
    key="${line%%=*}"; val="${line#*=}"
    key="${key//[[:space:]]/}"
    val="${val#"${val%%[![:space:]]*}"}"
    if [[ "$val" =~ ^\"(.*)\"$ || "$val" =~ ^\'(.*)\'$ ]]; then val="${BASH_REMATCH[1]}"; fi
    [[ -z "${!key:-}" ]] && export "$key=$val"
  done < "$ENV_FILE"
fi

: "${FLY_APP:?set FLY_APP in $ENV_FILE}"
: "${FLY_REGION:=ams}"; : "${FLY_ORG:=personal}"; : "${FLY_VOLUME_SIZE_GB:=1}"
: "${FLY_VM_MEMORY:=1gb}"; : "${FLY_MIN_MACHINES:=1}"
export FLY_API_TOKEN="${FLY_API_TOKEN:-}" FLY_APP FLY_REGION FLY_ORG FLY_VOLUME_SIZE_GB FLY_VM_MEMORY FLY_MIN_MACHINES

log()  { printf '\033[1;34m▸\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
confirm_flag() { [[ "${1:-}" == "--yes" ]] || die "this is irreversible; re-run with --yes"; }
need fly; need curl; need python3

# ── .env write-back (for generated keys and domain changes) ───────────────────────
env_set() { # env_set KEY VALUE — update or append in the env file, keep it 0600
  local key="$1" val="$2"
  [[ -f "$ENV_FILE" ]] || touch "$ENV_FILE"
  if grep -qE "^[[:space:]]*${key}=" "$ENV_FILE"; then
    python3 - "$ENV_FILE" "$key" "$val" <<'PY'
import re,sys
p,k,v=sys.argv[1:4]; s=open(p).read()
s=re.sub(r'(?m)^(\s*'+re.escape(k)+r'=)[^\n#]*', lambda m: m.group(1)+v+' ', s, count=1)
open(p,'w').write(s)
PY
  else
    printf '%s=%s\n' "$key" "$val" >> "$ENV_FILE"
  fi
  chmod 600 "$ENV_FILE"
  export "$key=$val"
}

render_config() {
  need_tmpl="$HERE/fly.toml.tmpl"
  AUTO_STOP="stop"; [[ "$FLY_MIN_MACHINES" -ge 1 ]] && AUTO_STOP="off"
  export AUTO_STOP
  CONFIG="$(mktemp -t calnode-fly.XXXXXX.toml)"
  python3 - "$need_tmpl" "$CONFIG" <<'PY'
import os,re,sys
src,dst=sys.argv[1:3]; s=open(src).read()
s=re.sub(r'\$\{(\w+)\}', lambda m: os.environ.get(m.group(1),''), s)
open(dst,'w').write(s)
PY
  echo "$CONFIG"
}

machine_id() { fly machines list -a "$FLY_APP" --json 2>/dev/null | python3 -c 'import sys,json;m=json.load(sys.stdin);print(m[0]["id"] if m else "")'; }

# Secrets: every Calnode variable that is set. Empty values are skipped (unset with
# `fly secrets unset` if you need to remove one).
secret_names=(BASE_URL PUBLIC_BASE_URL CALNODE_ENCRYPTION_KEY CALNODE_RECOVERY_SECRET LOG_LEVEL
  TRUSTED_PROXY_CIDRS FRAME_ANCESTORS RESEND_API_KEY EMAIL_FROM_ADDRESS EMAIL_FROM_NAME
  EMAIL_SMTP_HOST EMAIL_SMTP_PORT EMAIL_SMTP_USER EMAIL_SMTP_PASS EMAIL_SMTP_STARTTLS
  GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET
  LITESTREAM_REPLICA_URL LITESTREAM_ENDPOINT LITESTREAM_REGION LITESTREAM_ACCESS_KEY_ID LITESTREAM_SECRET_ACCESS_KEY)
collect_secrets() { # prints KEY=VALUE args for fly secrets set
  local args=() k
  for k in "${secret_names[@]}"; do [[ -n "${!k:-}" ]] && args+=("$k=${!k}"); done
  args+=("DATABASE_URL=sqlite:///data/calnode.db")
  printf '%s\n' "${args[@]}"
}
push_secrets() { # push_secrets [--stage]
  local extra=("$@") list
  mapfile -t list < <(collect_secrets)
  log "Setting ${#list[@]} secrets on $FLY_APP"
  fly secrets set -a "$FLY_APP" "${extra[@]}" "${list[@]}" >/dev/null
  ok "secrets set"
}
ensure_keys() {
  if [[ -z "${CALNODE_ENCRYPTION_KEY:-}" ]]; then
    env_set CALNODE_ENCRYPTION_KEY "$(openssl rand -hex 32)"; warn "generated CALNODE_ENCRYPTION_KEY → $ENV_FILE (back it up: losing it makes encrypted settings unrecoverable)"
  fi
  if [[ -z "${CALNODE_RECOVERY_SECRET:-}" ]]; then
    env_set CALNODE_RECOVERY_SECRET "$(openssl rand -hex 32)"; warn "generated CALNODE_RECOVERY_SECRET → $ENV_FILE (store it separately from the encryption key)"
  fi
}
check_env() {
  [[ "${BASE_URL:-}" =~ ^https?:// ]] || die "BASE_URL must include the scheme, e.g. https://bookings.example.com"
  [[ "$BASE_URL" == https://* ]] || warn "BASE_URL is not https: the app will run in non-production mode"
  if [[ -n "${LITESTREAM_REPLICA_URL:-}" ]]; then
    [[ "$LITESTREAM_REPLICA_URL" == s3://* || "$LITESTREAM_REPLICA_URL" == gcs://* ]] || die "LITESTREAM_REPLICA_URL must start with s3:// or gcs://"
    if [[ "$LITESTREAM_REPLICA_URL" == s3://* ]]; then
      [[ -n "${LITESTREAM_ACCESS_KEY_ID:-}" && -n "${LITESTREAM_SECRET_ACCESS_KEY:-}" ]] || die "s3:// replica needs LITESTREAM_ACCESS_KEY_ID and LITESTREAM_SECRET_ACCESS_KEY"
      [[ "${LITESTREAM_ENDPOINT:-}" != */"${LITESTREAM_REPLICA_URL#s3://}"* ]] || die "LITESTREAM_ENDPOINT must not contain the bucket name"
    fi
  else
    warn "LITESTREAM_REPLICA_URL is empty: continuous backups are OFF (a single volume with no replica is the biggest data-loss risk)"
  fi
}

deploy() {
  need git
  local cfg; cfg="$(render_config)"
  local commit branch
  commit="$(git -C "$REPO" rev-parse --short HEAD)"; branch="$(git -C "$REPO" rev-parse --abbrev-ref HEAD)"
  [[ -z "$(git -C "$REPO" status --porcelain)" ]] || warn "working tree has uncommitted changes; deploying them anyway"
  log "Deploying $branch@$commit to $FLY_APP ($FLY_REGION)"
  ( cd "$REPO" && fly deploy -a "$FLY_APP" -c "$cfg" --remote-only --depot=false --ha=false \
      --build-arg "COMMIT=$commit" --build-arg "VERSION=$branch" )
  rm -f "$cfg"
  verify
}
verify() {
  local host; host="$(fly status -a "$FLY_APP" --json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("Hostname",""))')"
  [[ -n "$host" ]] || host="$FLY_APP.fly.dev"
  local v; v="$(curl -sS --max-time 20 "https://$host/version" || true)"
  [[ -n "$v" ]] && ok "https://$host/version → $v" || warn "could not reach https://$host/version yet"
  local code; code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 "https://$host/healthz" || true)"
  [[ "$code" == 200 ]] && ok "healthz 200" || warn "healthz returned '$code'"
}

cmd="${1:-help}"; shift || true
case "$cmd" in
  env)
    case "${1:-check}" in
      check) check_env; log "app=$FLY_APP org=$FLY_ORG region=$FLY_REGION memory=$FLY_VM_MEMORY min=$FLY_MIN_MACHINES"
             log "secrets that would be pushed:"; collect_secrets | sed -E 's/=.*/=…/' | sed 's/^/    /'
             log "rendered config:"; sed 's/^/    /' "$(render_config)";;
      *) die "env check";;
    esac;;
  init)
    check_env; ensure_keys
    if fly apps list --json 2>/dev/null | python3 -c 'import sys,json;sys.exit(0 if any(a["Name"]==sys.argv[1] for a in json.load(sys.stdin)) else 1)' "$FLY_APP"; then
      ok "app $FLY_APP exists"
    else
      log "Creating app $FLY_APP in org $FLY_ORG"; fly apps create "$FLY_APP" --org "$FLY_ORG" >/dev/null; ok "app created"
    fi
    if fly volumes list -a "$FLY_APP" --json 2>/dev/null | python3 -c 'import sys,json;sys.exit(0 if any(v["name"]=="data" for v in json.load(sys.stdin)) else 1)'; then
      ok "volume 'data' exists"
    else
      log "Creating ${FLY_VOLUME_SIZE_GB}GB volume in $FLY_REGION"; fly volumes create data -a "$FLY_APP" --region "$FLY_REGION" --size "$FLY_VOLUME_SIZE_GB" --yes >/dev/null; ok "volume created"
    fi
    push_secrets --stage
    deploy
    log "Next: open $BASE_URL to create the owner account, then 'calnode-fly.sh settings apply' once you have an API key.";;
  deploy)   check_env; deploy;;
  secrets)  check_env; push_secrets; verify;;
  status)   fly status -a "$FLY_APP"; verify;;
  logs)     if [[ "${1:-}" == "--tail" ]]; then fly logs -a "$FLY_APP"; else fly logs -a "$FLY_APP" --no-tail | tail -n 100; fi;;
  domain)
    sub="${1:-}"; host="${2:-}"; [[ -n "$host" ]] || die "domain add|check|set HOST"
    case "$sub" in
      add)   fly certs add "$host" -a "$FLY_APP"
             log "DNS: CNAME $host → $FLY_APP.fly.dev, DNS-only (grey cloud) on Cloudflare until the certificate is issued."
             log "Then: calnode-fly.sh domain check $host  and  calnode-fly.sh domain set $host";;
      check) fly certs check "$host" -a "$FLY_APP";;
      set)   env_set BASE_URL "https://$host"; push_secrets; verify;;
      *) die "domain add|check|set HOST";;
    esac;;
  backup)
    case "${1:-}" in
      enable)   [[ -n "${LITESTREAM_REPLICA_URL:-}" ]] || die "set LITESTREAM_REPLICA_URL (+ endpoint/region/keys) in $ENV_FILE first"
                check_env; push_secrets; log "watch the first snapshot: calnode-fly.sh logs"; ;;
      verify)   [[ -n "${LITESTREAM_REPLICA_URL:-}" ]] || die "backups are not enabled"
                log "Non-destructive restore drill inside the machine (writes only to /tmp)"
                fly ssh console -a "$FLY_APP" -C "sh -c 'rm -f /tmp/check.db && litestream restore -config /etc/litestream.yml -o /tmp/check.db /data/calnode.db && ls -l /tmp/check.db && head -c 15 /tmp/check.db && echo && rm -f /tmp/check.db'" \
                  && ok "replica restores to a valid SQLite file" || die "restore drill failed; check LITESTREAM_* and the logs";;
      download) out="${2:-calnode-$(date +%Y%m%d-%H%M%S).db}"
                if [[ -n "${LITESTREAM_REPLICA_URL:-}" ]]; then
                  log "Restoring a consistent snapshot from the replica inside the machine"
                  fly ssh console -a "$FLY_APP" -C "sh -c 'rm -f /tmp/snap.db && litestream restore -config /etc/litestream.yml -o /tmp/snap.db /data/calnode.db'" >/dev/null
                  fly ssh sftp get /tmp/snap.db "$out" -a "$FLY_APP" >/dev/null
                else
                  warn "no replica configured: copying the raw file plus its WAL (consistent only while the app is quiet)"
                  fly ssh sftp get /data/calnode.db "$out" -a "$FLY_APP" >/dev/null
                  fly ssh sftp get /data/calnode.db-wal "$out-wal" -a "$FLY_APP" >/dev/null 2>&1 || true
                fi
                ok "saved $out ($(du -h "$out" | cut -f1))";;
      *) die "backup enable|verify|download [FILE]";;
    esac;;
  restore)
    file="${1:-}"; [[ -f "$file" ]] || die "restore FILE.db"
    head -c 15 "$file" | grep -q "SQLite format 3" || die "$file is not a SQLite database"
    confirm_flag "${2:-}"
    id="$(machine_id)"; [[ -n "$id" ]] || die "no machine found"
    log "Stopping machine $id"; fly machine stop "$id" -a "$FLY_APP" >/dev/null
    log "Uploading $file"; fly ssh sftp put "$file" /data/calnode.db -a "$FLY_APP" >/dev/null || true
    fly ssh console -a "$FLY_APP" -C "sh -c 'rm -f /data/calnode.db-wal /data/calnode.db-shm'" >/dev/null || true
    log "Starting machine"; fly machine start "$id" -a "$FLY_APP" >/dev/null; sleep 8; verify;;
  reset)
    confirm_flag "${1:-}"
    log "Deleting the database on $FLY_APP (the next boot shows first-run setup; secrets are kept)"
    fly ssh console -a "$FLY_APP" -C "sh -c 'rm -f /data/calnode.db /data/calnode.db-wal /data/calnode.db-shm'"
    id="$(machine_id)"; fly machine restart "$id" -a "$FLY_APP" >/dev/null; sleep 8; verify;;
  scale)
    while [[ $# -gt 0 ]]; do case "$1" in
      --memory) env_set FLY_VM_MEMORY "$2"; shift 2;;
      --min)    env_set FLY_MIN_MACHINES "$2"; shift 2;;
      *) die "scale [--memory 2gb] [--min N]";; esac; done
    fly scale memory "${FLY_VM_MEMORY%gb}"gb -a "$FLY_APP" >/dev/null 2>&1 || fly scale memory "$(( ${FLY_VM_MEMORY%gb} * 1024 ))" -a "$FLY_APP"
    deploy;;
  settings)
    [[ -n "${CALNODE_API_KEY:-}" ]] || die "set CALNODE_API_KEY (Admin → API Keys) in $ENV_FILE"
    api() { curl -sS --fail-with-body -H "X-API-Key: $CALNODE_API_KEY" -H 'Content-Type: application/json' "$@"; }
    case "${1:-show}" in
      show)  for p in branding participants signin; do echo "── $p"; api "$BASE_URL/v1/settings/$p" | python3 -m json.tool | grep -E '"(business_name|show_host_names|default_attendee_emails|default_calendar_message|allowed_signin_domains)"' -A3 | head -20; done;;
      apply)
        if [[ -n "${SETTINGS_SHOW_HOST_NAMES:-}${SETTINGS_BUSINESS_NAME:-}" ]]; then
          cur="$(api "$BASE_URL/v1/settings/branding")"
          body="$(python3 - "$cur" <<'PY'
import json,os,sys
cur=json.loads(sys.argv[1]); out={k:cur.get(k) for k in ("business_name","logo_height","logo_opacity","banner_opacity","privacy_url","terms_url","fallback_locale") if k in cur}
if os.environ.get("SETTINGS_SHOW_HOST_NAMES"): out["show_host_names"]=os.environ["SETTINGS_SHOW_HOST_NAMES"].lower()=="true"
if os.environ.get("SETTINGS_BUSINESS_NAME"): out["business_name"]=os.environ["SETTINGS_BUSINESS_NAME"]
print(json.dumps(out))
PY
)"
          api -X PATCH "$BASE_URL/v1/settings/branding" -d "$body" >/dev/null && ok "branding applied"
        fi
        if [[ -n "${SETTINGS_DEFAULT_PARTICIPANTS:-}${SETTINGS_DEFAULT_CALENDAR_MESSAGE:-}" ]]; then
          body="$(python3 -c 'import json,os;o={}
p=os.environ.get("SETTINGS_DEFAULT_PARTICIPANTS");m=os.environ.get("SETTINGS_DEFAULT_CALENDAR_MESSAGE")
if p is not None and p!="": o["default_attendee_emails"]=[x.strip() for x in p.split(",") if x.strip()]
if m: o["default_calendar_message"]=m
print(json.dumps(o))')"
          api -X PATCH "$BASE_URL/v1/settings/participants" -d "$body" >/dev/null && ok "meeting defaults applied"
        fi
        if [[ -n "${SETTINGS_SIGNIN_DOMAINS:-}" ]]; then
          body="$(python3 -c 'import json,os;print(json.dumps({"allowed_signin_domains":[x.strip() for x in os.environ["SETTINGS_SIGNIN_DOMAINS"].split(",") if x.strip()]}))')"
          api -X PATCH "$BASE_URL/v1/settings/signin" -d "$body" >/dev/null && ok "sign-in domains applied"
        fi;;
      *) die "settings show|apply";;
    esac;;
  teardown)
    confirm_flag "${1:-}"
    warn "Destroying app $FLY_APP and its volume. Take 'calnode-fly.sh backup download' first if you need the data."
    fly apps destroy "$FLY_APP" --yes; ok "destroyed";;
  help|--help|-h) sed -n '2,24p' "$0";;
  *) die "unknown command '$cmd' (see --help)";;
esac
