#!/usr/bin/env bash

set -euo pipefail
umask 077

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
state_dir="${TRAE_PROXY_STATE_DIR:-${repo_dir}/.trae-proxy}"
binary_path="${state_dir}/cli-proxy-api"
go_cache_dir="${TRAE_PROXY_GOCACHE:-${state_dir}/go-build-cache}"
config_path="${state_dir}/config.yaml"
api_key_path="${state_dir}/api-key"
auth_dir="${state_dir}/auths"
log_path="${state_dir}/server.log"
pid_path="${state_dir}/server.pid"

proxy_host="${TRAE_PROXY_HOST:-}"
proxy_port="${TRAE_PROXY_PORT:-}"
proxy_mode="${TRAE_PROXY_MODE:-}"
if [[ -s "${config_path}" ]]; then
  if [[ -z "${proxy_host}" ]]; then
    proxy_host="$(sed -n 's/^host: "\(.*\)"$/\1/p' "${config_path}" | head -n 1)"
  fi
  if [[ -z "${proxy_port}" ]]; then
    proxy_port="$(sed -n 's/^port: \([0-9][0-9]*\)$/\1/p' "${config_path}" | head -n 1)"
  fi
  if [[ -z "${proxy_mode}" ]]; then
    proxy_mode="$(sed -n 's/^  mode: "\(.*\)"$/\1/p' "${config_path}" | head -n 1)"
  fi
fi
proxy_host="${proxy_host:-127.0.0.1}"
proxy_port="${proxy_port:-8317}"
proxy_mode="${proxy_mode:-native}"
go_bin="${TRAE_PROXY_GO_BIN:-}"
trae_home="${TRAE_HOME:-${HOME}/.trae}"
trae_auth_path="${trae_home}/cli/auth.json"
trae_models_path="${trae_home}/cli/models_cache.json"

info() {
  printf '[trae-proxy] %s\n' "$*" >&2
}

warn() {
  printf '[trae-proxy] WARNING: %s\n' "$*" >&2
}

fail() {
  printf '[trae-proxy] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: ./trae-quickstart.sh [command]

Commands:
  start       Configure, build, and start the proxy in the background (default)
  prepare     Configure and build the proxy without starting it
  foreground  Configure, build, and run the proxy in the foreground
  stop        Stop the background proxy
  restart     Stop and start the background proxy
  status      Show the proxy process and health status
  logs        Follow the proxy log
  doctor      Check Go, TRAE CLI, login state, files, and proxy health
  models      List models exposed by the proxy
  api-key     Print the generated local API key
  help        Show this help

Environment overrides:
  TRAE_PROXY_HOST        Bind host (default: saved value or 127.0.0.1)
  TRAE_PROXY_PORT        Bind port (default: saved value or 8317)
  TRAE_PROXY_MODE        native or exec (default: saved value or native)
  TRAE_PROXY_STATE_DIR   Runtime directory (default: .trae-proxy)
  TRAE_PROXY_GOCACHE     Go build cache (default: <state-dir>/go-build-cache)
  TRAE_PROXY_GO_BIN      Explicit Go executable path
  TRAE_HOME              TRAE data directory (default: ~/.trae)
  TRAECLI_BIN            Explicit traecli/traex executable path
  TRAECLI_LOGIN_MODE     default, sso, sso-device, or git-code
EOF
}

