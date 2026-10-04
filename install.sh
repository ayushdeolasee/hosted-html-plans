#!/usr/bin/env bash
# hosted-html-plans installer — https://github.com/ayushdeolasee/hosted-html-plans
#
# GENERATED FILE — do not edit. Edit scripts/push-plan or
# scripts/build-installer, then run `make installer` to regenerate.

set -euo pipefail

REPO_SLUG="ayushdeolasee/hosted-html-plans"
INSTALL_URL="https://raw.githubusercontent.com/ayushdeolasee/hosted-html-plans/main/install.sh"
SKILLS_SOURCE="${PLANS_SKILLS_SOURCE:-$REPO_SLUG}"

MODE=""
BIN_DIR="${PLANS_BIN_DIR:-}"
SERVER_URL="${PLANS_URL:-}"
LAN_SERVER_URL="${PLANS_LAN_URL:-}"
TAILSCALE_SERVER_URL="${PLANS_TAILSCALE_URL:-}"
CONFIG="${PLANS_CONFIG:-$HOME/.config/plans/config.json}"
DO_CLI=1
DO_SKILLS=1
SKILLS_AGENTS=()
UNINSTALL=0
BINARY=""
VERSION=""
USER_UNIT=0
PRINT_ONLY=0
SERVER_TMP=""

usage() {
  cat <<'USAGE'
hosted-html-plans installer

  U=https://raw.githubusercontent.com/ayushdeolasee/hosted-html-plans/main/install.sh
  curl -fsSL $U | bash -s -- --client
  curl -fsSL $U | bash -s -- --server

Modes (no flag: interactive picker in a terminal, --client otherwise)
  --client            Install the `push-plan` command and use `npx skills` to
                      add the html-plans agent skill wherever you work with
                      hosted HTML plans.
  --server            Install the `plans` binary and register it as a
                      background service (launchd on macOS, systemd on Linux)
                      on the box that hosts them.
  Pass both flags to install both sides on the same machine.

Client options
  --url URL           Set the preferred server URL without prompting.
  --lan-url URL       Set the LAN/cloud server URL without prompting.
  --tailscale-url URL Set the Tailscale server URL without prompting.
  --bin-dir DIR       Where to install push-plan (default: ~/.local/bin, or
                      /usr/local/bin if that's writable and ~/.local/bin isn't
                      on PATH).
  --agent NAME         Install skills for this agent without prompting.
                      Repeat for multiple agents (e.g. claude-code, codex).
  --all-agents         Install skills for every agent supported by skills CLI.
  --cli-only          Install only the shared push-plan command.
  --skills-only       Skip the command.

Server options
  --binary PATH       Install this local binary instead of fetching one.
  --version TAG       Release tag to fetch (default: the latest release).
  --bin-dir DIR       Where to install plans (default: ~/.local/bin for macOS
                      and Linux --user; /usr/local/bin for Linux system units
                      when writable or sudo is available, else ~/.local/bin).
  --user              Linux: install a user-level systemd unit (no root)
                      instead of a system unit.
  --print             Dry run — show what would be written and run, change
                      nothing.

Common
  --uninstall         Reverse the selected mode.
  -h, --help          This text.

Environment: PLANS_BIN_DIR, PLANS_URL, PLANS_LAN_URL, PLANS_TAILSCALE_URL,
and PLANS_CONFIG are honored as defaults.
USAGE
}

add_mode() { # <client|server>
  case "$MODE:$1" in
    :client|client:client) MODE="client" ;;
    :server|server:server) MODE="server" ;;
    client:server|server:client|both:*) MODE="both" ;;
  esac
}

while [ $# -gt 0 ]; do
  case "$1" in
    --client)      add_mode "client"; shift ;;
    --server)      add_mode "server"; shift ;;
    --url|--server-url)
                    SERVER_URL="${2:?$1 needs a URL}"; shift 2 ;;
    --lan-url)     LAN_SERVER_URL="${2:?--lan-url needs a URL}"; shift 2 ;;
    --tailscale-url)
                    TAILSCALE_SERVER_URL="${2:?--tailscale-url needs a URL}"; shift 2 ;;
    --bin-dir)     BIN_DIR="${2:?--bin-dir needs a directory}"; shift 2 ;;
    --agent)       SKILLS_AGENTS+=("${2:?--agent needs a name}"); shift 2 ;;
    --all-agents)  SKILLS_AGENTS=("*"); shift ;;
    --cli-only)    DO_SKILLS=0; shift ;;
    --skills-only) DO_CLI=0; shift ;;
    --binary)      BINARY="${2:?--binary needs a path}"; shift 2 ;;
    --version)     VERSION="${2:?--version needs a tag}"; shift 2 ;;
    --user)        USER_UNIT=1; shift ;;
    --print)       PRINT_ONLY=1; shift ;;
    --uninstall)   UNINSTALL=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *) echo "install: unknown option: $1" >&2; echo "try: --help" >&2; exit 1 ;;
  esac
done

die() { echo "install: $*" >&2; exit 1; }

on_path() {
  case ":${PATH}:" in *":$1:"*) return 0 ;; *) return 1 ;; esac
}

need_curl() {
  command -v curl >/dev/null 2>&1 || die "curl is required"
}

cleanup_server_tmp() {
  if [ -n "${SERVER_TMP:-}" ] && [ -d "$SERVER_TMP" ]; then
    rm -rf -- "$SERVER_TMP"
  fi
  SERVER_TMP=""
}

ensure_command_on_path() { # <bin-dir>
  local bin_dir="$1" rc="" path_line
  path_line="export PATH=\"$bin_dir:\$PATH\""

  if on_path "$bin_dir"; then
    return
  fi

  # This process can use the command immediately. Persist the same addition for
  # future shells; a child installer cannot change its parent shell's PATH.
  PATH="$bin_dir:$PATH"
  export PATH
  case "${SHELL:-}" in
    */zsh)  rc="$HOME/.zshrc" ;;
    */bash) rc="$HOME/.bashrc" ;;
  esac

  if [ -n "$rc" ]; then
    mkdir -p "$(dirname "$rc")"
    if [ ! -f "$rc" ] || ! grep -Fqx "$path_line" "$rc"; then
      printf '\n# Added by hosted-html-plans installer\n%s\n' "$path_line" >> "$rc"
      echo "  PATH       added $bin_dir to $rc"
    fi
  else
    echo "  PATH       add this to your shell profile:"
    echo "             $path_line"
  fi
}

draw_mode_picker() { # <cursor> <server-selected> <client-selected> <message>
  local cursor="$1" server_selected="$2" client_selected="$3" message="$4"
  local reset="" bold="" dim="" cyan="" green="" red=""
  local server_cursor=" " client_cursor=" " server_box="[ ]" client_box="[ ]"

  if [ "${TERM:-dumb}" != "dumb" ]; then
    reset="$(printf '\033[0m')"
    bold="$(printf '\033[1m')"
    dim="$(printf '\033[2m')"
    cyan="$(printf '\033[36m')"
    green="$(printf '\033[32m')"
    red="$(printf '\033[31m')"
    printf '\033[2J\033[H' >&3
  fi

  [ "$cursor" -eq 0 ] && server_cursor="${cyan}>${reset}"
  [ "$cursor" -eq 1 ] && client_cursor="${cyan}>${reset}"
  [ "$server_selected" -eq 1 ] && server_box="${green}[x]${reset}"
  [ "$client_selected" -eq 1 ] && client_box="${green}[x]${reset}"

  printf '%s\n' "${bold}hosted-html-plans${reset}" >&3
  printf '%s\n\n' "${dim}Choose what to install on this machine${reset}" >&3
  printf '  %s %s %sServer%s\n' "$server_cursor" "$server_box" "$bold" "$reset" >&3
  printf '      %sHosts and serves your plans as a background service.%s\n\n' "$dim" "$reset" >&3
  printf '  %s %s %sClient%s\n' "$client_cursor" "$client_box" "$bold" "$reset" >&3
  printf '      %sAdds push-plan, then lets you choose agents via npx skills.%s\n\n' "$dim" "$reset" >&3
  printf '  %s↑/↓%s move   %sspace%s select   %senter%s install   %sq%s quit\n' \
    "$cyan" "$reset" "$cyan" "$reset" "$cyan" "$reset" "$cyan" "$reset" >&3
  if [ -n "$message" ]; then
    printf '\n  %s%s%s\n' "$red" "$message" "$reset" >&3
  fi
}

