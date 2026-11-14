# TRAE 一键部署与操作

本仓库可将已登录的 TRAE CLI 账号转换为 OpenAI、Codex、Claude 和 Gemini 兼容 API。

## 快速开始

开始前请准备：

- [Claude Code](https://github.com/anthropics/claude-code) 或 [Codex CLI](https://developers.openai.com/codex/cli/)，至少安装一个；无需登录 Anthropic 或 OpenAI 账号。
- Go 1.26 或更高版本。
- 本机已安装 TRAE CLI 2.0（`traecli` 或 `traex`），并有可用账号。

然后依次执行：

```bash
# 获取代码并启动 TRAE 转发服务
git clone https://github.com/lctm2005/CLIProxyAPI.git
cd CLIProxyAPI
./trae-quickstart.sh

# 配置并启动 Claude Code
./trae-client-setup.sh claude
claude

# 或配置并启动 Codex CLI
./trae-client-setup.sh codex
codex
```

首次运行转发服务时，如果尚未登录 TRAE CLI，按终端提示完成一次账号授权。进入 Claude Code 或 Codex 后可运行 `/status`，确认代理地址、模型和认证来源已经生效。

## 详细说明

### 常用操作

```bash
./trae-quickstart.sh status     # 进程与健康状态
./trae-quickstart.sh models     # 查看代理暴露的模型
./trae-quickstart.sh logs       # 跟随日志
./trae-quickstart.sh prepare    # 仅生成配置并编译，不启动服务
./trae-quickstart.sh restart    # 重新编译并重启
./trae-quickstart.sh stop       # 停止后台服务
./trae-quickstart.sh doctor     # 检查 Go、CLI、登录态和运行状态
./trae-quickstart.sh api-key    # 显式读取本地 API Key
```

### TRAE 转发服务

首次运行时，`trae-quickstart.sh` 会自动：

1. 检查本机已有的 `traex`/`traecli`。
2. 检查登录状态；未登录时打开一次交互授权。
3. 刷新 TRAE 模型缓存。
4. 在 `.trae-proxy/` 生成独立配置和随机 API Key；所有 `supported_in_api=true` 的缓存模型都会与显式模型覆盖和 Claude Code 兼容别名合并。
5. 编译并在后台启动 CLIProxyAPI。
6. 使用生成的 API Key 请求 `/v1/models`，确认认证和目标实例均正常后返回。

脚本不会修改仓库根目录的 `config.yaml`，也不会把 `~/.trae` 登录凭据复制进仓库。`.trae-proxy/` 已加入 `.gitignore`。

默认后台启动使用 `nohup`，适合本机快速使用，但不会注册开机自启服务。脚本会在认证就绪后继续检查 2 秒再返回；不过某些远程 Agent、CI 或沙箱会在命令结束时统一回收子进程，这种行为无法由 `nohup` 或短暂稳定性检查规避。需要长期或非交互运行时，请使用前台模式配合进程管理器，或按下节使用 systemd 托管。

### 用户级 systemd 托管（Linux）

下面的流程使用用户级 systemd，无需把 TRAE 凭据复制到系统目录。首次安装前，先在仓库目录完成登录、配置生成和编译，但不启动后台进程：

```bash
./trae-quickstart.sh prepare
```

然后生成用户级 unit。示例会记录当前仓库和状态目录的绝对路径；如果使用自定义 `TRAE_PROXY_STATE_DIR`，执行整段命令时也要保留同一个环境变量。

```bash
repo_dir="$(pwd -P)"
state_dir="${TRAE_PROXY_STATE_DIR:-${repo_dir}/.trae-proxy}"
state_dir="$(cd -- "${state_dir}" && pwd -P)"
unit_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"

install -d -m 700 "${unit_dir}"
cat >"${unit_dir}/cliproxy-trae.service" <<EOF
[Unit]
Description=CLIProxyAPI TRAE proxy
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
WorkingDirectory=${repo_dir}
ExecStart="${state_dir}/cli-proxy-api" --config "${state_dir}/config.yaml"
Restart=on-failure
RestartSec=3
UMask=0077

[Install]
WantedBy=default.target
EOF
chmod 600 "${unit_dir}/cliproxy-trae.service"

systemctl --user daemon-reload
systemctl --user enable --now cliproxy-trae.service
```

查看服务、代理健康状态和日志：

```bash
systemctl --user status cliproxy-trae.service --no-pager
./trae-quickstart.sh status
journalctl --user -u cliproxy-trae.service -f
```

更新代码后，应先停止服务，再重新生成配置和二进制，最后启动服务。systemd 托管期间不要使用 quickstart 的 `restart`，因为该命令只管理脚本自己的 PID 文件。

```bash
systemctl --user stop cliproxy-trae.service
./trae-quickstart.sh prepare
systemctl --user start cliproxy-trae.service
```

如果服务器需要在用户未登录时也自动运行，可让管理员评估后执行 `loginctl enable-linger "$USER"`。如果 `systemctl --user` 提示无法连接 user bus，则需要先修复当前系统的用户级 systemd 会话，不能依赖 `nohup` 作为替代。

卸载服务：

```bash
unit_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
systemctl --user disable --now cliproxy-trae.service
rm -f -- "${unit_dir}/cliproxy-trae.service"
systemctl --user daemon-reload
systemctl --user reset-failed
```

### 模型自动同步与排查

本地 TRAE 账号启用后，代理启动时立即检查模型缓存（默认 `~/.trae/cli/models_cache.json`），之后每小时检查一次。TRAE CLI 更新缓存后，新增、移除的 API 模型及上下文等元数据会在后续检查中自动同步到运行中的代理，通常最多等待约 1 小时，无需重新部署。已有模型的限流和冷却状态会保留，缓存更新不会重启服务或主动打断请求。

同步使用已有配置：账号的 `models_cache` 属性优先于 `traecli.models-cache`，最后使用默认路径。显式配置了 `traecli.models` 且 `include-cache-models: false` 时只使用显式模型；Home 模式的目录由中心管理，不受本地缓存同步影响。缓存暂时缺失或 JSON 损坏时保留上一份有效目录；合法的 `models: []` 会清空动态模型，显式配置条目仍保留。

代理只读取本地缓存，不会定时启动 TRAE CLI 或主动向上游拉取目录。如果新模型尚未出现，按以下顺序检查：

1. 在 TRAE CLI 中执行 `traecli models --json` 更新缓存（使用 `traex` 安装的环境可替换命令名），确认目标模型已写入代理实际读取的缓存文件，且 `supported_in_api=true`。
2. 等待一个轮询周期，运行 `./trae-quickstart.sh models` 查看代理目录，同时检查显式模型、别名和排除规则。
3. 代理已返回新模型，但 Claude Code 仍显示旧菜单时，运行 `./trae-client-setup.sh claude` 预热客户端发现缓存，再开启一个新会话。运行中会话的菜单刷新时机由 Claude Code 控制；服务端同步不保证已打开的菜单立即变化。

目录容错只保证模型发现使用上一份有效快照。请求执行器仍独立读取 TRAE 缓存解析元数据；缓存损坏期间，目录保留并不保证所有模型的请求元数据也完整可用。

### Claude Code

`trae-client-setup.sh` 不会安装、升级或替换 Claude Code，安装方式由用户自行选择。脚本会从 `PATH` 和常见安装位置查找 `claude`；如果没有找到，可以通过 `TRAE_CLAUDE_BIN` 指定可执行文件路径。

配置脚本会自动使用本机已有的 `jq`、Node.js 或 Python 3 合并现有设置，不固定依赖其中某一个工具。脚本只在内容发生变化时备份 `~/.claude/settings.json`，默认保留最近 5 份备份，并写入：

- `ANTHROPIC_BASE_URL`：指向本地 TRAE 转发服务。
- `apiKeyHelper`：运行时从 `.trae-proxy/api-key` 读取代理凭据，不在 Claude Code 设置中复制 API Key。
- `ANTHROPIC_DEFAULT_*_MODEL`：Opus 默认使用 `openrouter-3o`，Fable、Sonnet、Haiku 分别默认使用 `gpt-5.6-sol`、`gpt-5.5`、`gpt-5.4`。脚本不写入 `ANTHROPIC_MODEL`，主模型改由设置顶层的 `model` 字段决定，因此在 Claude Code 里用 `/model` 切换主模型后能持久保存，下次启动不会被覆盖。
- 顶层 `model`：决定主模型。首次生成（设置里尚无该字段）时写入默认值 `openrouter-3o`；显式传入 `TRAE_CLAUDE_MODEL` 时写入该值。此外保留现有值，即通过 `/model` 选定的模型，重跑脚本不会覆盖。
- `modelOverrides`：把 Claude Code 已知的 Fable、Opus、Sonnet、Haiku 模型映射到上述代理模型，避免将代理模型判断为未知模型；已有的其他映射会保留。
- `CLAUDE_CODE_MAX_CONTEXT_TOKENS`：按实际主模型解析上下文窗口，优先级为显式 `TRAE_CLAUDE_MODEL`、设置中已有的有效 `model`、默认模型。支持 Claude 编码 ID、公开 ID、`modelOverrides` 和模型槽位；无法可靠匹配时不写入猜测值。此项解析需要 Python 3，未安装时跳过窗口配置，接入配置和模型缓存预热仍可通过 jq 或 Node.js 完成。脚本输出的主模型与保存值一致。
- `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY`：允许 Claude Code 从转发服务读取可用模型。

配置时脚本会使用本地 API Key 请求 Anthropic 格式的 `/v1/models`，校验后原子预热 `~/.claude/cache/gateway-models.json`，让第一次启动即可看到完整模型列表。缓存只包含模型 ID、显示名、代理地址和更新时间，不包含 API Key；后续 Claude Code 启动时仍会自动刷新。预热失败不会覆盖已有缓存，也不会阻止设置生成。

配置脚本会替换已有的 `apiKeyHelper`，并移除设置 `env` 中的 `ANTHROPIC_AUTH_TOKEN`、`ANTHROPIC_API_KEY` 和 `ANTHROPIC_MODEL`（旧版本曾写入 `ANTHROPIC_MODEL`，会覆盖 `/model` 的选择，现已清理）。如果当前终端还导出了这些变量，请先取消后再启动 Claude Code；原设置可以从备份文件恢复。

可以通过环境变量覆盖默认模型；显式传入 `TRAE_CLAUDE_MODEL` 时会同时写入顶层 `model` 字段：

```bash
TRAE_CLAUDE_MODEL=gpt-5.6-sol \
TRAE_CLAUDE_HAIKU_MODEL=seed-2.1-turbo \
./trae-client-setup.sh claude
```

配置前会使用本地 API Key 验证代理；只有明确需要在代理停止时预先生成设置，才使用：

```bash
./trae-client-setup.sh claude --allow-offline
```

可通过 `TRAE_CLAUDE_BACKUP_LIMIT` 修改备份保留数量；设为 `0` 时保留全部备份。

如需指定 JSON 处理工具，可将 `TRAE_CLAUDE_JSON_TOOL` 设为 `jq`、`node` 或 `python3`；默认值 `auto` 会自动选择。

### Codex

`trae-client-setup.sh` 不会安装、升级或替换 Codex CLI。脚本会从 `PATH` 和常见安装位置查找 `codex`；如果没有找到，可以通过 `TRAE_CODEX_BIN` 指定可执行文件路径。

配置脚本会保留现有的 Codex 设置，只更新用户级 `~/.codex/config.toml` 中的顶层 `model`、`model_provider`，并维护一个带明确起止标记的 `trae_cli_proxy` provider 配置：

- `base_url`：指向本地 TRAE 转发服务的 `/v1`。
- `wire_api = "responses"`：使用 Codex 原生 Responses 协议。
- provider 的 command-backed auth：运行时从 `.trae-proxy/api-key` 读取代理凭据，不把 API Key 写入 Codex 配置或登录缓存。
- 默认模型：`gpt-5.5`。

只有配置内容发生变化时才会备份原文件，默认保留最近 5 份 `config.toml.trae-backup.*`。如果现有配置已经在 managed block 之外定义了同名 `trae_cli_proxy` provider，脚本会停止并保留原配置，避免覆盖用户自定义 provider。

可以通过环境变量覆盖默认模型：

```bash
TRAE_CODEX_MODEL=openrouter-3o ./trae-client-setup.sh codex
```

配置前会使用本地 API Key 验证代理；只有明确需要在代理停止时预先生成设置，才使用：

```bash
./trae-client-setup.sh codex --allow-offline
```

可通过 `TRAE_CODEX_BACKUP_LIMIT` 修改备份保留数量；设为 `0` 时保留全部备份。Codex 配置目录默认是 `~/.codex`，设置 `CODEX_HOME` 时会改为 `$CODEX_HOME/config.toml`。

### 其他兼容客户端

默认地址是 `http://127.0.0.1:8317`。OpenAI 兼容客户端可这样配置：

```bash
export OPENAI_BASE_URL="http://127.0.0.1:8317/v1"
export OPENAI_API_KEY="$(./trae-quickstart.sh api-key)"
```

## 模式选择

默认配置是严格 `native`：请求直接访问 TRAE raw-chat API，使用 `max` 后端变体（`openrouter-3o__max`）并转发客户端工具定义，不会自动启动 `traecli exec`。响应中的 `X-TraeCLI-Mode` 与 `X-TraeCLI-Native-Fallback` 可用于确认实际执行路径。

Claude Code 的 auto 权限模式会固定请求分类器模型 `claude-opus-5`。quickstart 配置将这个兼容 ID 映射到 `openrouter-3o__max`，并让执行器从真实模型元数据生成 Trae 所需的 `config_name=openrouter-3o`。CLIProxyAPI 会在 Codex 专用模型目录中将这个兼容 ID 标记为隐藏，因此它不会出现在 Codex 的 `/model` 菜单中；Claude Code 分类器仍可直接调用该 ID。

使用 systemd 托管代理时，应按照“用户级 systemd 托管”小节停止服务、运行 `prepare` 重新编译，再启动真正持有端口的服务。仅修改代码后直接 restart 不会更新已经生成的二进制：

```bash
systemctl --user stop cliproxy-trae.service
./trae-quickstart.sh prepare
systemctl --user start cliproxy-trae.service
```

如果只想走官方 CLI 路径：

```bash
TRAE_PROXY_MODE=exec ./trae-quickstart.sh restart
```

`exec` 模式更稳但更慢，而且每个请求都会启动一个 CLI 子进程。默认使用只读 sandbox，并把工作目录限制在 `.trae-proxy/`。

## 自定义端口与状态目录

```bash
TRAE_PROXY_PORT=9000 ./trae-quickstart.sh restart
TRAE_PROXY_STATE_DIR=/data/cliproxy-trae ./trae-quickstart.sh start
```

生成配置后，未显式传入的 `TRAE_PROXY_HOST`、`TRAE_PROXY_PORT` 和 `TRAE_PROXY_MODE` 会沿用现有配置。使用自定义状态目录时，后续命令仍需传入相同的 `TRAE_PROXY_STATE_DIR`，脚本才能找到对应部署。

常用环境变量：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `TRAE_PROXY_HOST` | `127.0.0.1` | 监听地址 |
| `TRAE_PROXY_PORT` | `8317` | 监听端口 |
| `TRAE_PROXY_MODE` | `native` | `native` 或 `exec` |
| `TRAE_PROXY_STATE_DIR` | `.trae-proxy` | 配置、二进制、PID、日志目录 |
| `TRAE_PROXY_GOCACHE` | `<状态目录>/go-build-cache` | 隔离的 Go 编译缓存 |
| `TRAE_PROXY_GO_BIN` | 自动检测 | 指定 Go 可执行文件路径 |
| `TRAE_HOME` | `~/.trae` | TRAE 用户数据目录 |
| `TRAECLI_BIN` | 自动检测 | 指定 `traex`/`traecli` 路径 |
| `TRAECLI_LOGIN_MODE` | `default` | `default`、`sso`、`sso-device` 或 `git-code` |

## 多用户与服务器部署

TRAE 的 `auth.json` 是用户凭据，不能把某个人的文件提交到 Git、打进镜像或分发给其他人。正确流程是每个用户克隆仓库后运行同一条 quickstart 命令，并完成自己的登录。

如果要作为共享服务器对外提供服务，至少应：

- 将 `TRAE_PROXY_HOST` 改为受控内网地址，不要直接暴露到公网。
- 通过防火墙或反向代理限制访问来源，并妥善保管 `.trae-proxy/api-key`。
- 明确账号席位、并发与转发使用是否符合组织和 TRAE 服务规则。
- 使用进程管理器托管前台模式，例如 `./trae-quickstart.sh foreground`；不要依赖个人终端会话。

## 排障

先运行：

```bash
./trae-quickstart.sh doctor
```

常见问题：

- 找不到 CLI：确认当前机器能访问内网安装源；也可先手动安装，然后设置 `TRAECLI_BIN`。
- 登录成功但缺少 `~/.trae/cli/auth.json`：当前命令可能是旧版 Coco 或其他 TRAE CLI，需安装 TRAE CLI 2.0（TraeX）。
- native 请求触发风控、限流或服务错误：错误会直接返回，不会自动回退到 exec；查看响应头和 `.trae-proxy/server.log`。
- `exec` 报 sandbox/bubblewrap 错误：Linux 安装 `bubblewrap`，或联系运行环境管理员，不要为了省事开启完全绕过权限和 sandbox。
- 端口占用：换端口后重启，例如 `TRAE_PROXY_PORT=9000 ./trae-quickstart.sh restart`。