validate_settings() {
  case "${proxy_mode}" in
    native | exec) ;;
    *) fail "TRAE_PROXY_MODE must be 'native' or 'exec', got '${proxy_mode}'." ;;
  esac

  case "${proxy_port}" in
    '' | *[!0-9]*) fail "TRAE_PROXY_PORT must be an integer, got '${proxy_port}'." ;;
  esac
  proxy_port=$((10#${proxy_port}))
  if ((proxy_port < 1 || proxy_port > 65535)); then
    fail "TRAE_PROXY_PORT must be between 1 and 65535, got '${proxy_port}'."
  fi
}

is_trae_cli_next() {
  local candidate="$1"
  [[ -x "${candidate}" ]] || return 1
  "${candidate}" exec --help >/dev/null 2>&1 || return 1
  "${candidate}" login --help >/dev/null 2>&1 || return 1
}

find_trae_cli() {
  local candidate=""
  local -a candidates=()

  if [[ -n "${TRAECLI_BIN:-}" ]]; then
    candidates+=("${TRAECLI_BIN}")
  fi
  if command -v traex >/dev/null 2>&1; then
    candidates+=("$(command -v traex)")
  fi
  if command -v traecli >/dev/null 2>&1; then
    candidates+=("$(command -v traecli)")
  fi
  candidates+=("${HOME}/.local/bin/traex" "${HOME}/.local/bin/traecli")

  for candidate in "${candidates[@]}"; do
    if is_trae_cli_next "${candidate}"; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  return 1
}

require_trae_cli() {
  local trae_cli=""
  if trae_cli="$(find_trae_cli)"; then
    printf '%s\n' "${trae_cli}"
    return 0
  fi
  fail "TRAE CLI 2.0 was not found. Install traecli/traex first or set TRAECLI_BIN to its executable path."
}

ensure_trae_login() {
  local trae_cli="$1"
  if "${trae_cli}" login status >/dev/null 2>&1 && [[ -s "${trae_auth_path}" ]]; then
    return 0
  fi
  [[ -t 0 ]] || fail "TRAE login is required. Run '${trae_cli} login' interactively, then retry."

  local -a login_args=(login)
  case "${TRAECLI_LOGIN_MODE:-default}" in
    default) ;;
    sso) login_args+=(--sso) ;;
    sso-device) login_args+=(--sso-device) ;;
    git-code) login_args+=(--git-code) ;;
    *) fail "Unknown TRAECLI_LOGIN_MODE '${TRAECLI_LOGIN_MODE}'." ;;
  esac

  info "TRAE login is required. Complete the interactive authorization once."
  "${trae_cli}" "${login_args[@]}"
  "${trae_cli}" login status >/dev/null 2>&1 || fail "TRAE CLI still reports that it is logged out."
  [[ -s "${trae_auth_path}" ]] || fail "TRAE login succeeded, but '${trae_auth_path}' was not created. This repository requires TRAE CLI 2.0."
}

refresh_models_cache() {
  local trae_cli="$1"
  if ! "${trae_cli}" models --json >/dev/null 2>&1; then
    if [[ -s "${trae_models_path}" ]]; then
      warn "Could not refresh TRAE models; using the existing cache."
    else
      warn "Could not create the TRAE model cache. The proxy will expose its fallback model until the cache is available."
    fi
  fi
}

yaml_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '%s' "${value}"
}

generate_api_key() {
  if command -v openssl >/dev/null 2>&1 && openssl rand -hex 24 2>/dev/null; then
    return
  fi
  command -v od >/dev/null 2>&1 || fail "Neither a working openssl nor od command is available to generate the API key."
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n'
}