choose_modes() {
  local cursor=0 server_selected=0 client_selected=0 key="" rest="" message=""

  # Read from the controlling terminal rather than stdin. This keeps the UI
  # interactive when the installer itself arrived through `curl | bash`.
  if ! { exec 3<>/dev/tty; } 2>/dev/null || [ ! -t 3 ]; then
    MODE="client"
    return
  fi
  trap 'printf "\033[0m\n" >&3; exit 130' INT TERM
  while :; do
    draw_mode_picker "$cursor" "$server_selected" "$client_selected" "$message"
    key=""
    IFS= read -rsn1 key <&3 || {
      printf '\033[0m\n' >&3
      exit 1
    }
    message=""
    case "$key" in
      " ")
        if [ "$cursor" -eq 0 ]; then
          server_selected=$((1 - server_selected))
        else
          client_selected=$((1 - client_selected))
        fi
        ;;
      "")
        if [ "$server_selected" -eq 0 ] && [ "$client_selected" -eq 0 ]; then
          message="Select at least one option."
          continue
        fi
        if [ "$server_selected" -eq 1 ] && [ "$client_selected" -eq 1 ]; then
          MODE="both"
        elif [ "$server_selected" -eq 1 ]; then
          MODE="server"
        else
          MODE="client"
        fi
        break
        ;;
      q|Q)
        printf '\033[0m\nInstallation cancelled.\n' >&3
        exit 0
        ;;
      k|K) cursor=0 ;;
      j|J) cursor=1 ;;
      "$(printf '\033')")
        rest=""
        IFS= read -rsn2 -t 1 rest <&3 || true
        case "$rest" in
          "[A") cursor=0 ;;
          "[B") cursor=1 ;;
        esac
        ;;
    esac
  done
  trap - INT TERM
  if [ "${TERM:-dumb}" != "dumb" ]; then
    printf '\033[2J\033[H' >&3
  fi
  printf 'Installing: %s\n\n' "$MODE" >&3
  exec 3>&-
}

# Explicit mode flags keep scripted installs non-interactive. With no mode,
# show the picker when a controlling terminal is available; headless/CI runs
# retain the historical client default.
[ -n "$MODE" ] || choose_modes

# ---- embedded payloads -----------------------------------------------------
#
# Written as functions so the same heredoc can serve both the CLI install and
# the skill's bundled copy without duplicating the script in this file.

write_push_plan() { # <target>
  cat > "$1" <<'PUSH_PLAN_SCRIPT_EOF__DO_NOT_EDIT'
#!/usr/bin/env bash
# push-plan — push an HTML deliverable to a hosted-html-plans server.
#
# Usage:
#   push-plan <file.html> [title] [-m note] [--slug S] [--base-version N]
#   push-plan draft [slug]              print the path to write the plan HTML to
#   push-plan pull <slug|url> [--version N] fetch a plan into the draft cache
#   push-plan gc                        drop drafts whose plan is gone server-side
#
# The draft cache exists so agents never litter a git worktree with plan HTML.
# It lives at:
#
#   ${PLANS_DRAFT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/plans/drafts}
#
# and holds exactly one file per plan: <slug>.html. Because the name is derived
# from the plan's slug, every revision overwrites the same file — revisions can
# never pile up, and "revert to an older version" is just `pull --version N`
# overwriting that same path. A brand-new plan (no slug yet) gets a temporary
# `_new-*.html` name, which is renamed to <slug>.html the moment the server
# assigns a real slug on the first push.
#
# URL resolution, in order:
#   1. the server in a full URL passed to `pull` (explicit; no fallback)
#   2. $PLANS_URL env var (explicit override; no fallback)
#   3. "tailscale_url" in ~/.config/plans/config.json
#   4. "lan_url" in ~/.config/plans/config.json
#   5. legacy "push_url" in ~/.config/plans/config.json
#   6. http://localhost:8080 when no URL is configured
#
# `pull` accepts either a bare slug or a full plan URL. A URL pins the server
# it names, so a configured default (which may be a *different* box) can never
# win and silently hand back a different plan that happens to share the slug.
#
# Repo + branch are auto-tagged from the git context this script runs in
# (git remote get-url origin / git branch --show-current); both are omitted
# gracefully when not run inside a repo, or when there's no remote.
#
# Portable: bash or zsh, macOS or Linux. Uses jq if available, else python3,
# else a grep/sed fallback for the tiny bits of JSON we need to read.
#
# See agent-loop.html §5 (write primitives) and plan.html §10 (agent
# integration) for the design this implements.

set -u

# ---- usage / arg parsing ----------------------------------------------

usage() {
  cat >&2 <<'EOF'
usage:
  push-plan <file.html> [title] [-m "note"] [--slug S] [--base-version N]
  push-plan draft [slug]               print the draft path to write HTML to
  push-plan pull <slug|url> [--version N]
                                       fetch a plan into the draft cache; a
                                       full URL pins the server it names
  push-plan gc                         remove drafts with no plan on the server

env:
  PLANS_URL          override all configured server URLs (no fallback)
  PLANS_DRAFT_DIR     override the draft cache directory (default
                      ${XDG_CACHE_HOME:-$HOME/.cache}/plans/drafts)
  PUSH_PLAN_AGENT     value for ?agent= (default: "cli")
EOF
}

# A leading `draft` / `pull` / `gc` is a subcommand — unless a file by that
# name actually exists, in which case the old positional form still wins and
# `push-plan draft` keeps meaning "push the file ./draft".
SUBCMD=""
if [ $# -gt 0 ]; then
  case "$1" in
    draft|pull|gc)
      if [ ! -e "$1" ]; then
        SUBCMD="$1"
        shift
      fi
      ;;
  esac
fi

FILE=""
TITLE=""
NOTE=""
SLUG=""
BASE_VERSION=""
VERSION=""
# Server pinned by a full URL argument. Set by split_plan_url; consulted by
# resolve_urls ahead of everything else, and never falls back.
PLANS_URL_PIN=""
# Version carried in a URL's query string. Applied only when --version was not
# given explicitly, so the flag always wins.
URL_VERSION=""

# ---- accept a plan URL wherever a slug is accepted ----------------------
#
# `pull` takes either a bare slug or a full URL. Splitting the URL here, in the
# CLI, is deliberate: the alternative is every caller (a human, or an agent
# reading SKILL.md) hand-parsing it into a slug plus a PLANS_URL= prefix, which
# is a step that fails quietly on trailing slashes, ports, and query strings.
#
# Assigns the globals PLAN_SLUG, PLANS_URL_PIN (server) and URL_VERSION (from
# ?version=). It cannot echo its result: a command substitution would run it in
# a subshell, where those assignments — and a failing `exit` — would be lost.
# A bare `/p/<slug>` path with no host pins nothing and resolves normally.
PLAN_SLUG=""
split_plan_url() {
  local arg="$1" rest="" path="" query="" slug="" part=""

  case "$arg" in
    *://*)
      rest="${arg#*://}"
      case "$rest" in
        */*) path="/${rest#*/}" ;;
        *)   path="" ;;
      esac
      # Everything before the first path separator is scheme + authority.
      PLANS_URL_PIN="${arg%"$path"}"
      PLANS_URL_PIN="${PLANS_URL_PIN%/}"
      ;;
    /*)
      path="$arg"
      ;;
    *)
      # A bare slug — nothing to split.
      PLAN_SLUG="$arg"
      return 0
      ;;
  esac

  # Drop a fragment before parsing the query, so ?version=3#section stays v3.
  path="${path%%#*}"
  case "$path" in
    *\?*)
      query="${path#*\?}"
      path="${path%%\?*}"
      ;;
  esac
  path="${path%/}"
  slug="${path##*/}"

  if [ -z "$slug" ]; then
    echo "push-plan pull: could not find a plan slug in: $arg" >&2
    exit 1
  fi

  # Pull ?version=N out of the query; ignore every other parameter.
  local old_ifs="$IFS"
  IFS='&'
  for part in $query; do
    case "$part" in
      version=*) URL_VERSION="${part#version=}" ;;
    esac
  done
  IFS="$old_ifs"

  PLAN_SLUG="$slug"
}

