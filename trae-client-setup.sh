#!/usr/bin/env bash

set -euo pipefail
umask 077

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
state_dir="${TRAE_PROXY_STATE_DIR:-${repo_dir}/.trae-proxy}"
if [[ "${state_dir}" != /* ]]; then
  state_dir="$(pwd -P)/${state_dir}"
fi
api_key_path="${state_dir}/api-key"
proxy_config_path="${state_dir}/config.yaml"
claude_key_helper_path="${state_dir}/claude-api-key-helper.sh"
codex_key_helper_path="${state_dir}/codex-api-key-helper.sh"

proxy_host="${TRAE_PROXY_HOST:-}"
proxy_port="${TRAE_PROXY_PORT:-}"
if [[ -s "${proxy_config_path}" ]]; then
  if [[ -z "${proxy_host}" ]]; then
    proxy_host="$(sed -n 's/^host: "\(.*\)"$/\1/p' "${proxy_config_path}" | head -n 1)"
  fi
  if [[ -z "${proxy_port}" ]]; then
    proxy_port="$(sed -n 's/^port: \([0-9][0-9]*\)$/\1/p' "${proxy_config_path}" | head -n 1)"
  fi
fi
proxy_host="${proxy_host:-127.0.0.1}"
proxy_port="${proxy_port:-8317}"
claude_config_dir="${CLAUDE_CONFIG_DIR:-${HOME}/.claude}"
claude_settings_path="${TRAE_CLAUDE_SETTINGS_PATH:-${claude_config_dir}/settings.json}"
claude_gateway_models_path="${claude_config_dir}/cache/gateway-models.json"
claude_bin="${TRAE_CLAUDE_BIN:-}"
claude_model="${TRAE_CLAUDE_MODEL:-openrouter-3o}"
# Only overwrite the persisted top-level "model" when the caller explicitly
# requests one; otherwise keep whatever the user selected via /model.
claude_model_explicit=""
if [[ -n "${TRAE_CLAUDE_MODEL:-}" ]]; then
  claude_model_explicit="1"
fi
claude_model_name="${TRAE_CLAUDE_MODEL_NAME:-}"
claude_max_context_tokens=""
claude_fable_model="${TRAE_CLAUDE_FABLE_MODEL:-gpt-5.6-sol}"
claude_fable_model_name="${TRAE_CLAUDE_FABLE_MODEL_NAME:-GPT-5.6 Sol}"
claude_opus_model="${TRAE_CLAUDE_OPUS_MODEL:-openrouter-3o}"
claude_opus_model_name="${TRAE_CLAUDE_OPUS_MODEL_NAME:-OpenRouter 3o}"
claude_sonnet_model="${TRAE_CLAUDE_SONNET_MODEL:-gpt-5.5}"
claude_sonnet_model_name="${TRAE_CLAUDE_SONNET_MODEL_NAME:-GPT-5.5}"
claude_haiku_model="${TRAE_CLAUDE_HAIKU_MODEL:-gpt-5.4}"
claude_haiku_model_name="${TRAE_CLAUDE_HAIKU_MODEL_NAME:-GPT-5.4}"
claude_backup_limit="${TRAE_CLAUDE_BACKUP_LIMIT:-5}"
json_tool_preference="${TRAE_CLAUDE_JSON_TOOL:-auto}"
json_tool_kind=""
json_tool_bin=""
codex_config_dir="${CODEX_HOME:-${HOME}/.codex}"
codex_config_path="${codex_config_dir}/config.toml"
codex_bin="${TRAE_CODEX_BIN:-}"
codex_model="${TRAE_CODEX_MODEL:-gpt-5.5}"
codex_backup_limit="${TRAE_CODEX_BACKUP_LIMIT:-5}"
codex_provider_id="trae_cli_proxy"
codex_marker_start="# BEGIN TRAE CLIProxyAPI CODEX CONFIG"
codex_marker_end="# END TRAE CLIProxyAPI CODEX CONFIG"
allow_offline=0

info() {
  printf '[trae-client-setup] %s\n' "$*" >&2
}

warn() {
  printf '[trae-client-setup] WARNING: %s\n' "$*" >&2
}

fail() {
  printf '[trae-client-setup] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: ./trae-client-setup.sh <client> [options]

Clients:
  claude [--allow-offline]
          Configure Claude Code to use the local TRAE proxy
  codex [--allow-offline]
          Configure Codex CLI to use the local TRAE proxy
  help    Show this help

Environment overrides:
  TRAE_PROXY_HOST              Proxy host (default: saved value or 127.0.0.1)
  TRAE_PROXY_PORT              Proxy port (default: saved value or 8317)
  TRAE_PROXY_STATE_DIR         Proxy runtime directory (default: .trae-proxy)
  TRAE_CLAUDE_BIN              Explicit Claude Code executable path
  TRAE_CLAUDE_SETTINGS_PATH    Claude Code settings file (default: ~/.claude/settings.json)
  TRAE_CLAUDE_MODEL            Main Claude Code model (default: openrouter-3o)
  TRAE_CLAUDE_FABLE_MODEL      Fable slot model (default: gpt-5.6-sol)
  TRAE_CLAUDE_OPUS_MODEL       Opus slot model (default: openrouter-3o)
  TRAE_CLAUDE_SONNET_MODEL     Sonnet slot model (default: gpt-5.5)
  TRAE_CLAUDE_HAIKU_MODEL      Haiku slot model (default: gpt-5.4)
  TRAE_CLAUDE_*_MODEL_NAME     Display name for the corresponding model slot
  TRAE_CLAUDE_BACKUP_LIMIT     Settings backups to retain; 0 keeps all (default: 5)
  TRAE_CLAUDE_JSON_TOOL        JSON tool: auto, jq, node, or python3 (default: auto)
  TRAE_CODEX_BIN               Explicit Codex CLI executable path
  TRAE_CODEX_MODEL             Codex model (default: gpt-5.5)
  TRAE_CODEX_BACKUP_LIMIT      Config backups to retain; 0 keeps all (default: 5)
EOF
}

validate_proxy_settings() {
  case "${proxy_port}" in
    '' | *[!0-9]*) fail "TRAE_PROXY_PORT must be an integer, got '${proxy_port}'." ;;
  esac
  proxy_port=$((10#${proxy_port}))
  if ((proxy_port < 1 || proxy_port > 65535)); then
    fail "TRAE_PROXY_PORT must be between 1 and 65535, got '${proxy_port}'."
  fi
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

check_proxy() {
  [[ -s "${api_key_path}" ]] || fail "Proxy API key not found. Run './trae-quickstart.sh' first."
  if ! command -v curl >/dev/null 2>&1; then
    if ((allow_offline)); then
      warn "curl is not available; skipping the authenticated proxy check because --allow-offline was specified."
      return
    fi
    fail "curl is required to verify the proxy. Install it or rerun with --allow-offline."
  fi
  if curl_with_api_key -fsS --max-time 2 "$(models_url)" >/dev/null 2>&1; then
    return
  fi
  if ((allow_offline)); then
    warn "Proxy did not accept its API key at $(base_url); the configured client will work after the proxy starts."
    return
  fi
  fail "Proxy did not accept its API key at $(base_url). Start it first or rerun with --allow-offline."
}

find_claude_code() {
  local candidate=""
  if [[ -n "${claude_bin}" ]]; then
    if command -v "${claude_bin}" >/dev/null 2>&1; then
      command -v "${claude_bin}"
      return 0
    fi
    [[ -x "${claude_bin}" ]] || return 1
    printf '%s\n' "${claude_bin}"
    return 0
  fi
  if command -v claude >/dev/null 2>&1; then
    command -v claude
    return 0
  fi
  for candidate in "${HOME}/.local/bin/claude" /usr/local/bin/claude /opt/homebrew/bin/claude; do
    if [[ -x "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  return 1
}

find_json_tool() {
  local candidate=""
  local -a candidates=()
  case "${json_tool_preference}" in
    auto) candidates=(jq node python3) ;;
    jq | node | python3) candidates=("${json_tool_preference}") ;;
    *) fail "TRAE_CLAUDE_JSON_TOOL must be auto, jq, node, or python3; got '${json_tool_preference}'." ;;
  esac

  for candidate in "${candidates[@]}"; do
    if command -v "${candidate}" >/dev/null 2>&1; then
      json_tool_kind="${candidate}"
      json_tool_bin="$(command -v "${candidate}")"
      return 0
    fi
  done
  return 1
}

require_claude_code() {
  claude_bin="$(find_claude_code)" || fail "Claude Code was not found. Install it using your preferred method, then retry or set TRAE_CLAUDE_BIN to its executable path."
  if ! find_json_tool; then
    if [[ "${json_tool_preference}" == "auto" ]]; then
      fail "A JSON processor is required to preserve existing Claude Code settings. Install jq, Node.js, or Python 3, then retry."
    fi
    fail "Requested JSON processor '${json_tool_preference}' was not found. Install it or use TRAE_CLAUDE_JSON_TOOL=auto."
  fi
  case "${claude_backup_limit}" in
    '' | *[!0-9]*) fail "TRAE_CLAUDE_BACKUP_LIMIT must be a non-negative integer, got '${claude_backup_limit}'." ;;
  esac
  claude_backup_limit=$((10#${claude_backup_limit}))
}

find_codex_cli() {
  local candidate=""
  if [[ -n "${codex_bin}" ]]; then
    if command -v "${codex_bin}" >/dev/null 2>&1; then
      command -v "${codex_bin}"
      return 0
    fi
    [[ -x "${codex_bin}" ]] || return 1
    printf '%s\n' "${codex_bin}"
    return 0
  fi
  if command -v codex >/dev/null 2>&1; then
    command -v codex
    return 0
  fi
  for candidate in "${HOME}/.local/bin/codex" /usr/local/bin/codex /opt/homebrew/bin/codex; do
    if [[ -x "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  return 1
}

require_codex_cli() {
  codex_bin="$(find_codex_cli)" || fail "Codex CLI was not found. Install it using your preferred method, then retry or set TRAE_CODEX_BIN to its executable path."
  case "${codex_backup_limit}" in
    '' | *[!0-9]*) fail "TRAE_CODEX_BACKUP_LIMIT must be a non-negative integer, got '${codex_backup_limit}'." ;;
  esac
  codex_backup_limit=$((10#${codex_backup_limit}))
  [[ -n "${codex_model}" ]] || fail "TRAE_CODEX_MODEL must not be empty."
}

write_claude_key_helper() {
  local temp_path=""
  temp_path="$(mktemp "${state_dir}/claude-api-key-helper.XXXXXX")"
  {
    printf '#!/usr/bin/env bash\n\n'
    printf 'set -euo pipefail\n\n'
    printf 'export TRAE_PROXY_STATE_DIR=%q\n\n' "${state_dir}"
    printf 'exec %q api-key\n' "${repo_dir}/trae-quickstart.sh"
  } >"${temp_path}"
  chmod 700 "${temp_path}"
  mv -- "${temp_path}" "${claude_key_helper_path}"
}

write_claude_settings_with_jq() {
  local input_path="$1"
  local output_path="$2"
  local key_helper_command="$3"

  local jq_filter='
    if type != "object" then
      error("Claude Code settings must contain a JSON object")
    elif .env != null and (.env | type) != "object" then
      error("Claude Code settings env must contain a JSON object")
    elif .modelOverrides != null and (.modelOverrides | type) != "object" then
      error("Claude Code settings modelOverrides must contain a JSON object")
    else
      .
    end
    | .apiKeyHelper = $api_key_helper
    | (if $model_explicit == "1" or (.model | type) != "string" or (.model | test("\\S") | not) then .model = $model else . end)
    | .modelOverrides = (
      (.modelOverrides // {})
      + {
          "claude-fable-5": $fable_model,
          "claude-opus-5": $opus_model,
          "claude-sonnet-5": $sonnet_model,
          "claude-haiku-4-5": $haiku_model
        }
    )
    | .env = (
      ((.env // {}) | del(
        .ANTHROPIC_AUTH_TOKEN,
        .ANTHROPIC_API_KEY,
        .ANTHROPIC_MODEL,
        .ANTHROPIC_SMALL_FAST_MODEL,
        .CLAUDE_CODE_MAX_CONTEXT_TOKENS,
        .ANTHROPIC_CUSTOM_MODEL_OPTION,
        .ANTHROPIC_CUSTOM_MODEL_OPTION_NAME,
        .ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION,
        .ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES,
        .ANTHROPIC_DEFAULT_SONNET_MODEL,
        .ANTHROPIC_DEFAULT_SONNET_MODEL_NAME,
        .ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION,
        .ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES,
        .ANTHROPIC_DEFAULT_HAIKU_MODEL,
        .ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME,
        .ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION,
        .ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES
      ))
      + {
          "ANTHROPIC_BASE_URL": $base_url,
          "ANTHROPIC_DEFAULT_FABLE_MODEL": $fable_model,
          "ANTHROPIC_DEFAULT_FABLE_MODEL_NAME": $fable_model_name,
          "ANTHROPIC_DEFAULT_FABLE_MODEL_DESCRIPTION": ("TRAE " + $fable_model_name + " via CLIProxyAPI"),
          "ANTHROPIC_DEFAULT_OPUS_MODEL": $opus_model,
          "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME": $opus_model_name,
          "ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION": ("TRAE " + $opus_model_name + " via CLIProxyAPI"),
          "ANTHROPIC_DEFAULT_SONNET_MODEL": $sonnet_model,
          "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": $sonnet_model_name,
          "ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION": ("TRAE " + $sonnet_model_name + " via CLIProxyAPI"),
          "ANTHROPIC_DEFAULT_HAIKU_MODEL": $haiku_model,
          "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": $haiku_model_name,
          "ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION": ("TRAE " + $haiku_model_name + " via CLIProxyAPI"),
          "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"
        }
      + (if $max_ctx != "" then {"CLAUDE_CODE_MAX_CONTEXT_TOKENS": $max_ctx} else {} end)
    )
  '
  local -a jq_args=(
    --arg base_url "$(base_url)"
    --arg api_key_helper "${key_helper_command}"
    --arg model "${claude_model}"
    --arg model_explicit "${claude_model_explicit}"
    --arg max_ctx "${claude_max_context_tokens}"
    --arg fable_model "${claude_fable_model}"
    --arg fable_model_name "${claude_fable_model_name}"
    --arg opus_model "${claude_opus_model}"
    --arg opus_model_name "${claude_opus_model_name}"
    --arg sonnet_model "${claude_sonnet_model}"
    --arg sonnet_model_name "${claude_sonnet_model_name}"
    --arg haiku_model "${claude_haiku_model}"
    --arg haiku_model_name "${claude_haiku_model_name}"
  )

  if [[ -n "${input_path}" ]]; then
    "${json_tool_bin}" "${jq_args[@]}" "${jq_filter}" "${input_path}" >"${output_path}"
  else
    "${json_tool_bin}" -n "${jq_args[@]}" "{} | ${jq_filter}" >"${output_path}"
  fi
}

write_claude_settings_with_node() {
  local input_path="$1"
  local output_path="$2"
  local key_helper_command="$3"

  "${json_tool_bin}" -e '
    const fs = require("fs");
    const [inputPath, outputPath, apiKeyHelper, baseURL, model,
      fableModel, fableModelName, opusModel, opusModelName,
      sonnetModel, sonnetModelName, haikuModel, haikuModelName, maxCtx,
      modelExplicit] = process.argv.slice(1);
    let settings = {};
    if (inputPath) {
      settings = JSON.parse(fs.readFileSync(inputPath, "utf8"));
    }
    if (settings === null || Array.isArray(settings) || typeof settings !== "object") {
      throw new Error("Claude Code settings must contain a JSON object");
    }
    if (settings.env !== undefined && settings.env !== null &&
        (Array.isArray(settings.env) || typeof settings.env !== "object")) {
      throw new Error("Claude Code settings env must contain a JSON object");
    }
    if (settings.modelOverrides !== undefined && settings.modelOverrides !== null &&
        (Array.isArray(settings.modelOverrides) || typeof settings.modelOverrides !== "object")) {
      throw new Error("Claude Code settings modelOverrides must contain a JSON object");
    }
    const env = {...(settings.env || {})};
    delete env.ANTHROPIC_AUTH_TOKEN;
    delete env.ANTHROPIC_API_KEY;
    delete env.ANTHROPIC_MODEL;
    delete env.ANTHROPIC_SMALL_FAST_MODEL;
    delete env.CLAUDE_CODE_MAX_CONTEXT_TOKENS;
    delete env.ANTHROPIC_CUSTOM_MODEL_OPTION;
    delete env.ANTHROPIC_CUSTOM_MODEL_OPTION_NAME;
    delete env.ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION;
    delete env.ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES;
    for (const slot of ["SONNET", "HAIKU"]) {
      delete env[`ANTHROPIC_DEFAULT_${slot}_MODEL`];
      delete env[`ANTHROPIC_DEFAULT_${slot}_MODEL_NAME`];
      delete env[`ANTHROPIC_DEFAULT_${slot}_MODEL_DESCRIPTION`];
      delete env[`ANTHROPIC_DEFAULT_${slot}_MODEL_SUPPORTED_CAPABILITIES`];
    }
    Object.assign(env, {
      ANTHROPIC_BASE_URL: baseURL,
      ANTHROPIC_DEFAULT_FABLE_MODEL: fableModel,
      ANTHROPIC_DEFAULT_FABLE_MODEL_NAME: fableModelName,
      ANTHROPIC_DEFAULT_FABLE_MODEL_DESCRIPTION: `TRAE ${fableModelName} via CLIProxyAPI`,
      ANTHROPIC_DEFAULT_OPUS_MODEL: opusModel,
      ANTHROPIC_DEFAULT_OPUS_MODEL_NAME: opusModelName,
      ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION: `TRAE ${opusModelName} via CLIProxyAPI`,
      ANTHROPIC_DEFAULT_SONNET_MODEL: sonnetModel,
      ANTHROPIC_DEFAULT_SONNET_MODEL_NAME: sonnetModelName,
      ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION: `TRAE ${sonnetModelName} via CLIProxyAPI`,
      ANTHROPIC_DEFAULT_HAIKU_MODEL: haikuModel,
      ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME: haikuModelName,
      ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION: `TRAE ${haikuModelName} via CLIProxyAPI`,
      CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY: "1",
    });
    if (maxCtx) { env.CLAUDE_CODE_MAX_CONTEXT_TOKENS = maxCtx; }
    settings.apiKeyHelper = apiKeyHelper;
    if (modelExplicit === "1" || typeof settings.model !== "string" || !settings.model.trim()) {
      settings.model = model;
    }
    settings.modelOverrides = {
      ...(settings.modelOverrides || {}),
      "claude-fable-5": fableModel,
      "claude-opus-5": opusModel,
      "claude-sonnet-5": sonnetModel,
      "claude-haiku-4-5": haikuModel,
    };
    settings.env = env;
    fs.writeFileSync(outputPath, `${JSON.stringify(settings, null, 2)}\n`, {mode: 0o600});
  ' "${input_path}" "${output_path}" "${key_helper_command}" "$(base_url)" "${claude_model}" \
    "${claude_fable_model}" "${claude_fable_model_name}" "${claude_opus_model}" "${claude_opus_model_name}" \
    "${claude_sonnet_model}" "${claude_sonnet_model_name}" "${claude_haiku_model}" "${claude_haiku_model_name}" \
    "${claude_max_context_tokens}" "${claude_model_explicit}"
}

write_claude_settings_with_python3() {
  local input_path="$1"
  local output_path="$2"
  local key_helper_command="$3"

  "${json_tool_bin}" - "${input_path}" "${output_path}" "${key_helper_command}" "$(base_url)" "${claude_model}" \
    "${claude_fable_model}" "${claude_fable_model_name}" "${claude_opus_model}" "${claude_opus_model_name}" \
    "${claude_sonnet_model}" "${claude_sonnet_model_name}" "${claude_haiku_model}" "${claude_haiku_model_name}" \
    "${claude_max_context_tokens}" "${claude_model_explicit}" <<'PYTHON'
import json
import sys

(
    input_path,
    output_path,
    api_key_helper,
    base_url,
    model,
    fable_model,
    fable_model_name,
    opus_model,
    opus_model_name,
    sonnet_model,
    sonnet_model_name,
    haiku_model,
    haiku_model_name,
    max_ctx,
    model_explicit,
) = sys.argv[1:]
settings = {}
if input_path:
    with open(input_path, encoding="utf-8") as input_file:
        settings = json.load(input_file)
if not isinstance(settings, dict):
    raise ValueError("Claude Code settings must contain a JSON object")

model_overrides = settings.get("modelOverrides")
if model_overrides is None:
    model_overrides = {}
elif not isinstance(model_overrides, dict):
    raise ValueError("Claude Code settings modelOverrides must contain a JSON object")
else:
    model_overrides = dict(model_overrides)

env = settings.get("env")
if env is None:
    env = {}
elif not isinstance(env, dict):
    raise ValueError("Claude Code settings env must contain a JSON object")
else:
    env = dict(env)

env.pop("ANTHROPIC_AUTH_TOKEN", None)
env.pop("ANTHROPIC_API_KEY", None)
env.pop("ANTHROPIC_MODEL", None)
env.pop("ANTHROPIC_SMALL_FAST_MODEL", None)
env.pop("CLAUDE_CODE_MAX_CONTEXT_TOKENS", None)
env.pop("ANTHROPIC_CUSTOM_MODEL_OPTION", None)
env.pop("ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", None)
env.pop("ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION", None)
env.pop("ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES", None)
for slot in ("SONNET", "HAIKU"):
    env.pop(f"ANTHROPIC_DEFAULT_{slot}_MODEL", None)
    env.pop(f"ANTHROPIC_DEFAULT_{slot}_MODEL_NAME", None)
    env.pop(f"ANTHROPIC_DEFAULT_{slot}_MODEL_DESCRIPTION", None)
    env.pop(f"ANTHROPIC_DEFAULT_{slot}_MODEL_SUPPORTED_CAPABILITIES", None)
env.update({
    "ANTHROPIC_BASE_URL": base_url,
    "ANTHROPIC_DEFAULT_FABLE_MODEL": fable_model,
    "ANTHROPIC_DEFAULT_FABLE_MODEL_NAME": fable_model_name,
    "ANTHROPIC_DEFAULT_FABLE_MODEL_DESCRIPTION": f"TRAE {fable_model_name} via CLIProxyAPI",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": opus_model,
    "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME": opus_model_name,
    "ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION": f"TRAE {opus_model_name} via CLIProxyAPI",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": sonnet_model,
    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": sonnet_model_name,
    "ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION": f"TRAE {sonnet_model_name} via CLIProxyAPI",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": haiku_model,
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": haiku_model_name,
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION": f"TRAE {haiku_model_name} via CLIProxyAPI",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
})
if max_ctx:
    env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = max_ctx
model_overrides.update({
    "claude-fable-5": fable_model,
    "claude-opus-5": opus_model,
    "claude-sonnet-5": sonnet_model,
    "claude-haiku-4-5": haiku_model,
})
settings["apiKeyHelper"] = api_key_helper
if model_explicit == "1" or not isinstance(settings.get("model"), str) or not settings["model"].strip():
    settings["model"] = model
settings["modelOverrides"] = model_overrides
settings["env"] = env
with open(output_path, "w", encoding="utf-8") as output_file:
    json.dump(settings, output_file, ensure_ascii=False, indent=2)
    output_file.write("\n")
PYTHON
}

write_claude_settings() {
  case "${json_tool_kind}" in
    jq) write_claude_settings_with_jq "$@" ;;
    node) write_claude_settings_with_node "$@" ;;
    python3) write_claude_settings_with_python3 "$@" ;;
    *) fail "Unsupported JSON processor '${json_tool_kind}'." ;;
  esac
}

write_claude_gateway_models_with_jq() {
  local models_json="$1"
  local output_path="$2"
  local fetched_at="$3"
  local jq_filter='
    if type != "object" then
      error("Claude gateway models response must contain a JSON object")
    elif (.data | type) != "array" then
      error("Claude gateway models response data must contain an array")
    elif any(.data[];
      ((.id | type) != "string") or
      (has("display_name") and ((.display_name | type) != "string"))
    ) then
      error("Claude gateway models response contains invalid model metadata")
    else
      [
        .data[]
        | select(.id | test("^(claude|anthropic)"; "i"))
        | {id} + (if has("display_name") then {display_name} else {} end)
      ]
    end
    | if length == 0 then
        error("Claude gateway models response contains no compatible models")
      else
        .
      end
    | {
        baseUrl: $base_url,
        fetchedAt: $fetched_at,
        models: .
      }
  '

  printf '%s\n' "${models_json}" | "${json_tool_bin}" -e \
    --arg base_url "$(base_url)" \
    --argjson fetched_at "${fetched_at}" \
    "${jq_filter}" >"${output_path}"
  "${json_tool_bin}" -r '.models | length' "${output_path}"
}

write_claude_gateway_models_with_node() {
  local models_json="$1"
  local output_path="$2"
  local fetched_at="$3"

  printf '%s\n' "${models_json}" | "${json_tool_bin}" -e '
    const fs = require("fs");
    const [outputPath, baseURL, fetchedAtRaw] = process.argv.slice(1);
    const payload = JSON.parse(fs.readFileSync(0, "utf8"));
    if (payload === null || Array.isArray(payload) || typeof payload !== "object") {
      throw new Error("Claude gateway models response must contain a JSON object");
    }
    if (!Array.isArray(payload.data)) {
      throw new Error("Claude gateway models response data must contain an array");
    }
    const models = [];
    for (const entry of payload.data) {
      if (entry === null || Array.isArray(entry) || typeof entry !== "object" ||
          typeof entry.id !== "string" ||
          (Object.prototype.hasOwnProperty.call(entry, "display_name") &&
           typeof entry.display_name !== "string")) {
        throw new Error("Claude gateway models response contains invalid model metadata");
      }
      if (!/^(claude|anthropic)/i.test(entry.id)) { continue; }
      const model = {id: entry.id};
      if (Object.prototype.hasOwnProperty.call(entry, "display_name")) {
        model.display_name = entry.display_name;
      }
      models.push(model);
    }
    if (models.length === 0) {
      throw new Error("Claude gateway models response contains no compatible models");
    }
    const cache = {
      baseUrl: baseURL,
      fetchedAt: Number(fetchedAtRaw),
      models,
    };
    fs.writeFileSync(outputPath, `${JSON.stringify(cache, null, 2)}\n`, {mode: 0o600});
    process.stdout.write(`${models.length}\n`);
  ' "${output_path}" "$(base_url)" "${fetched_at}"
}

write_claude_gateway_models_with_python3() {
  local models_json="$1"
  local output_path="$2"
  local fetched_at="$3"

  printf '%s\n' "${models_json}" | "${json_tool_bin}" -c '
import json
import re
import sys

output_path, base_url, fetched_at_raw = sys.argv[1:]
payload = json.load(sys.stdin)
if not isinstance(payload, dict):
    raise ValueError("Claude gateway models response must contain a JSON object")
entries = payload.get("data")
if not isinstance(entries, list):
    raise ValueError("Claude gateway models response data must contain an array")

models = []
for entry in entries:
    if not isinstance(entry, dict) or not isinstance(entry.get("id"), str):
        raise ValueError("Claude gateway models response contains invalid model metadata")
    if "display_name" in entry and not isinstance(entry["display_name"], str):
        raise ValueError("Claude gateway models response contains invalid model metadata")
    if not re.match(r"^(claude|anthropic)", entry["id"], re.IGNORECASE):
        continue
    model = {"id": entry["id"]}
    if "display_name" in entry:
        model["display_name"] = entry["display_name"]
    models.append(model)

if not models:
    raise ValueError("Claude gateway models response contains no compatible models")

cache = {
    "baseUrl": base_url,
    "fetchedAt": int(fetched_at_raw),
    "models": models,
}
with open(output_path, "w", encoding="utf-8") as output_file:
    json.dump(cache, output_file, ensure_ascii=False, indent=2)
    output_file.write("\n")
print(len(models))
' "${output_path}" "$(base_url)" "${fetched_at}"
}

write_claude_gateway_models_cache() {
  case "${json_tool_kind}" in
    jq) write_claude_gateway_models_with_jq "$@" ;;
    node) write_claude_gateway_models_with_node "$@" ;;
    python3) write_claude_gateway_models_with_python3 "$@" ;;
    *) return 1 ;;
  esac
}

prewarm_claude_gateway_models() {
  local claude_version=""
  claude_version="$("${claude_bin}" --version 2>/dev/null | head -n 1)"
  claude_version="${claude_version%% *}"
  claude_version="${claude_version:-unknown}"

  local models_json=""
  models_json="$(curl_with_api_key -fsS --max-time 5 \
    -H 'Anthropic-Version: 2023-06-01' \
    -H "User-Agent: claude-cli/${claude_version} (external, trae-client-setup)" \
    "$(models_url)?limit=1000" 2>/dev/null)" || {
      warn "Could not prewarm Claude Code model cache; Claude Code will retry discovery when it starts."
      return 0
    }
  if [[ -z "${models_json}" ]]; then
    warn "Could not prewarm Claude Code model cache from an empty response; Claude Code will retry discovery when it starts."
    return 0
  fi

  local cache_dir=""
  cache_dir="$(dirname -- "${claude_gateway_models_path}")"
  if ! mkdir -p -- "${cache_dir}" || ! chmod 700 "${cache_dir}"; then
    warn "Could not prepare Claude Code model cache directory ${cache_dir}."
    return 0
  fi
  if [[ -L "${claude_gateway_models_path}" ]] ||
    [[ -e "${claude_gateway_models_path}" && ! -f "${claude_gateway_models_path}" ]]; then
    warn "Could not prewarm Claude Code model cache because ${claude_gateway_models_path} is not a regular file."
    return 0
  fi

  local temp_path=""
  temp_path="$(mktemp "${cache_dir}/gateway-models.json.trae.XXXXXX")" || {
    warn "Could not create a temporary Claude Code model cache file."
    return 0
  }
  local fetched_at_seconds=""
  fetched_at_seconds="$(date +%s)"
  local fetched_at=$((fetched_at_seconds * 1000))
  local model_count=""
  if ! model_count="$(write_claude_gateway_models_cache \
    "${models_json}" "${temp_path}" "${fetched_at}" 2>/dev/null)"; then
    rm -f -- "${temp_path}"
    warn "Could not prewarm Claude Code model cache from the proxy response; keeping any existing cache."
    return 0
  fi
  case "${model_count}" in
    '' | *[!0-9]*)
      rm -f -- "${temp_path}"
      warn "Could not prewarm Claude Code model cache because the generated model count was invalid."
      return 0
      ;;
  esac
  if ! chmod 600 "${temp_path}" || ! mv -- "${temp_path}" "${claude_gateway_models_path}"; then
    rm -f -- "${temp_path}"
    warn "Could not install the Claude Code model cache at ${claude_gateway_models_path}."
    return 0
  fi
  info "Prewarmed Claude Code model cache with ${model_count} models in ${claude_gateway_models_path}."
}

prune_claude_backups() {
  ((claude_backup_limit == 0)) && return

  local candidate=""
  local -a backups=()
  for candidate in "${claude_settings_path}".trae-backup.*; do
    [[ -e "${candidate}" ]] || continue
    backups+=("${candidate}")
  done

  local remove_count=0
  remove_count=$((${#backups[@]} - claude_backup_limit))
  local index=0
  for ((index = 0; index < remove_count; index += 1)); do
    rm -f -- "${backups[index]}"
    info "Removed old Claude Code settings backup ${backups[index]}."
  done
}

# The settings writer has already validated and selected the main model.
read_claude_model() {
  case "${json_tool_kind}" in
    jq) "${json_tool_bin}" -er '.model' "$1" ;;
    node) "${json_tool_bin}" -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).model)' "$1" ;;
    python3) "${json_tool_bin}" -c 'import json, sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["model"])' "$1" ;;
  esac
}

# Resolve the MAIN Claude model's real context window from the proxy's authenticated
# Anthropic /v1/models view so Claude Code stops assuming a default window.
# Leaves claude_max_context_tokens empty (and warns) when it cannot be determined.
resolve_claude_context_window() {
  local settings_path="$1"
  claude_max_context_tokens=""
  command -v curl >/dev/null 2>&1 || { warn "curl unavailable; cannot resolve context window."; return 0; }
  local py=""; py="$(command -v python3 2>/dev/null || true)"
  [[ -n "${py}" ]] || { warn "python3 unavailable; cannot resolve context window."; return 0; }
  local models_json=""
  models_json="$(curl_with_api_key -fsS --max-time 5 \
    -H 'Anthropic-Version: 2023-06-01' \
    -H 'User-Agent: claude-cli/trae-client-setup' \
    "$(models_url)?limit=1000" 2>/dev/null)" || {
    warn "Could not fetch model metadata; leaving context window unset."; return 0; }
  [[ -n "${models_json}" ]] || { warn "Empty model metadata; leaving context window unset."; return 0; }
  local win=""
  win="$(MODELS_JSON="${models_json}" "${py}" - "${settings_path}" "${claude_model_name}" <<'PYWIN' 2>/dev/null
import json, os, sys
with open(sys.argv[1], encoding="utf-8") as f:
    settings = json.load(f)
rows = [m for m in json.loads(os.environ["MODELS_JSON"])["data"] if isinstance(m, dict)]
alias, display = settings["model"], sys.argv[2].strip()
slot = {"fable":"FABLE", "opus":"OPUS", "sonnet":"SONNET", "haiku":"HAIKU"}.get(alias)
alias = settings["modelOverrides"].get(alias) or (settings["env"].get(f"ANTHROPIC_DEFAULT_{slot}_MODEL") if slot else alias)
if not isinstance(alias, str) or not alias.strip():
    sys.exit(0)
def win_of(m):
    for key in ("max_context_length", "context_length", "max_input_tokens"):
        w = m.get(key)
        if type(w) is int and w > 0:
            return w
    return None
# The Anthropic view cloaks non-Claude model IDs as this prefix plus the
# character-reversed public ID.
def public_id(model_id):
    prefix = "claude-fable-5-dd-"
    return model_id[len(prefix):][::-1] if isinstance(model_id, str) and model_id.startswith(prefix) else model_id
matches = ([m for m in rows if m.get("id") == alias]
           + [m for m in rows if public_id(m.get("id")) == public_id(alias)]
           + [m for m in rows if display and m.get("display_name") == display])
for m in matches:
    win = win_of(m)
    if win:
        print(win)
        break
PYWIN
)" || win=""
  if [[ -n "${win}" ]]; then
    claude_max_context_tokens="${win}"
    info "Resolved context window for ${claude_model}: ${win}"
  else
    warn "Could not resolve a context window for '${claude_model}'; Claude Code will use its default assumption."
  fi
}

configure_claude_code() {
  validate_proxy_settings
  require_claude_code
  check_proxy

  local settings_dir=""
  settings_dir="$(dirname -- "${claude_settings_path}")"
  mkdir -p -- "${settings_dir}"

  if [[ -e "${claude_settings_path}" ]]; then
    [[ -f "${claude_settings_path}" ]] || fail "Claude Code settings path is not a regular file: ${claude_settings_path}"
    [[ ! -L "${claude_settings_path}" ]] || fail "Claude Code settings path must not be a symbolic link: ${claude_settings_path}"
    chmod 600 "${claude_settings_path}"
  fi

  local temp_path=""
  temp_path="$(mktemp "${settings_dir}/settings.json.trae.XXXXXX")"
  local key_helper_command=""
  printf -v key_helper_command '%q' "${claude_key_helper_path}"
  local input_path=""
  if [[ -e "${claude_settings_path}" ]]; then
    input_path="${claude_settings_path}"
  fi
  if ! write_claude_settings "${input_path}" "${temp_path}" "${key_helper_command}"; then
    rm -f -- "${temp_path}"
    fail "Failed to update Claude Code settings."
  fi
  if ! claude_model="$(read_claude_model "${temp_path}")"; then
    rm -f -- "${temp_path}"
    fail "Failed to read the effective Claude Code model."
  fi
  resolve_claude_context_window "${temp_path}"
  if ! write_claude_settings "${input_path}" "${temp_path}" "${key_helper_command}"; then
    rm -f -- "${temp_path}"
    fail "Failed to update Claude Code context settings."
  fi
  write_claude_key_helper

  chmod 600 "${temp_path}"
  if [[ -e "${claude_settings_path}" ]] && cmp -s -- "${temp_path}" "${claude_settings_path}"; then
    rm -f -- "${temp_path}"
    chmod 600 "${claude_settings_path}"
    info "Claude Code settings are already up to date in ${claude_settings_path}."
  else
    if [[ -e "${claude_settings_path}" ]]; then
      local backup_path=""
      backup_path="${claude_settings_path}.trae-backup.$(date -u +%Y%m%dT%H%M%SZ).$$"
      cp -- "${claude_settings_path}" "${backup_path}"
      chmod 600 "${backup_path}"
      info "Backed up existing Claude Code settings to ${backup_path}."
    fi
    mv -- "${temp_path}" "${claude_settings_path}"
    info "Configured Claude Code in ${claude_settings_path}."
  fi
  prune_claude_backups
  prewarm_claude_gateway_models

  info "Claude Code: $("${claude_bin}" --version 2>/dev/null | head -n 1) (${claude_bin})"
  info "JSON processor: ${json_tool_kind} (${json_tool_bin})"
  info "API key helper: ${claude_key_helper_path}"
  info "Proxy: $(base_url)"
  info "Main model: ${claude_model}"
  info "Fable model: ${claude_fable_model}"
  info "Opus model: ${claude_opus_model}"
  info "Sonnet model: ${claude_sonnet_model}"
  info "Haiku model: ${claude_haiku_model}"
  if [[ -n "${ANTHROPIC_AUTH_TOKEN:-}" || -n "${ANTHROPIC_API_KEY:-}" ]]; then
    warn "The current shell still defines an Anthropic credential variable; unset it before starting Claude Code so apiKeyHelper can take effect."
  fi
  info "Start Claude Code with $(printf '%q' "${claude_bin}"), then run '/status' to verify the gateway settings."
}

codex_api_base_url() {
  printf '%s/v1\n' "$(base_url)"
}

toml_escape_basic_string() {
  local value="$1"
  if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    return 1
  fi
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\t'/\\t}"
  printf '%s' "${value}"
}

write_codex_key_helper() {
  local temp_path=""
  temp_path="$(mktemp "${state_dir}/codex-api-key-helper.XXXXXX")"
  {
    printf '#!/usr/bin/env bash\n\n'
    printf 'set -euo pipefail\n\n'
    printf 'export TRAE_PROXY_STATE_DIR=%q\n\n' "${state_dir}"
    printf 'exec %q api-key\n' "${repo_dir}/trae-quickstart.sh"
  } >"${temp_path}"
  chmod 700 "${temp_path}"
  mv -- "${temp_path}" "${codex_key_helper_path}"
}

remove_codex_managed_block() {
  local input_path="$1"
  local output_path="$2"
  awk -v marker_start="${codex_marker_start}" -v marker_end="${codex_marker_end}" '
    $0 == marker_start {
      if (in_block || starts > 0) {
        invalid = 1
      }
      in_block = 1
      starts += 1
      next
    }
    $0 == marker_end {
      if (!in_block) {
        invalid = 1
      } else {
        in_block = 0
      }
      ends += 1
      next
    }
    !in_block {
      print
    }
    END {
      if (in_block || starts != ends || invalid) {
        exit 1
      }
    }
  ' "${input_path}" >"${output_path}"
}

codex_provider_conflicts() {
  local config_path="$1"
  local providers_key="(model_providers|\"model_providers\"|'model_providers')"
  local provider_key="(trae_cli_proxy|\"trae_cli_proxy\"|'trae_cli_proxy')"
  awk -v providers_key="${providers_key}" -v provider_key="${provider_key}" '
    function mark_conflict() {
      conflict = 1
      exit
    }
    {
      if ($0 ~ ("^[[:space:]]*\\[\\[?[[:space:]]*" providers_key "[[:space:]]*\\.[[:space:]]*" provider_key "([[:space:]]*\\.|[[:space:]]*\\]\\]?)")) {
        mark_conflict()
      }
      if (at_root && ($0 ~ ("^[[:space:]]*" providers_key "[[:space:]]*\\.[[:space:]]*" provider_key "([[:space:]]*\\.|[[:space:]]*=)"))) {
        mark_conflict()
      }
      if ($0 ~ ("^[[:space:]]*\\[[[:space:]]*" providers_key "[[:space:]]*\\][[:space:]]*(#.*)?$")) {
        at_root = 0
        in_providers = 1
        next
      }
      if ($0 ~ /^[[:space:]]*\[/) {
        at_root = 0
        in_providers = 0
        next
      }
      if (in_providers && ($0 ~ ("^[[:space:]]*" provider_key "([[:space:]]*\\.|[[:space:]]*=)"))) {
        mark_conflict()
      }
    }
    BEGIN {
      at_root = 1
    }
    END {
      exit conflict ? 0 : 1
    }
  ' "${config_path}"
}

write_codex_config() {
  local input_path="$1"
  local output_path="$2"
  local output_dir=""
  output_dir="$(dirname -- "${output_path}")"

  local stripped_path=""
  stripped_path="$(mktemp "${output_dir}/config.toml.trae-base.XXXXXX")"
  if [[ -n "${input_path}" ]]; then
    if ! remove_codex_managed_block "${input_path}" "${stripped_path}"; then
      rm -f -- "${stripped_path}" "${output_path}"
      fail "Codex config contains incomplete or duplicate TRAE managed markers: ${input_path}"
    fi
  else
    : >"${stripped_path}"
  fi

  if codex_provider_conflicts "${stripped_path}"; then
    rm -f -- "${stripped_path}" "${output_path}"
    fail "Codex provider '${codex_provider_id}' already exists outside the TRAE managed block in ${codex_config_path}."
  fi

  local model_escaped=""
  local base_url_escaped=""
  local helper_path_escaped=""
  local model_key="(model|\"model\"|'model')"
  local model_provider_key="(model_provider|\"model_provider\"|'model_provider')"
  model_escaped="$(toml_escape_basic_string "${codex_model}")" || {
    rm -f -- "${stripped_path}" "${output_path}"
    fail "TRAE_CODEX_MODEL must not contain newlines."
  }
  base_url_escaped="$(toml_escape_basic_string "$(codex_api_base_url)")" || {
    rm -f -- "${stripped_path}" "${output_path}"
    fail "The Codex proxy URL must not contain newlines."
  }
  helper_path_escaped="$(toml_escape_basic_string "${codex_key_helper_path}")" || {
    rm -f -- "${stripped_path}" "${output_path}"
    fail "The Codex API key helper path must not contain newlines."
  }

  {
    printf 'model = "%s"\n' "${model_escaped}"
    printf 'model_provider = "%s"\n' "${codex_provider_id}"
    awk -v model_key="${model_key}" -v model_provider_key="${model_provider_key}" '
      BEGIN {
        at_root = 1
        have_content = 0
        pending_blanks = 0
      }
      {
        if (at_root && $0 ~ /^[[:space:]]*\[/) {
          at_root = 0
        }
        if (at_root && ($0 ~ ("^[[:space:]]*" model_key "[[:space:]]*=") ||
                        $0 ~ ("^[[:space:]]*" model_provider_key "[[:space:]]*="))) {
          next
        }
        if ($0 ~ /^[[:space:]]*$/) {
          if (have_content) {
            pending_blanks += 1
          }
          next
        }
        if (!have_content) {
          print ""
          have_content = 1
        } else {
          while (pending_blanks > 0) {
            print ""
            pending_blanks -= 1
          }
        }
        print
        pending_blanks = 0
      }
    ' "${stripped_path}"
    printf '\n%s\n' "${codex_marker_start}"
    printf '[model_providers.%s]\n' "${codex_provider_id}"
    printf 'name = "TRAE via CLIProxyAPI"\n'
    printf 'base_url = "%s"\n' "${base_url_escaped}"
    printf 'wire_api = "responses"\n\n'
    printf '[model_providers.%s.auth]\n' "${codex_provider_id}"
    printf 'command = "%s"\n' "${helper_path_escaped}"
    printf 'refresh_interval_ms = 0\n'
    printf '%s\n' "${codex_marker_end}"
  } >"${output_path}"
  rm -f -- "${stripped_path}"
}

prune_codex_backups() {
  ((codex_backup_limit == 0)) && return

  local candidate=""
  local -a backups=()
  for candidate in "${codex_config_path}".trae-backup.*; do
    [[ -e "${candidate}" ]] || continue
    backups+=("${candidate}")
  done

  local remove_count=0
  remove_count=$((${#backups[@]} - codex_backup_limit))
  local index=0
  for ((index = 0; index < remove_count; index += 1)); do
    rm -f -- "${backups[index]}"
    info "Removed old Codex config backup ${backups[index]}."
  done
}

configure_codex_cli() {
  validate_proxy_settings
  require_codex_cli
  check_proxy

  local config_dir=""
  config_dir="$(dirname -- "${codex_config_path}")"
  mkdir -p -- "${config_dir}"

  if [[ -e "${codex_config_path}" ]]; then
    [[ -f "${codex_config_path}" ]] || fail "Codex config path is not a regular file: ${codex_config_path}"
    [[ ! -L "${codex_config_path}" ]] || fail "Codex config path must not be a symbolic link: ${codex_config_path}"
    chmod 600 "${codex_config_path}"
  fi

  local temp_path=""
  temp_path="$(mktemp "${config_dir}/config.toml.trae.XXXXXX")"
  local input_path=""
  if [[ -e "${codex_config_path}" ]]; then
    input_path="${codex_config_path}"
  fi
  write_codex_config "${input_path}" "${temp_path}"
  write_codex_key_helper

  chmod 600 "${temp_path}"
  if [[ -e "${codex_config_path}" ]] && cmp -s -- "${temp_path}" "${codex_config_path}"; then
    rm -f -- "${temp_path}"
    chmod 600 "${codex_config_path}"
    info "Codex config is already up to date in ${codex_config_path}."
  else
    if [[ -e "${codex_config_path}" ]]; then
      local backup_path=""
      backup_path="${codex_config_path}.trae-backup.$(date -u +%Y%m%dT%H%M%SZ).$$"
      cp -- "${codex_config_path}" "${backup_path}"
      chmod 600 "${backup_path}"
      info "Backed up existing Codex config to ${backup_path}."
    fi
    mv -- "${temp_path}" "${codex_config_path}"
    info "Configured Codex CLI in ${codex_config_path}."
  fi
  prune_codex_backups

  info "Codex CLI: $("${codex_bin}" --version 2>/dev/null | head -n 1) (${codex_bin})"
  info "API key helper: ${codex_key_helper_path}"
  info "Proxy: $(codex_api_base_url)"
  info "Model: ${codex_model}"
  info "Start Codex with $(printf '%q' "${codex_bin}"), then run '/status' to verify the model and provider."
}

client_name="${1:-help}"
case "${client_name}" in
  claude)
    case "${2:-}" in
      '') ;;
      --allow-offline) allow_offline=1 ;;
      *) fail "Unknown Claude Code option '${2}'." ;;
    esac
    (($# <= 2)) || fail "Too many arguments for the Claude Code configuration command."
    configure_claude_code
    ;;
  codex)
    case "${2:-}" in
      '') ;;
      --allow-offline) allow_offline=1 ;;
      *) fail "Unknown Codex CLI option '${2}'." ;;
    esac
    (($# <= 2)) || fail "Too many arguments for the Codex CLI configuration command."
    configure_codex_cli
    ;;
  help | -h | --help) usage ;;
  *)
    usage >&2
    fail "Unknown client '${client_name}'."
    ;;
esac