write_config() {
  local trae_cli="$1"
  local api_key=""

  mkdir -p -- "${state_dir}" "${auth_dir}"
  chmod 700 "${state_dir}" "${auth_dir}"

  if [[ ! -s "${api_key_path}" ]]; then
    generate_api_key >"${api_key_path}"
    printf '\n' >>"${api_key_path}"
    info "Generated an API key in ${api_key_path}."
  fi
  chmod 600 "${api_key_path}"
  api_key="$(tr -d '\r\n' <"${api_key_path}")"
  [[ -n "${api_key}" ]] || fail "API key file '${api_key_path}' is empty."

  cat >"${config_path}" <<EOF
# Generated by trae-quickstart.sh. Re-run the script to refresh this file.
host: "$(yaml_escape "${proxy_host}")"
port: ${proxy_port}
auth-dir: "$(yaml_escape "${auth_dir}")"
api-keys:
  - "$(yaml_escape "${api_key}")"
logging-to-file: false
error-logs-max-files: 10
request-retry: 1
traecli:
  enabled: true
  mode: "${proxy_mode}"
  auth-file: "$(yaml_escape "${trae_auth_path}")"
  models-cache: "$(yaml_escape "${trae_models_path}")"
  backend-variant: "max"
  include-cache-models: true
  native-fallback-to-exec: false
  headers:
    x-traecli-forward-tools: "true"
  models:
    - name: "GPT-5.6 Sol"
      alias: "gpt-5.6-sol"
      model-name: "gpt-5.6-sol__max"
      description: "TRAE GPT-5.6 Sol via CLIProxyAPI"
    - name: "GPT-5.6 Luna"
      alias: "gpt-5.6-luna"
      model-name: "gpt-5.6-luna__max"
      description: "TRAE GPT-5.6 Luna via CLIProxyAPI"
    - name: "GPT-5.6 Terra"
      alias: "gpt-5.6-terra"
      model-name: "gpt-5.6-terra__max"
      description: "TRAE GPT-5.6 Terra via CLIProxyAPI"
    - name: "OpenRouter 3o"
      alias: "openrouter-3o"
      aliases:
        - "claude-fable-5"
        - "claude-sonnet-5"
        - "claude-haiku-4-5"
      alias-names:
        "claude-sonnet-5": "CC Execution Route · GPT-5.5"
        "claude-haiku-4-5": "CC Background Route · GPT-5.4"
      model-name: "openrouter-3o__max"
      description: "TRAE OpenRouter 3o via CLIProxyAPI"
    - name: "GPT-5.5"
      alias: "gpt-5.5"
      model-name: "gpt-5.5__max"
      description: "TRAE GPT-5.5 via CLIProxyAPI"
    - name: "OpenRouter 2o"
      alias: "openrouter-2o"
      model-name: "openrouter-2o__max"
      description: "TRAE OpenRouter 2o via CLIProxyAPI"
    - name: "Seed 2.1 Pro"
      alias: "seed-2.1-pro"
      model-name: "Doubao-Seed-2.1-Pro__dev"
      description: "TRAE Seed 2.1 Pro via CLIProxyAPI"
    - name: "Seed 2.1 Turbo"
      alias: "seed-2.1-turbo"
      model-name: "Doubao-Seed-2.1-Turbo__dev"
      description: "TRAE Seed 2.1 Turbo via CLIProxyAPI"
    - name: "GPT-5.4"
      alias: "gpt-5.4"
      model-name: "gpt-5.4__max"
      description: "TRAE GPT-5.4 via CLIProxyAPI"
    - name: "GPT-5.2"
      alias: "gpt-5.2"
      model-name: "gpt-5.2__dev"
      description: "TRAE GPT-5.2 via CLIProxyAPI"
    - name: "DeepSeek V4 Pro"
      alias: "deepseek-v4-pro"
      model-name: "DeepSeek-V4-Pro__dev"
      description: "TRAE DeepSeek V4 Pro via CLIProxyAPI"
    - name: "DeepSeek V4 Flash"
      alias: "deepseek-v4-flash"
      model-name: "DeepSeek-V4-Flash__dev"
      description: "TRAE DeepSeek V4 Flash via CLIProxyAPI"
    - name: "CC Reasoning Route · OpenRouter 3o"
      alias: "claude-opus-5"
      model-name: "openrouter-3o__max"
      description: "TRAE automatic Claude model via CLIProxyAPI"
  exec-path: "$(yaml_escape "${trae_cli}")"
  exec-workdir: "$(yaml_escape "${state_dir}")"
  exec-sandbox: "read-only"
EOF
  chmod 600 "${config_path}"
}