case "$SUBCMD" in
  draft)
    while [ $# -gt 0 ]; do
      case "$1" in
        -h|--help) usage; exit 0 ;;
        -*) echo "push-plan draft: unknown option: $1" >&2; exit 1 ;;
        *)
          if [ -z "$SLUG" ]; then
            SLUG="$1"
          else
            echo "push-plan draft: unexpected argument: $1" >&2
            exit 1
          fi
          shift
          ;;
      esac
    done
    ;;
  pull)
    while [ $# -gt 0 ]; do
      case "$1" in
        -v|--version)
          if [ $# -lt 2 ]; then
            echo "push-plan pull: $1 requires an argument" >&2
            exit 1
          fi
          VERSION="$2"
          shift 2
          ;;
        -h|--help) usage; exit 0 ;;
        -*) echo "push-plan pull: unknown option: $1" >&2; exit 1 ;;
        *)
          if [ -z "$SLUG" ]; then
            SLUG="$1"
          else
            echo "push-plan pull: unexpected argument: $1" >&2
            exit 1
          fi
          shift
          ;;
      esac
    done
    if [ -z "$SLUG" ]; then
      echo "push-plan pull: missing <slug|url>" >&2
      usage
      exit 1
    fi
    split_plan_url "$SLUG"
    SLUG="$PLAN_SLUG"
    # An explicit --version overrides one carried in the URL.
    [ -z "$VERSION" ] && VERSION="$URL_VERSION"
    if [ -n "$VERSION" ]; then
      case "$VERSION" in
        *[!0-9]*|0)
          echo "push-plan pull: --version must be a positive integer" >&2
          exit 1
          ;;
      esac
    fi
    ;;
  gc)
    if [ $# -gt 0 ]; then
      echo "push-plan gc: unexpected argument: $1" >&2
      exit 1
    fi
    ;;
  *)
    while [ $# -gt 0 ]; do
      case "$1" in
        -m|--note)
          if [ $# -lt 2 ]; then
            echo "push-plan: $1 requires an argument" >&2
            exit 1
          fi
          NOTE="$2"
          shift 2
          ;;
        -s|--slug)
          if [ $# -lt 2 ]; then
            echo "push-plan: $1 requires an argument" >&2
            exit 1
          fi
          SLUG="$2"
          shift 2
          ;;
        --base-version)
          if [ $# -lt 2 ]; then
            echo "push-plan: $1 requires an argument" >&2
            exit 1
          fi
          BASE_VERSION="$2"
          shift 2
          ;;
        -h|--help)
          usage
          exit 0
          ;;
        -*)
          echo "push-plan: unknown option: $1" >&2
          usage
          exit 1
          ;;
        *)
          if [ -z "$FILE" ]; then
            FILE="$1"
          elif [ -z "$TITLE" ]; then
            TITLE="$1"
          else
            echo "push-plan: unexpected argument: $1" >&2
            usage
            exit 1
          fi
          shift
          ;;
      esac
    done

    if [ -z "$FILE" ]; then
      echo "push-plan: missing <file.html>" >&2
      usage
      exit 1
    fi

    if [ ! -f "$FILE" ]; then
      echo "push-plan: file not found: $FILE" >&2
      exit 1
    fi
    ;;
esac

# `draft` is the one subcommand that never talks to the server.
if [ "$SUBCMD" != "draft" ] && ! command -v curl >/dev/null 2>&1; then
  echo "push-plan: curl is required but not found on PATH" >&2
  exit 1
fi

# ---- tiny JSON helpers (jq > python3 > grep/sed) -----------------------

# json_get_field <json-file-or-string-flag> <input> <key>
# Reads a top-level string field from a JSON blob given on stdin.
json_get_field() {
  local key="$1"
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg k "$key" '.[$k] // empty' 2>/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
import json, sys
key = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
v = d.get(key)
if isinstance(v, str):
    print(v)
' "$key" 2>/dev/null
  else
    # crude fallback: only handles simple "key": "value" pairs.
    grep -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" 2>/dev/null \
      | sed -E "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"([^\"]*)\"/\1/" \
      | head -n1
  fi
}

# json_get_nested_field <input-on-stdin> <outer-key> <inner-key>
json_get_nested_field() {
  local outer="$1" inner="$2"
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg o "$outer" --arg i "$inner" '.[$o][$i] // empty' 2>/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
import json, sys
outer, inner = sys.argv[1], sys.argv[2]
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
v = d.get(outer, {})
if isinstance(v, dict):
    val = v.get(inner)
    if isinstance(val, str):
        print(val)
' "$outer" "$inner" 2>/dev/null
  else
    grep -o "\"$inner\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" 2>/dev/null \
      | sed -E "s/.*\"$inner\"[[:space:]]*:[[:space:]]*\"([^\"]*)\"/\1/" \
      | head -n1
  fi
}

# json_slugs — reads GET /api/plans (a JSON array of plan objects) on stdin
# and prints one slug per line. Same jq > python3 > grep/sed ladder; the
# fallback is safe here because slugs are always simple "slug": "..." pairs.
json_slugs() {
  if command -v jq >/dev/null 2>&1; then
    jq -r '.[].slug // empty' 2>/dev/null
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if isinstance(d, list):
    for p in d:
        if isinstance(p, dict) and isinstance(p.get("slug"), str):
            print(p["slug"])
' 2>/dev/null
  else
    grep -o "\"slug\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" 2>/dev/null \
      | sed -E "s/.*\"slug\"[[:space:]]*:[[:space:]]*\"([^\"]*)\"/\1/"
  fi
}

# ---- resolve server URL -------------------------------------------------

resolve_urls() {
  # A server named in a URL argument is the most explicit signal there is: it
  # outranks even $PLANS_URL, and emits a single candidate so there is no
  # fallback to some other box holding a same-named plan.
  if [ -n "$PLANS_URL_PIN" ]; then
    printf '%s\n' "${PLANS_URL_PIN%/}"
    return
  fi

  if [ -n "${PLANS_URL:-}" ]; then
    printf '%s\n' "${PLANS_URL%/}"
    return
  fi

  local cfg="${PLANS_CONFIG:-$HOME/.config/plans/config.json}"
  if [ -f "$cfg" ]; then
    local tailscale_url lan_url push_url seen="" url found=0
    tailscale_url="$(json_get_field "tailscale_url" < "$cfg")"
    lan_url="$(json_get_field "lan_url" < "$cfg")"
    push_url="$(json_get_field "push_url" < "$cfg")"
    for url in "$tailscale_url" "$lan_url" "$push_url"; do
      url="${url%/}"
      [ -n "$url" ] || continue
      case " $seen " in
        *" $url "*) continue ;;
      esac
      printf '%s\n' "$url"
      seen="$seen $url"
      found=1
    done
    [ "$found" -eq 1 ] && return
  fi

  printf '%s\n' "http://localhost:8080"
}

PLANS_URL_CANDIDATES="$(resolve_urls)"
PLANS_URL_RESOLVED=""
HTTP_CODE=""
CURL_EXIT=0