find_go() {
  local candidate=""
  if [[ -n "${go_bin}" ]]; then
    if command -v "${go_bin}" >/dev/null 2>&1; then
      command -v "${go_bin}"
      return 0
    fi
    [[ -x "${go_bin}" ]] || return 1
    printf '%s\n' "${go_bin}"
    return 0
  fi
  if command -v go >/dev/null 2>&1; then
    command -v go
    return 0
  fi
  for candidate in /usr/local/go/bin/go "${HOME}/.local/go/bin/go"; do
    if [[ -x "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  return 1
}

require_go() {
  go_bin="$(find_go)" || fail "Go 1.26+ is required to build this repository."

  local go_version=""
  local version_core=""
  local major=""
  local minor=""
  go_version="$(
    cd -- "${repo_dir}"
    "${go_bin}" env GOVERSION 2>/dev/null || "${go_bin}" version | awk '{print $3}'
  )"
  version_core="${go_version#go}"
  version_core="${version_core%%[^0-9.]*}"
  IFS=. read -r major minor _ <<<"${version_core}"
  major="${major:-0}"
  minor="${minor:-0}"
  if ((major < 1 || (major == 1 && minor < 26))); then
    fail "Go 1.26+ is required; found ${go_version}."
  fi
}

build_proxy() {
  require_go
  mkdir -p -- "${state_dir}" "${go_cache_dir}"

  local version="dev"
  local commit="none"
  local build_date=""
  if command -v git >/dev/null 2>&1 && git -C "${repo_dir}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    version="$(git -C "${repo_dir}" describe --tags --always --dirty 2>/dev/null || printf 'dev')"
    commit="$(git -C "${repo_dir}" rev-parse --short HEAD 2>/dev/null || printf 'none')"
  fi
  build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  local active_go_version=""
  active_go_version="$(
    cd -- "${repo_dir}"
    "${go_bin}" env GOVERSION
  )"
  info "Building CLIProxyAPI with Go ${active_go_version}..."
  (
    cd -- "${repo_dir}"
    GOCACHE="${go_cache_dir}" "${go_bin}" build -buildvcs=false \
      -ldflags="-X 'main.Version=${version}' -X 'main.Commit=${commit}' -X 'main.BuildDate=${build_date}'" \
      -o "${binary_path}" ./cmd/server
  )
}

read_pid() {
  [[ -s "${pid_path}" ]] || return 1
  local pid=""
  pid="$(tr -d '[:space:]' <"${pid_path}")"
  case "${pid}" in
    '' | *[!0-9]*) return 1 ;;
  esac
  printf '%s\n' "${pid}"
}

pid_matches_proxy() {
  local pid="$1"
  kill -0 "${pid}" >/dev/null 2>&1 || return 1
  local command_line=""
  command_line="$(ps -p "${pid}" -o command= 2>/dev/null || true)"
  [[ "${command_line}" == *"${binary_path}"* && "${command_line}" == *"${config_path}"* ]]
}

running_pid() {
  local pid=""
  pid="$(read_pid)" || return 1
  pid_matches_proxy "${pid}" || return 1
  printf '%s\n' "${pid}"
}

health_url() {
  local url_host="${proxy_host}"
  if [[ "${url_host}" == "0.0.0.0" || -z "${url_host}" ]]; then
    url_host="127.0.0.1"
  elif [[ "${url_host}" == "::" ]]; then
    url_host="[::1]"
  elif [[ "${url_host}" == *:* && "${url_host}" != \[*\] ]]; then
    url_host="[${url_host}]"
  fi
  printf 'http://%s:%s/healthz\n' "${url_host}" "${proxy_port}"
}

base_url() {
  local health=""
  health="$(health_url)"
  printf '%s\n' "${health%/healthz}"
}

models_url() {
  printf '%s/v1/models\n' "$(base_url)"
}

curl_with_api_key() {
  local api_key=""
  chmod 600 "${api_key_path}"
  api_key="$(tr -d '\r\n' <"${api_key_path}")"
  [[ -n "${api_key}" ]] || return 1
  printf 'Authorization: Bearer %s\n' "${api_key}" | curl --header @- "$@"
}

proxy_is_ready() {
  command -v curl >/dev/null 2>&1 || return 1
  [[ -s "${api_key_path}" ]] || return 1
  curl_with_api_key -fsS --max-time 2 "$(models_url)" >/dev/null 2>&1
}