# curl_with_fallback <path> <body-file> <error-file> <attempt-log> [curl args]
#
# Tries the ordered server candidates until curl can complete a request.
# HTTP responses (including 4xx/5xx) are authoritative and never fall through
# to another server; only DNS/TLS/connectivity failures trigger fallback.
curl_with_fallback() {
  local path="$1" body_file="$2" error_file="$3" attempt_log="$4"
  shift 4
  local old_ifs="$IFS" base_url detail

  : > "$attempt_log"
  IFS='
'
  for base_url in $PLANS_URL_CANDIDATES; do
    IFS="$old_ifs"
    : > "$body_file"
    : > "$error_file"
    HTTP_CODE="$(curl --connect-timeout 5 -o "$body_file" -w '%{http_code}' \
      "$@" "${base_url}${path}" 2>"$error_file")"
    CURL_EXIT=$?
    if [ "$CURL_EXIT" -eq 0 ]; then
      PLANS_URL_RESOLVED="$base_url"
      if [ -s "$attempt_log" ]; then
        echo "push-plan: using fallback server $PLANS_URL_RESOLVED" >&2
      fi
      IFS="$old_ifs"
      return 0
    fi
    detail="$(tr '\n' ' ' < "$error_file" | sed 's/[[:space:]]*$//')"
    [ -n "$detail" ] || detail="curl exited $CURL_EXIT"
    printf '  %s -> %s\n' "$base_url" "$detail" >> "$attempt_log"
    IFS='
'
  done
  IFS="$old_ifs"
  return 1
}

report_unreachable() { # <attempt-log>
  echo "push-plan: could not reach any configured server:" >&2
  cat "$1" >&2
}

# ---- git context: repo + branch -----------------------------------------

normalize_repo() {
  local url="$1"
  # strip trailing .git
  url="${url%.git}"
  case "$url" in
    git@*)
      # git@host:owner/name
      local host_path host path
      host_path="${url#git@}"
      host="${host_path%%:*}"
      path="${host_path#*:}"
      printf '%s/%s\n' "$host" "$path"
      ;;
    ssh://*)
      local rest
      rest="${url#ssh://}"
      rest="${rest#git@}"
      # drop an optional :port right after the host
      rest="$(printf '%s' "$rest" | sed -E 's#^([^/:]+):[0-9]+/#\1/#')"
      printf '%s\n' "$rest"
      ;;
    http://*|https://*)
      local rest
      rest="${url#*://}"
      rest="${rest#*@}" # drop any user@ credentials
      printf '%s\n' "$rest"
      ;;
    *)
      printf '%s\n' "$url"
      ;;
  esac
}

REPO=""
BRANCH=""
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  origin_url="$(git remote get-url origin 2>/dev/null || true)"
  if [ -n "$origin_url" ]; then
    REPO="$(normalize_repo "$origin_url")"
  fi
  BRANCH="$(git branch --show-current 2>/dev/null || true)"
fi

# ---- draft cache ---------------------------------------------------------
#
# One file per plan, named after the plan's slug. Everything below refuses to
# touch anything outside DRAFT_DIR — deletions in particular are guarded by
# in_draft_dir(), which resolves symlinks before comparing.

DRAFT_DIR="${PLANS_DRAFT_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/plans/drafts}"

ensure_draft_dir() {
  if [ ! -d "$DRAFT_DIR" ]; then
    mkdir -p "$DRAFT_DIR" || {
      echo "push-plan: could not create draft dir: $DRAFT_DIR" >&2
      exit 1
    }
    chmod 0700 "$DRAFT_DIR" 2>/dev/null || true
  fi
}

# slugify — the same normalization the server applies (lowercase, runs of
# non-alphanumerics collapsed to '-', trimmed; empty becomes "plan"). Keeping
# it in sync means the path we print is the path the plan will keep.
slugify() {
  local s
  s="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//')"
  [ -z "$s" ] && s="plan"
  printf '%s' "$s"
}

# new_draft_name — the temporary name for a plan the server hasn't slugged
# yet. Derived from the git context (or the working directory) so that two
# calls in the same place return the same path rather than piling up files.
# The `_` prefix can't collide with a real slug, and marks the file as "not
# yet a plan" for gc.
new_draft_name() {
  local ctx=""
  if [ -n "$REPO" ] || [ -n "$BRANCH" ]; then
    ctx="${REPO}-${BRANCH}"
  else
    ctx="$(basename "$PWD")"
  fi
  printf '_new-%s' "$(slugify "$ctx")"
}

# dir_of — the absolute, symlink-resolved directory containing a path.
dir_of() {
  local d
  d="$(dirname "$1")"
  (cd "$d" 2>/dev/null && pwd -P)
}

# in_draft_dir <path> — true only when path sits directly inside DRAFT_DIR.
# Every deletion and every rename is gated on this.
in_draft_dir() {
  local real_draft file_dir
  real_draft="$(cd "$DRAFT_DIR" 2>/dev/null && pwd -P)" || return 1
  file_dir="$(dir_of "$1")" || return 1
  [ -n "$real_draft" ] && [ "$file_dir" = "$real_draft" ]
}

# ---- percent-encoding ----------------------------------------------------

# Percent-encode a query-string value. NOTE: we build the full request URL
# ourselves (rather than using curl's `-G`/`--data-urlencode`) because `-G`
# redirects ALL `-d`/`--data-binary` payloads into the URL as GET params —
# which would send the plan's HTML body as a query string instead of the
# POST body. Keeping our own encoder lets --data-binary stay the body.
urlencode() {
  local string="$1" strlen out c byte i
  # Shells count/slice characters in a UTF-8 locale, but URL encoding must
  # operate on the underlying bytes. Without this, printf receives a whole
  # Unicode code point (for example U+2014) and can emit a sign-extended
  # value such as %FFFFFFFFFFFFFFE2 instead of %E2%80%94.
  local LC_ALL=C
  strlen=${#string}
  out=""
  for (( i = 0; i < strlen; i++ )); do
    c="${string:i:1}"
    case "$c" in
      [a-zA-Z0-9.~_-]) out+="$c" ;;
      *)
        byte=$(printf '%d' "'$c")
        out+=$(printf '%%%02X' "$((byte & 255))")
        ;;
    esac
  done
  printf '%s' "$out"
}

# ---- subcommand: draft ---------------------------------------------------
#
# Print the path the agent should write its HTML to. Never write plan HTML
# into a git worktree; write here, push from here.

if [ "$SUBCMD" = "draft" ]; then
  ensure_draft_dir
  if [ -n "$SLUG" ]; then
    name="$(slugify "$SLUG")"
  else
    name="$(new_draft_name)"
  fi
  printf '%s/%s.html\n' "$DRAFT_DIR" "$name"
  exit 0
fi

# ---- subcommand: pull ----------------------------------------------------
#
# Fetch a plan (latest, or ?version=N) into <draftdir>/<slug>.html, clobbering
# whatever was there. That overwrite IS the revert flow: there is no second,
# newer file to clean up, because there is only ever one file per plan.
#
# ?format=raw serves the stored bytes verbatim. A plain /p/{slug} view injects
# either the live-reload client (latest) or the "viewing vN of M" banner
# (historical), and since whatever we pull here gets pushed back on the next
# revision, an injection would be round-tripped into the plan itself.

if [ "$SUBCMD" = "pull" ]; then
  ensure_draft_dir
  slug="$(slugify "$SLUG")"

  fetch_path="/p/${slug}?format=raw"
  [ -n "$VERSION" ] && fetch_path="${fetch_path}&version=$(urlencode "$VERSION")"

  tmp_body="$(mktemp)"
  trap 'rm -f "$tmp_body" "$tmp_body.err" "$tmp_body.attempts"' EXIT

  if ! curl_with_fallback "$fetch_path" "$tmp_body" "$tmp_body.err" \
      "$tmp_body.attempts" -sSL; then
    report_unreachable "$tmp_body.attempts"
    exit 1
  fi

  fetch_url="${PLANS_URL_RESOLVED}${fetch_path}"
  case "$HTTP_CODE" in
    2??) ;;
    *)
      echo "push-plan: server returned HTTP $HTTP_CODE for $fetch_url" >&2
      cat "$tmp_body" >&2
      exit 1
      ;;
  esac

  target="${DRAFT_DIR}/${slug}.html"
  cp -f "$tmp_body" "$target" || {
    echo "push-plan: could not write $target" >&2
    exit 1
  }

  echo "Pulled: ${slug} (v${VERSION:-latest})"
  printf '%s\n' "$target"
  exit 0
fi

# ---- subcommand: gc ------------------------------------------------------
#
# Drop draft files whose plan no longer exists on the server. Only *.html
# directly inside DRAFT_DIR are ever considered, `_`-prefixed drafts (not yet
# pushed, so not yet on the server) are skipped, and every unlink is gated on
# in_draft_dir().

if [ "$SUBCMD" = "gc" ]; then
  if [ ! -d "$DRAFT_DIR" ]; then
    echo "gc: no draft dir at $DRAFT_DIR — nothing to do"
    exit 0
  fi

  tmp_body="$(mktemp)"
  trap 'rm -f "$tmp_body" "$tmp_body.err" "$tmp_body.attempts"' EXIT

  if ! curl_with_fallback "/api/plans" "$tmp_body" "$tmp_body.err" \
      "$tmp_body.attempts" -sS; then
    report_unreachable "$tmp_body.attempts"
    exit 1
  fi

  case "$HTTP_CODE" in
    2??) ;;
    *)
      echo "push-plan: server returned HTTP $HTTP_CODE listing plans" >&2
      cat "$tmp_body" >&2
      exit 1
      ;;
  esac

  live_slugs="$(json_slugs < "$tmp_body")"

  removed=0
  for f in "$DRAFT_DIR"/*.html; do
    [ -f "$f" ] || continue          # no match: the glob stayed literal
    base="${f##*/}"
    slug="${base%.html}"
    case "$slug" in
      _*) continue ;;                # not pushed yet, so not on the server
    esac
    if printf '%s\n' "$live_slugs" | grep -qx -- "$slug"; then
      continue
    fi
    if in_draft_dir "$f"; then
      rm -f -- "$f"
      echo "removed  $f"
      removed=$((removed + 1))
    fi
  done

  if [ "$removed" -eq 0 ]; then
    echo "gc: nothing to remove ($DRAFT_DIR)"
  else
    echo "gc: removed $removed draft(s)"
  fi
  exit 0
fi

# ---- push -----------------------------------------------------------------

AGENT="${PUSH_PLAN_AGENT:-cli}"

# A draft file is named after its plan, so a push from <draftdir>/<slug>.html
# is by definition a revision of <slug> — tell the server so explicitly
# instead of letting it re-derive a slug from the title (which would fork a
# new plan the moment the title is reworded).
FILE_BASE="${FILE##*/}"
if [ -z "$SLUG" ] && in_draft_dir "$FILE"; then
  case "$FILE_BASE" in
    _*) ;; # brand-new draft: the server assigns the slug
    *.html) SLUG="${FILE_BASE%.html}" ;;
  esac
fi
[ -n "$SLUG" ] && SLUG="$(slugify "$SLUG")"

query="agent=$(urlencode "$AGENT")"
[ -n "$TITLE" ] && query="${query}&title=$(urlencode "$TITLE")"
[ -n "$REPO" ] && query="${query}&repo=$(urlencode "$REPO")"
[ -n "$BRANCH" ] && query="${query}&branch=$(urlencode "$BRANCH")"
[ -n "$NOTE" ] && query="${query}&note=$(urlencode "$NOTE")"

# --base-version means "revise this exact plan, and 409 if someone beat me to
# it" — that's PUT /api/plans/{slug}. Without it we POST, which creates the
# plan or appends a version to it when the slug already exists.
if [ -n "$BASE_VERSION" ]; then
  if [ -z "$SLUG" ]; then
    echo "push-plan: --base-version needs a slug (--slug S, or push from <draftdir>/<slug>.html)" >&2
    exit 1
  fi
  METHOD="PUT"
  push_path="/api/plans/${SLUG}?${query}&base_version=$(urlencode "$BASE_VERSION")"
else
  METHOD="POST"
  [ -n "$SLUG" ] && query="${query}&slug=$(urlencode "$SLUG")"
  push_path="/api/plans?${query}"
fi

tmp_body="$(mktemp)"
trap 'rm -f "$tmp_body" "$tmp_body.err" "$tmp_body.attempts"' EXIT

if ! curl_with_fallback "$push_path" "$tmp_body" "$tmp_body.err" \
    "$tmp_body.attempts" -sS -X "$METHOD" \
    -H "Content-Type: text/html" --data-binary "@${FILE}"; then
  report_unreachable "$tmp_body.attempts"
  exit 1
fi
rm -f "$tmp_body.err"

resp="$(cat "$tmp_body")"
push_url="${PLANS_URL_RESOLVED}${push_path}"

case "$HTTP_CODE" in
  2??)
    ;;
  *)
    echo "push-plan: server returned HTTP $HTTP_CODE from $PLANS_URL_RESOLVED" >&2
    echo "$resp" >&2
    exit 1
    ;;
esac

slug="$(printf '%s' "$resp" | json_get_field "slug")"
version="$(printf '%s' "$resp" | json_get_field "version")"
lan_url="$(printf '%s' "$resp" | json_get_nested_field "urls" "lan")"
tailnet_url="$(printf '%s' "$resp" | json_get_nested_field "urls" "tailnet")"

echo "Pushed: ${slug:-<unknown>} (v${version:-?})"
[ -n "$lan_url" ] && echo "  lan:     $lan_url"
[ -n "$tailnet_url" ] && echo "  tailnet: $tailnet_url"
if [ -z "$lan_url" ] && [ -z "$tailnet_url" ]; then
  echo "  (server response did not include URLs — raw response below)"
  echo "$resp"
fi

# ---- settle the draft on its canonical path -------------------------------
#
# The plan now has a server-assigned slug, so its one local file belongs at
# <draftdir>/<slug>.html. A first push from a temporary `_new-*` draft is
# renamed onto that path here; every later revision is already there and this
# is a no-op. Files outside the draft dir (the old `push-plan ./plan.html`
# form) are left strictly alone.

if [ -n "$slug" ] && in_draft_dir "$FILE"; then
  canonical="${DRAFT_DIR}/${slug}.html"
  current="$(dir_of "$FILE")/${FILE_BASE}"
  if [ "$current" != "$canonical" ] && in_draft_dir "$canonical"; then
    if mv -f -- "$current" "$canonical"; then
      echo "  draft:   $canonical"
    fi
  fi
fi

exit 0
PUSH_PLAN_SCRIPT_EOF__DO_NOT_EDIT
  chmod +x "$1"
}


# ---- client mode -----------------------------------------------------------

client_bin_dir() {
  # An explicit choice wins; otherwise prefer ~/.local/bin (no sudo), falling
  # back to /usr/local/bin only when it's writable and ~/.local/bin isn't
  # already on PATH.
  if [ -n "$BIN_DIR" ]; then
    printf '%s' "$BIN_DIR"
    return
  fi
  if on_path "$HOME/.local/bin" || [ ! -w /usr/local/bin ]; then
    printf '%s' "$HOME/.local/bin"
  else
    printf '%s' "/usr/local/bin"
  fi
}

client_uninstall() {
  local target="$(client_bin_dir)/push-plan"
  if [ "$DO_SKILLS" -eq 1 ]; then
    run_skills_cli remove
  fi
  if [ "$DO_CLI" -eq 1 ]; then
    if [ -e "$target" ]; then
      rm -f "$target"
      echo "removed    $target"
    else
      echo "nothing to remove at $target"
    fi
  fi
}