verify_background_stability() {
  local pid="$1"
  local attempt=0
  info "Authenticated readiness passed; verifying background stability for 2 seconds..."
  while ((attempt < 10)); do
    sleep 0.2
    if ! pid_matches_proxy "${pid}"; then
      tail -n 40 "${log_path}" >&2 || true
      fail "The proxy exited during the post-readiness stability check. Use './trae-quickstart.sh foreground' under a process manager for reliable operation. See ${log_path}."
    fi
    if ! proxy_is_ready; then
      tail -n 40 "${log_path}" >&2 || true
      fail "The proxy became unhealthy during the post-readiness stability check. See ${log_path}."
    fi
    ((attempt += 1))
  done
}

warn_if_noninteractive_background() {
  if [[ ! -t 0 || ! -t 1 ]]; then
    warn "This command is running non-interactively. The surrounding agent or job supervisor may terminate background child processes after the command exits."
    warn "For reliable long-running use, run './trae-quickstart.sh foreground' under systemd or another process manager. See TRAE_DEPLOY_CN.md."
  fi
}

show_start_guidance() {
  info "Start in the background: ./trae-quickstart.sh start"
  info "Long-running or non-interactive use: run './trae-quickstart.sh foreground' under a process manager; see TRAE_DEPLOY_CN.md."
}

prepare_runtime() {
  validate_settings
  local trae_cli=""
  trae_cli="$(require_trae_cli)"
  info "Using $("${trae_cli}" --version 2>/dev/null | head -n 1) at ${trae_cli}."
  ensure_trae_login "${trae_cli}"
  refresh_models_cache "${trae_cli}"
  write_config "${trae_cli}"
  build_proxy
}

start_proxy() {
  local existing_pid=""
  if existing_pid="$(running_pid)"; then
    if proxy_is_ready; then
      info "Proxy is already running with PID ${existing_pid} at $(base_url)."
      return 0
    fi
    fail "Proxy PID ${existing_pid} is running, but its authenticated readiness check failed. Inspect './trae-quickstart.sh logs' or run './trae-quickstart.sh restart'."
  fi
  if proxy_is_ready; then
    fail "The proxy endpoint at $(base_url) is already responding with this deployment's API key, but its PID is not tracked. It may be managed by systemd or another supervisor; stop it there before starting a background instance."
  fi
  rm -f -- "${pid_path}"

  prepare_runtime
  : >"${log_path}"
  info "Starting the proxy in the background..."
  (
    cd -- "${repo_dir}"
    nohup "${binary_path}" --config "${config_path}" >>"${log_path}" 2>&1 </dev/null &
    printf '%s\n' "$!" >"${pid_path}"
  )

  local pid=""
  pid="$(read_pid)" || fail "The proxy did not write a valid PID."
  local attempt=0
  while ((attempt < 50)); do
    if ! pid_matches_proxy "${pid}"; then
      tail -n 40 "${log_path}" >&2 || true
      fail "The proxy exited during startup. See ${log_path}."
    fi
    if proxy_is_ready; then
      pid_matches_proxy "${pid}" || fail "The proxy exited immediately after passing its readiness check. See ${log_path}."
      verify_background_stability "${pid}"
      info "Proxy is ready at $(base_url) (PID ${pid})."
      info "API key: $(printf 'cat %q' "${api_key_path}")"
      info "Models: ./trae-quickstart.sh models"
      info "Claude Code: ./trae-client-setup.sh claude"
      info "Codex CLI: ./trae-client-setup.sh codex"
      warn_if_noninteractive_background
      return 0
    fi
    sleep 0.2
    ((attempt += 1))
  done

  tail -n 40 "${log_path}" >&2 || true
  fail "The proxy did not pass its authenticated readiness check within 10 seconds. See ${log_path}."
}

foreground_proxy() {
  if running_pid >/dev/null 2>&1; then
    fail "A background proxy is already running. Stop it first."
  fi
  prepare_runtime
  info "Starting the proxy in the foreground at $(base_url)..."
  cd -- "${repo_dir}"
  exec "${binary_path}" --config "${config_path}"
}