client_install_cli() {
  local bin_dir target
  bin_dir="$(client_bin_dir)"
  target="$bin_dir/push-plan"

  need_curl
  mkdir -p "$bin_dir"

  # Remove any existing entry first. Critical: if $target is a symlink (e.g. an
  # older dev install that linked into a checkout), a plain `cat >` would follow
  # it and overwrite the link's target instead of replacing the link.
  rm -f "$target"
  write_push_plan "$target"
  echo "installed  $target"

  if ! on_path "$bin_dir"; then
    local rc
    case "${SHELL:-}" in
      */zsh)  rc="~/.zshrc" ;;
      */bash) rc="~/.bashrc" ;;
      *)      rc="your shell rc" ;;
    esac
    echo
    echo "  !  $bin_dir is not on your PATH. Add this to $rc:"
    echo "       export PATH=\"$bin_dir:\$PATH\""
    echo "     then restart your shell (or run: export PATH=\"$bin_dir:\$PATH\")"
    echo
  fi
}

run_skills_cli() { # <add|remove>
  command -v npx >/dev/null 2>&1 || die "npx is required to install agent skills.
    Install Node.js/npm, or use --cli-only to install just push-plan."

  local action="$1" agent
  local args=()
  if [ "$action" = "add" ]; then
    args=(add "$SKILLS_SOURCE" --global --skill html-plans)
  else
    args=(remove html-plans --global)
  fi

  for agent in "${SKILLS_AGENTS[@]+"${SKILLS_AGENTS[@]}"}"; do
    args+=(--agent "$agent")
  done

  if [ "${#SKILLS_AGENTS[@]}" -gt 0 ]; then
    # Explicit agents make this safe for scripts and CI.
    args+=(--yes)
    npx -y skills "${args[@]}"
    return
  fi

  echo
  echo "Choose which agents should receive the hosted-html-plans skill:"
  echo
  # The installer is commonly piped into Bash, so stdin is the script rather
  # than the keyboard. Give the skills CLI the controlling terminal directly.
  if ! { exec 3<>/dev/tty; } 2>/dev/null || [ ! -t 3 ]; then
    die "agent selection needs an interactive terminal.
    Re-run with --agent <name> (repeatable), for example:
      --agent codex
      --agent claude-code --agent codex"
  fi
  npx -y skills "${args[@]}" <&3
  exec 3>&-
}

normalize_lan_url() { # <IP, host, or URL>
  local value="${1%/}"
  case "$value" in
    http://*|https://*) printf '%s' "$value" ;;
    *:*)               printf 'http://%s' "$value" ;;
    *)                 printf 'http://%s:8080' "$value" ;;
  esac
}

normalize_tailscale_url() { # <FQDN or URL>
  local value="${1%/}"
  case "$value" in
    http://*|https://*) printf '%s' "$value" ;;
    *)                 printf 'https://%s' "$value" ;;
  esac
}

tailscale_cli() {
  if command -v tailscale >/dev/null 2>&1; then
    command -v tailscale
  elif [ -x /Applications/Tailscale.app/Contents/MacOS/Tailscale ]; then
    printf '%s' /Applications/Tailscale.app/Contents/MacOS/Tailscale
  fi
}

tailscale_dns_state() { # <tailscale-cli>
  local output
  output="$("$1" dns status 2>/dev/null || true)"
  if printf '%s\n' "$output" | grep -Eq '^Tailscale DNS:[[:space:]]*enabled'; then
    printf '%s' enabled
  elif printf '%s\n' "$output" | grep -Eq '^Tailscale DNS:[[:space:]]*disabled'; then
    printf '%s' disabled
  else
    printf '%s' unknown
  fi
}

print_tailscale_dns_instructions() { # <tailscale-cli-or-empty>
  local cli="$1"
  echo
  echo "To make Tailscale hostnames resolve:"
  if [ "$(uname -s)" = "Darwin" ]; then
    echo "  Open Tailscale > Settings and enable:"
    echo "    [x] Use Tailscale DNS settings"
  else
    echo "  Open this device's Tailscale DNS settings and enable Tailscale DNS."
  fi
  if [ -n "$cli" ]; then
    echo "  Or run:"
    echo "    $cli set --accept-dns=true"
  fi
}

enable_tailscale_dns() { # <tailscale-cli>
  local cli="$1"
  echo
  echo "Enabling Tailscale DNS:"
  if [ "$(uname -s)" = "Linux" ] && [ "$(id -u)" -ne 0 ]; then
    if ! command -v sudo >/dev/null 2>&1; then
      echo "  sudo is required to change Tailscale DNS on this machine." >&2
      return 1
    fi
    echo "  sudo $cli set --accept-dns=true"
    sudo "$cli" set --accept-dns=true
  else
    echo "  $cli set --accept-dns=true"
    "$cli" set --accept-dns=true
  fi
}

check_tailscale_dns() {
  local cli="" state="" answer=""

  # There is nothing to resolve through MagicDNS unless this client has been
  # configured with a Tailscale address. Also recognize a legacy --url that
  # points directly at a ts.net hostname.
  if [ -z "$TAILSCALE_SERVER_URL" ]; then
    case "$SERVER_URL" in
      *.ts.net|*.ts.net:*) ;;
      *) return ;;
    esac
  fi

  cli="$(tailscale_cli)"
  if [ -z "$cli" ]; then
    echo
    echo "Tailscale DNS check skipped: the Tailscale CLI was not found."
    print_tailscale_dns_instructions ""
    return
  fi

  state="$(tailscale_dns_state "$cli")"
  case "$state" in
    enabled)
      echo
      echo "Tailscale DNS is enabled. MagicDNS hostnames can resolve on this device."
      return
      ;;
    unknown)
      echo
      echo "The installer could not determine whether Tailscale DNS is enabled."
      print_tailscale_dns_instructions "$cli"
      return
      ;;
  esac

  echo
  echo "Tailscale DNS is disabled on this device."
  echo "Without it, MagicDNS addresses such as plans.<tailnet>.ts.net will not resolve."

  if ! { exec 3<>/dev/tty; } 2>/dev/null || [ ! -t 3 ]; then
    print_tailscale_dns_instructions "$cli"
    return
  fi

  printf 'Enable Tailscale DNS now? [Y/n] ' >&3
  IFS= read -r answer <&3 || answer="n"
  exec 3>&-
  case "$answer" in
    ""|y|Y|yes|Yes|YES)
      if enable_tailscale_dns "$cli" &&
          [ "$(tailscale_dns_state "$cli")" = "enabled" ]; then
        echo "Tailscale DNS is now enabled."
      else
        echo "The installer could not enable or verify Tailscale DNS." >&2
        print_tailscale_dns_instructions "$cli"
      fi
      ;;
    *)
      echo "Tailscale DNS was left disabled."
      print_tailscale_dns_instructions "$cli"
      ;;
  esac
}

prompt_client_urls() {
  local input=""
  # Explicit flags/environment keep automated installs non-interactive.
  if [ -n "$SERVER_URL" ] || [ -n "$LAN_SERVER_URL" ] || [ -n "$TAILSCALE_SERVER_URL" ]; then
    return
  fi
  if ! { exec 3<>/dev/tty; } 2>/dev/null || [ ! -t 3 ]; then
    return
  fi

  echo >&3
  echo "Server addresses" >&3
  echo "Enter either or both. Press Enter to leave an address unset." >&3
  echo >&3
  printf 'LAN/cloud IP or URL (e.g. 192.168.1.20 or https://plans.example.com): ' >&3
  IFS= read -r input <&3 || input=""
  if [ -n "$input" ]; then
    LAN_SERVER_URL="$(normalize_lan_url "$input")"
  fi
  printf 'Tailscale hostname or URL (e.g. plans.example.ts.net): ' >&3
  IFS= read -r input <&3 || input=""
  if [ -n "$input" ]; then
    TAILSCALE_SERVER_URL="$(normalize_tailscale_url "$input")"
  fi
  exec 3>&-
}

client_record_urls() {
  LAN_SERVER_URL="${LAN_SERVER_URL%/}"
  TAILSCALE_SERVER_URL="${TAILSCALE_SERVER_URL%/}"
  if [ -z "$SERVER_URL" ]; then
    if [ -n "$TAILSCALE_SERVER_URL" ]; then
      SERVER_URL="$TAILSCALE_SERVER_URL"
    else
      SERVER_URL="$LAN_SERVER_URL"
    fi
  fi
  [ -n "$SERVER_URL" ] || return 0
  SERVER_URL="${SERVER_URL%/}"
  mkdir -p "$(dirname "$CONFIG")"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$CONFIG" "$SERVER_URL" "$LAN_SERVER_URL" "$TAILSCALE_SERVER_URL" <<'PY'
import json, os, sys
path, preferred, lan, tailnet = sys.argv[1:5]
data = {}
if os.path.exists(path):
    try:
        with open(path) as f:
            data = json.load(f)
    except Exception:
        data = {}
if not isinstance(data, dict):
    data = {}
data["push_url"] = preferred
if lan:
    data["lan_url"] = lan
if tailnet:
    data["tailscale_url"] = tailnet
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
os.replace(tmp, path)
PY
    echo "configured $CONFIG  push_url=$SERVER_URL"
    [ -n "$LAN_SERVER_URL" ] && echo "                         lan_url=$LAN_SERVER_URL"
    [ -n "$TAILSCALE_SERVER_URL" ] && echo "                   tailscale_url=$TAILSCALE_SERVER_URL"
  elif [ ! -e "$CONFIG" ]; then
    if [ -n "$LAN_SERVER_URL" ] && [ -n "$TAILSCALE_SERVER_URL" ]; then
      printf '{\n  "push_url": "%s",\n  "lan_url": "%s",\n  "tailscale_url": "%s"\n}\n' \
        "$SERVER_URL" "$LAN_SERVER_URL" "$TAILSCALE_SERVER_URL" > "$CONFIG"
    elif [ -n "$LAN_SERVER_URL" ]; then
      printf '{\n  "push_url": "%s",\n  "lan_url": "%s"\n}\n' \
        "$SERVER_URL" "$LAN_SERVER_URL" > "$CONFIG"
    elif [ -n "$TAILSCALE_SERVER_URL" ]; then
      printf '{\n  "push_url": "%s",\n  "tailscale_url": "%s"\n}\n' \
        "$SERVER_URL" "$TAILSCALE_SERVER_URL" > "$CONFIG"
    else
      printf '{\n  "push_url": "%s"\n}\n' "$SERVER_URL" > "$CONFIG"
    fi
    echo "configured $CONFIG  push_url=$SERVER_URL"
  else
    echo "install: python3 not found and $CONFIG already exists — add" >&2
    echo "         \"push_url\": \"$SERVER_URL\" to it by hand." >&2
  fi
}

run_client() {
  if [ "$UNINSTALL" -eq 1 ]; then
    client_uninstall
    return
  fi
  prompt_client_urls
  check_tailscale_dns
  [ "$DO_CLI" -eq 1 ] && client_install_cli
  [ "$DO_SKILLS" -eq 1 ] && run_skills_cli add
  client_record_urls

  echo
  if [ "$DO_CLI" -eq 1 ]; then
    echo "Done. Try:  push-plan --help"
  else
    echo "Done. Reload your agent to make the new skills available."
  fi
  if [ -z "$SERVER_URL" ] && [ "$DO_CLI" -eq 1 ]; then
    echo
    echo "push-plan needs to know your server. Either re-run this installer with"
    echo "  --url https://plans.<tailnet>.ts.net"
    echo "or export PLANS_URL in your shell. Defaults to http://localhost:8080."
  fi
}

# ---- server mode -----------------------------------------------------------

detect_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux)  os="linux" ;;
    *) die "unsupported OS: $(uname -s) — plans runs on macOS and Linux" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  OS="$os"
  PLATFORM="${os}-${arch}"
}

server_bin_dir() {
  if [ -n "$BIN_DIR" ]; then
    printf '%s' "$BIN_DIR"
    return
  fi
  # Per-user services must be able to stage and replace their own binary
  # without sudo for updates from Settings to work.
  if [ "$OS" = "darwin" ] || [ "$USER_UNIT" -eq 1 ]; then
    printf '%s' "$HOME/.local/bin"
    return
  fi
  # A system-level systemd unit runs as root, so the binary wants to live
  # somewhere root can always read. Prefer /usr/local/bin when we can write
  # there directly or via sudo; otherwise fall back to the home dir.
  if [ -w /usr/local/bin ] || [ "$(id -u)" -eq 0 ]; then
    printf '%s' "/usr/local/bin"
  elif command -v sudo >/dev/null 2>&1; then
    printf '%s' "/usr/local/bin"
  else
    printf '%s' "$HOME/.local/bin"
  fi
}

# sudo_if_needed — echo "sudo" when we need it to write to <dir> (or to
# register a system unit) and it's available. Empty otherwise.
sudo_if_needed() { # <dir>
  [ "$(id -u)" -eq 0 ] && return 0
  if [ -w "$1" ] || { [ ! -e "$1" ] && [ -w "$(dirname "$1")" ]; }; then
    return 0
  fi
  command -v sudo >/dev/null 2>&1 && printf 'sudo'
}

latest_release_tag() {
  local json tag
  json="$(curl -fsSL "https://api.github.com/repos/$REPO_SLUG/releases/latest" 2>/dev/null || true)"
  [ -n "$json" ] || return 1
  if command -v python3 >/dev/null 2>&1; then
    tag="$(printf '%s' "$json" | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("tag_name",""))
except Exception:
    pass' 2>/dev/null || true)"
  else
    tag="$(printf '%s' "$json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  fi
  [ -n "$tag" ] || return 1
  printf '%s' "$tag"
}

# fetch_release — download the release asset for this platform into <dest>.
fetch_release() { # <dest>
  local tag url
  tag="$VERSION"
  if [ -z "$tag" ]; then
    tag="$(latest_release_tag)" || return 1
  fi
  url="https://github.com/$REPO_SLUG/releases/download/$tag/plans-$PLATFORM"
  echo "  fetching   $url" >&2
  curl -fsSL -o "$1" "$url" 2>/dev/null || return 1
  [ -s "$1" ] || return 1
  chmod +x "$1"
  RESOLVED_FROM="release $tag"
}

# build_from_source — last resort when no release asset matches this platform.
# Needs Go on the box; takes ~30s.
build_from_source() { # <dest>
  command -v go >/dev/null 2>&1 || return 1
  local ref src_dir work
  ref="${VERSION:-main}"
  work="$(mktemp -d)"
  echo "  building   from source ($ref) with $(go version | awk '{print $3}')" >&2
  if ! curl -fsSL "https://codeload.github.com/$REPO_SLUG/tar.gz/$ref" \
      | tar -xz -C "$work" 2>/dev/null; then
    rm -rf "$work"
    return 1
  fi
  src_dir="$(find "$work" -maxdepth 1 -mindepth 1 -type d | head -1)"
  [ -n "$src_dir" ] || { rm -rf "$work"; return 1; }
  if ! ( cd "$src_dir" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o "$1" ./cmd/plans ) >&2; then
    rm -rf "$work"
    return 1
  fi
  rm -rf "$work"
  chmod +x "$1"
  RESOLVED_FROM="source ($ref)"
}