stop_proxy() {
  local pid=""
  if ! pid="$(read_pid)"; then
    info "Proxy is not running."
    return 0
  fi
  if ! pid_matches_proxy "${pid}"; then
    warn "Ignoring stale PID file; PID ${pid} is not this proxy."
    rm -f -- "${pid_path}"
    return 0
  fi

  info "Stopping proxy PID ${pid}..."
  kill -TERM "${pid}"
  local attempt=0
  while kill -0 "${pid}" >/dev/null 2>&1 && ((attempt < 50)); do
    sleep 0.2
    ((attempt += 1))
  done
  if kill -0 "${pid}" >/dev/null 2>&1; then
    warn "Proxy did not stop gracefully; sending SIGKILL."
    kill -KILL "${pid}"
  fi
  rm -f -- "${pid_path}"
  info "Proxy stopped."
}

show_status() {
  local pid=""
  if pid="$(running_pid)"; then
    if proxy_is_ready; then
      info "running: PID ${pid}, healthy, $(base_url)"
    else
      info "running: PID ${pid}, authenticated readiness check failed, $(base_url)"
      info "Next: inspect './trae-quickstart.sh logs' or run './trae-quickstart.sh restart'."
      return 1
    fi
  else
    if proxy_is_ready; then
      info "running: healthy, $(base_url), but not tracked by the quickstart PID file"
      info "The proxy is likely managed by systemd or another supervisor; use that supervisor for process control."
      return 0
    fi
    info "stopped"
    show_start_guidance
    return 1
  fi
}

show_logs() {
  [[ -f "${log_path}" ]] || fail "No log file exists yet. Start the proxy first."
  tail -n 100 -f "${log_path}"
}

show_api_key() {
  [[ -s "${api_key_path}" ]] || fail "No API key exists yet. Start the proxy first."
  chmod 600 "${api_key_path}"
  tr -d '\r\n' <"${api_key_path}"
  printf '\n'
}

show_models() {
  [[ -s "${api_key_path}" ]] || fail "No API key exists yet. Start the proxy first."

  local models_json=""
  models_json="$(curl_with_api_key -fsS --max-time 5 "$(models_url)")" || fail "Proxy is not ready or rejected its API key. Start it first."

  if command -v jq >/dev/null 2>&1; then
    printf '%s\n' "${models_json}" | jq -r '.data[] | [.id, .owned_by] | @tsv'
  else
    printf '%s\n' "${models_json}"
  fi
}

doctor() {
  validate_settings
  local failed=0
  local trae_cli=""

  if go_bin="$(find_go)"; then
    local active_go_version=""
    active_go_version="$(
      cd -- "${repo_dir}"
      "${go_bin}" env GOVERSION 2>/dev/null || "${go_bin}" version
    )"
    info "Go: ${active_go_version} (${go_bin})"
  else
    warn "Go: not found (1.26+ is required)"
    failed=1
  fi

  if trae_cli="$(find_trae_cli)"; then
    info "TRAE CLI: $("${trae_cli}" --version 2>/dev/null | head -n 1) (${trae_cli})"
    if "${trae_cli}" login status >/dev/null 2>&1; then
      info "TRAE login: valid"
    else
      warn "TRAE login: required"
      failed=1
    fi
  else
    warn "TRAE CLI 2.0: not found"
    failed=1
  fi

  if [[ -s "${trae_auth_path}" ]]; then
    info "TRAE auth: ${trae_auth_path}"
  else
    warn "TRAE auth file missing: ${trae_auth_path}"
    failed=1
  fi
  if [[ -s "${trae_models_path}" ]]; then
    info "TRAE models cache: ${trae_models_path}"
  else
    warn "TRAE models cache missing: ${trae_models_path}"
  fi

  if ! show_status; then
    failed=1
  fi
  return "${failed}"
}

command_name="${1:-start}"
case "${command_name}" in
  start) start_proxy ;;
  prepare) prepare_runtime ;;
  foreground) foreground_proxy ;;
  stop) stop_proxy ;;
  restart)
    stop_proxy
    start_proxy
    ;;
  status) show_status ;;
  logs) show_logs ;;
  doctor) doctor ;;
  models) show_models ;;
  api-key) show_api_key ;;
  help | -h | --help) usage ;;
  *)
    usage >&2
    fail "Unknown command '${command_name}'."
    ;;
esac