resolve_binary() { # <dest>
  if [ -n "$BINARY" ]; then
    [ -f "$BINARY" ] || die "--binary: no such file: $BINARY"
    cp "$BINARY" "$1"
    chmod +x "$1"
    RESOLVED_FROM="$BINARY"
    return 0
  fi
  need_curl
  if fetch_release "$1"; then
    return 0
  fi
  echo "  note       no release asset for $PLATFORM${VERSION:+ at $VERSION} — trying source" >&2
  if build_from_source "$1"; then
    return 0
  fi
  die "could not obtain a plans binary for $PLATFORM.
    Tried: GitHub Releases, then building from source (needs Go).
    Fix one of those, or cross-compile on another machine and pass
    --binary /path/to/plans-$PLATFORM"
}

server_status_output() { # <plans-binary> <service-flags>
  local target="$1" flags="$2"
  # shellcheck disable=SC2086
  "$target" service status $flags 2>/dev/null || true
}

tailnet_value() { # <status-output> <auth-url|fqdn>
  case "$2" in
    auth-url)
      printf '%s\n' "$1" |
        sed -n 's#^Tailnet auth:.*visit \(https://[^ ]*\).*#\1#p' |
        head -1
      ;;
    fqdn)
      printf '%s\n' "$1" |
        sed -n 's/^Tailnet auth:[[:space:]]*authenticated as \([^ ]*\).*/\1/p' |
        head -1
      ;;
  esac
}

wait_for_tailnet_value() { # <plans-binary> <service-flags> <field> <seconds>
  local target="$1" flags="$2" field="$3" limit="$4"
  local elapsed=0 status value
  while [ "$elapsed" -lt "$limit" ]; do
    status="$(server_status_output "$target" "$flags")"
    value="$(tailnet_value "$status" "$field")"
    if [ -n "$value" ]; then
      printf '%s' "$value"
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  return 1
}

print_client_install_command() { # <server-url>
  echo
  echo "Install the client on your laptop with:"
  echo
  echo "  curl -fsSL $INSTALL_URL | bash -s -- --client --tailscale-url $1"
}

guide_tailscale_setup() { # <plans-binary> <service-flags>
  local target="$1" flags="$2" auth_url="" fqdn="" answer=""

  # Returning users may already have authenticated this tsnet node.
  fqdn="$(wait_for_tailnet_value "$target" "$flags" fqdn 3 || true)"
  if [ -n "$fqdn" ]; then
    fqdn="${fqdn%.}"
    SERVER_URL="https://$fqdn"
    TAILSCALE_SERVER_URL="$SERVER_URL"
    echo
    echo "Tailscale is ready:  $SERVER_URL"
    print_client_install_command "$SERVER_URL"
    return
  fi

  echo
  echo "Waiting for the Tailscale authentication URL..."
  auth_url="$(wait_for_tailnet_value "$target" "$flags" auth-url 30 || true)"
  if [ -z "$auth_url" ]; then
    echo "Tailscale has not produced an authentication URL yet."
    echo "You can check again at any time with:  plans service status"
    return
  fi

  echo
  echo "Tailscale needs one-time approval for the 'plans' node:"
  echo
  echo "  $auth_url"
  echo

  if ! { exec 3<>/dev/tty; } 2>/dev/null || [ ! -t 3 ]; then
    echo "Open that URL to approve the node, then run:  plans service status"
    return
  fi

  printf 'Press Enter after approving it, or type s to skip Tailscale setup: ' >&3
  IFS= read -r answer <&3 || answer="s"
  exec 3>&-
  case "$answer" in
    s|S|q|Q|n|N|cancel|Cancel)
      echo
      echo "Tailscale setup skipped. The LAN service remains available."
      echo "Resume later with:  plans service status"
      return
      ;;
  esac

  echo
  echo "Waiting for Tailscale to finish connecting..."
  fqdn="$(wait_for_tailnet_value "$target" "$flags" fqdn 120 || true)"
  if [ -z "$fqdn" ]; then
    echo "Tailscale is still connecting. Check it with:  plans service status"
    return
  fi

  fqdn="${fqdn%.}"
  SERVER_URL="https://$fqdn"
  TAILSCALE_SERVER_URL="$SERVER_URL"
  echo "Tailscale is ready:  $SERVER_URL"
  print_client_install_command "$SERVER_URL"
}

server_uninstall() {
  local bin_dir target sudo_cmd flags
  bin_dir="$(server_bin_dir)"
  target="$bin_dir/plans"
  [ -x "$target" ] || target="$(command -v plans 2>/dev/null || true)"
  [ -n "$target" ] && [ -x "$target" ] || die "no plans binary found to uninstall"

  flags=""
  [ "$USER_UNIT" -eq 1 ] && flags="-user"
  [ "$PRINT_ONLY" -eq 1 ] && flags="$flags -print"

  sudo_cmd=""
  if [ "$OS" = "linux" ] && [ "$USER_UNIT" -eq 0 ]; then
    sudo_cmd="$(sudo_if_needed "/etc/systemd/system")"
  fi
  # shellcheck disable=SC2086
  $sudo_cmd "$target" service uninstall $flags

  if [ "$PRINT_ONLY" -eq 0 ]; then
    sudo_cmd="$(sudo_if_needed "$target")"
    # shellcheck disable=SC2086
    $sudo_cmd rm -f "$target" && echo "removed    $target"
  fi
}

run_server() {
  detect_platform

  if [ "$UNINSTALL" -eq 1 ]; then
    server_uninstall
    return
  fi

  local bin_dir target sudo_cmd flags
  bin_dir="$(server_bin_dir)"
  target="$bin_dir/plans"

  echo "hosted-html-plans — server install"
  echo "  platform   $PLATFORM"

  SERVER_TMP="$(mktemp -d)"
  trap cleanup_server_tmp EXIT
  RESOLVED_FROM=""
  resolve_binary "$SERVER_TMP/plans"
  echo "  source     $RESOLVED_FROM"

  if [ "$PRINT_ONLY" -eq 1 ]; then
    echo "  would install to $target, then run: $target service install"
    echo "  (the ExecStart below names the staging copy, since nothing has been"
    echo "   installed yet — a real run records $target)"
    echo
    "$SERVER_TMP/plans" service install -print
    cleanup_server_tmp
    trap - EXIT
    return
  fi

  sudo_cmd="$(sudo_if_needed "$bin_dir")"
  # shellcheck disable=SC2086
  $sudo_cmd mkdir -p "$bin_dir"
  # shellcheck disable=SC2086
  $sudo_cmd install -m 0755 "$SERVER_TMP/plans" "$target"
  echo "  installed  $target"
  ensure_command_on_path "$bin_dir"
  hash -r 2>/dev/null || true
  echo
  echo "CLI ready"
  echo "  command     plans"
  echo "  location    $target"
  echo "  help        plans --help"

  # Register the service using the installed path, not the temp one — the unit
  # records whatever binary invokes it (os.Executable), so running the temp
  # copy would write an ExecStart that disappears when this script exits.
  flags=""
  [ "$USER_UNIT" -eq 1 ] && flags="-user"

  sudo_cmd=""
  if [ "$OS" = "linux" ] && [ "$USER_UNIT" -eq 0 ]; then
    sudo_cmd="$(sudo_if_needed "/etc/systemd/system")"
    if [ -z "$sudo_cmd" ] && [ "$(id -u)" -ne 0 ]; then
      die "a system-level systemd unit needs root, and sudo isn't available.
    Re-run as root, or use --user for a user-level unit."
    fi
  fi
  # shellcheck disable=SC2086
  $sudo_cmd "$target" service install $flags

  cleanup_server_tmp
  trap - EXIT

  guide_tailscale_setup "$target" "$flags"
}

# ---- dispatch --------------------------------------------------------------

case "$MODE" in
  client) run_client ;;
  server) run_server ;;
  both)
    run_server
    echo
    run_client
    ;;
  *) die "unknown mode: $MODE" ;;
esac
