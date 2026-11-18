# Desk Tunnel

单实例、自托管的 **Go + Codex CLI API 网关**，附中英文 Web 控制台。适合给受信任的内部客户端提供统一入口，不是匿名公共服务，也不是 OpenAI API 的完整替代。每个 API 密钥拥有独立会话，但共用管理员配置的上游账户、额度与并发池。

## Docker Compose 快速启动

需要 Docker Engine / Docker Desktop 和 Compose v2。首次在项目目录执行：

```sh
# 从示例创建 .env，并填入新的随机管理令牌（不要覆盖已有部署的 .env）
(umask 077; set -C; sed "s/^ADMIN_TOKEN=$/ADMIN_TOKEN=$(openssl rand -hex 32)/" .env.example > .env)
docker compose up --build -d
docker compose exec gateway codex -c 'cli_auth_credentials_store="file"' login --device-auth
docker compose exec gateway codex -c 'cli_auth_credentials_store="file"' login status
```

打开 `http://127.0.0.1:56439`，用 `.env` 中的 `ADMIN_TOKEN` 登录。到 **访问密钥 → 创建密钥**，立即复制一次性显示的 `dt_…`。SDK 的 `base_url` 使用 `http://127.0.0.1:56439/v1`，`api_key` 使用此客户端密钥，**不是**管理员令牌或上游凭据。更改 `.env` 后通过 `docker compose up -d` 重建容器配置。

也可用 API key 登录 CLI（从本机环境变量经 stdin 输入，不要把密钥写进命令参数）：

```sh
printf '%s' "$OPENAI_API_KEY" | docker compose exec -T gateway codex -c 'cli_auth_credentials_store="file"' login --with-api-key
```

**文本、配额和生图是不同的上游能力。** 固定版 CLI 的内置生图工具需要支持它的 Codex 后端登录、模型与账户；普通 API-key 登录能聊天，并不意味着可用该工具。配额查询也可能不受当前账户支持。网关不伪造图片或额度，不会静默切换到另一个计费账户。

## 在高位端口对外提供 HTTP

在 `.env` 使用 `BIND_IP=0.0.0.0` 和 `PORT=56439`，仅使用基础 `compose.yaml`（不要启用 `compose.https.yaml`），然后运行 `docker compose up -d`。访问 `http://服务器IP:56439/`，API 前缀为 `/v1`。本项目只发布这一个 TCP 端口。**HTTP 不加密管理令牌、客户端密钥或对话内容**；建议通过 VPN、SSH 隧道或限制来源 IP 访问。不配置 `BIND_IP` 时仍仅绑定回环地址。

## 在高位端口对外提供 HTTPS（可选）

默认仅在回环地址提供 HTTP。需要外部访问时，使用支持 `!override` 的 Compose 版本，将证书链和私钥分别放入 `tls/server.crt`、`tls/server.key`（私钥不入库，须允许容器内 UID/GID 1000 读取）。在服务器 `.env` 中设置：

```dotenv
COMPOSE_FILE=compose.yaml:compose.https.yaml
PORT=56439
TLS_HEALTH_SERVER_NAME=替换为证书SAN包含的公网IP或域名
```

执行 `docker compose config -q && docker compose up --build -d --wait`。仅允许所需来源访问此 TCP 端口。公网模式只监听一个 HTTPS 端口，无 HTTP 跳转；容器内外端口相同，范围 49152–65535。客户端使用 `https://主机:端口/` 和 `https://主机:端口/v1`。自签证书应通过受信任渠道核对 SHA-256 指纹并显式信任，不要关闭证书校验；换证后 `docker compose up -d --force-recreate`。

## 面板与会话

登录页和面板页眉的语言选择器支持简体中文与英语。控制台默认跟随系统配色，在页眉可切换浅色或深色。手机上通过顶部菜单切换页面。首次访问按浏览器语言偏好选择，不匹配时使用简体中文；手动选择保存在当前浏览器。切换不刷新页面，不翻译用户消息、模型 ID 或上游原始数据。新增语言时，在 `src/web/locales/` 添加同键字典并在 `src/web/i18n.mjs` 注册。

| 页面 | 可执行操作 |
| --- | --- |
| 运行概览 | 查看请求与 Token 计数、最近 24 次请求的耗时、最近活动和客户端接入地址 |
| 访问密钥 | 创建、分页查看、撤销；数据库只保存随机密钥的 SHA-256 摘要 |
| 会话管理 | 查看归属、模型、最近活动；按页读取对话历史、查看该会话的请求或移除绑定 |
| 请求日志 | 查看入站请求与出站 Codex CLI 调用，搜索、筛选、分页、导出本页或复制单条详情。在采集设置中控制范围、容量、正文预览与暂停 |
| 用量统计 | 选择最近 1 小时至 90 天或全部历史，以 5 分钟、小时或天为粒度查看 Token、请求数和缓存命中率趋势。可按模型和客户端密钥查看明细 |
| 错误中心 | 按来源、错误码和状态归并失败事件，查看首次及最近发生时间、请求 ID、路径、模型和未读数，可标记已读或清空 |
| 模型与配额 | 查看网关 CLI 登录身份、套餐、限额窗口和重置卡数量及明细，刷新模型清单 |
| CLI 维护 | 检查当前版本及 npm 官方稳定版、验证安装后切换、回退到内置版本 |
| 接口调试 | 测量网关进程内存，或用客户端密钥新建会话、发送文本和生成图片 |

请求日志默认仅采集推理元数据，保留最近 2,000 条，可设为 1 至 10,000 条。切换到全部业务接口可排查模型列表、账户、会话等调用。出站记录代表网关交给 CLI 的调用，同次请求的两个方向共用请求 ID。正文预览默认关闭。开启后会保存推理文本，每份预览最多约 8 KiB，对话中可能含有敏感信息。鉴权头、管理接口正文及图片字节均省略。

用量统计以每次执行的推理为单位。暂停采集、清空诊断日志或调整保留条数后，累计用量仍可查询。旧记录原有的请求数及用量会计入趋势，CLI 未报告的字段显示“未报告”。首个文本 Token 耗时以网关实际收到文本增量的时间计算，速度仅统计输出至少 100 Tokens 的轮次。缓存命中率以有缓存计量的输入 Tokens 为分母。生图按成功和失败计数，并显示 CLI 报告的整轮用量。美元费用按已知模型的标准短上下文文本单价折算，面板列出可估价的请求数。订阅账单以提供方为准。[OpenAI API 价格表](https://developers.openai.com/api/docs/pricing)

“模型与配额”读取网关所用 Codex 配置目录中的登录状态。Docker 部署请在上方的 `gateway` 容器内完成设备登录，再点击“刷新账户和配额”。限额和重置卡通过只读 RPC 查询。上游可能只提供可用数量，或完全省略重置卡信息；面板会区分“可用 0 张”和“上游未报告”。

“接口调试 → 测试内存占用”通过管理员接口 `GET /admin/memory` 即时采样 Go 堆分配量。Linux 还会显示网关进程当前和启动以来的峰值常驻内存（MiB）。查看包含 CLI 子进程的整个容器用量可运行 `docker compose stats --no-stream gateway`。内存采样无需客户端密钥，不启动推理。

**默认无状态**：每次创建临时 Codex 线程，可以传完整的文本消息序列。**显式续聊**：先创建网关会话，再带 `X-Session-ID`，只传本轮的新用户消息。不要重复发送整段历史。会话与密钥绑定，首次运行后固定模型；重启后通过 SQLite 中的绑定和 Codex 持久线程继续。并发写同一会话返回 `409`；并发槽满返回 `429`。生成中断可能已经改变上游会话，重试前可先读取历史，接口不保证幂等。

## 客户端 API

所有 `/v1/*` 请求都需要 `Authorization: Bearer <客户端密钥>`。以下假设已设置 `DT_KEY`：

```sh
BASE=http://127.0.0.1:56439
curl -fsS -H "Authorization: Bearer $DT_KEY" "$BASE/v1/models"

# 1. 建立会话；复制返回的 id 到 SID
curl -fsS -H "Authorization: Bearer $DT_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"产品讨论"}' "$BASE/v1/sessions"

# 2. 在该会话发送消息；删除 X-Session-ID 行即可改用无状态模式
curl -N -H "Authorization: Bearer $DT_KEY" -H 'Content-Type: application/json' \
  -H "X-Session-ID: $SID" \
  -d '{"messages":[{"role":"user","content":"给项目起一个简短的名字"}],"stream":true,"stream_options":{"include_usage":true}}' \
  "$BASE/v1/chat/completions"

# 3. 生图（返回 data[0].b64_json；不会公开托管图片）
curl -fsS -H "Authorization: Bearer $DT_KEY" -H 'Content-Type: application/json' \
  -d '{"prompt":"画一只墨绿色的纸鹤","n":1,"response_format":"b64_json"}' \
  "$BASE/v1/images/generations" > image-response.json

curl -fsS -H "Authorization: Bearer $DT_KEY" "$BASE/v1/usage"
```

| 方法及路径 | 含义 |
| --- | --- |
| `GET /healthz` | 网关存活探针，不代表上游已登录 |
| `GET /v1/models` | 从 CLI `model/list` 分页拉取，不硬编码模型 |
| `POST /v1/chat/completions` | 文本与 SSE；支持 `model`、`messages`、`stream`、`stream_options.include_usage` |
| `POST /v1/images/generations` | 单图 `b64_json`；可选 `model` 指调度生图工具的 **Codex 模型**，不是图片引擎 ID |
| `POST /v1/sessions` | `name`、`model` 都可省略，发送 `{}` 即可 |
| `GET /v1/sessions` | 本密钥会话，`?offset=100` 翻页，每页最多 100 条 |
| `GET /v1/sessions/{id}/history` | 从新到旧读取对话轮次，每页 10 轮，用 `next_cursor` 作为下一页的 `cursor` |
| `DELETE /v1/sessions/{id}` | 解除绑定；不承诺物理擦除 CLI 历史或备份 |
| `GET /v1/usage` | 当前密钥已执行的推理请求汇总（含已报告用量的失败请求） |

兼容范围：消息 `content` 为字符串，最后一条为 `user`。无状态多条消息会作为带角色的对话上下文交给 CLI，非逐字段透传。暂不支持 Responses API、客户端工具调用、视觉输入、`temperature`、批量生图或图片编辑。未知请求字段返回 `400`。单次请求体上限 1 MiB，推理超时 10 分钟。上游无计量时返回 `usage: null`，不将其伪装成零消耗。网关累计值是观测值，不含其他应用的调用，不是账单，也不能用来推算订阅余额。

## 部署、更新与安全

- Compose 默认仅发布到宿主机回环地址。远程访问请自行部署 HTTPS、网络访问控制、请求限速和预算限制；不要直接把管理面板暴露给匿名互联网。反向代理需关闭 SSE 缓冲，设置足够的响应超时。
- 容器采用非 root 用户、只读根文件系统、不授予额外 capabilities，不挂载宿主机仓库、个人 `~/.codex` 或 Docker socket。**使用专用且无自定义 MCP、插件、技能的 CLI 配置目录**。每个推理进程由 Go 通过 stdio 调用，不拼接 shell 命令；线程使用只读沙箱、禁用审批和本地工具环境。管理员令牌不传入 CLI。
- 三个命名卷分别存 SQLite（WAL）、Codex 登录凭据与原始历史、托管 CLI 版本。备份前先停服务，三个卷一起备份并加密保护；不要对这些目录做静态文件发布。推理用量、原始历史及 CLI 生成图片没有自动过期，请制定磁盘容量与保留策略。`docker compose down` 保留卷，`down -v` **会删除数据和登录状态**。
- 只支持一个网关实例使用同一组卷（会话锁与并发池在进程内）。`MAX_CONCURRENT` 默认 4，可设 1–64。`PORT` 默认 56439。控制台使用有效期为 7 天的 HttpOnly 会话 Cookie。刷新可返回当前栏目，同一浏览器的新标签页和网关重启后也可继续使用。退出会撤销当前浏览器的会话，轮换 `ADMIN_TOKEN` 会使旧会话失效。HTTPS 下 Cookie 附带 Secure 属性。客户端密钥单独管理。
- 镜像默认固定 `@openai/codex@0.170.0`。面板安装只接受当次查询到的 npm 官方稳定版本，安装到独立目录且禁用 npm 生命周期脚本，验证版本、协议字段与握手后再切换。运行中的请求继续使用原版本。失败不切换，可用“恢复镜像内置版本”回退。CLI 新版可能迁移其内部存储，升级前应备份；二进制回退不等于数据回滚。
- `502` 优先检查 `docker compose logs gateway`、CLI 登录状态、所选模型及账户权限；`504` 表示请求上下文取消或超时。用量只统计已进入执行的推理请求。鉴权、校验或繁忙拒绝可在错误中心及相应采集范围的请求日志查看。

## 本地开发与验证

Go 服务与测试位于 `src/`，嵌入式管理面板位于 `src/web/`。以下命令均从仓库根目录执行。

需要 Go 1.24.3+、C 编译器（SQLite 使用 CGO）、已安装的官方 Codex CLI。支持 macOS / Linux。

```sh
export ADMIN_TOKEN="$(openssl rand -hex 32)"
export DATA_DIR="$PWD/data" CODEX_HOME="$PWD/data/codex"
mkdir -p "$CODEX_HOME"
codex -c 'cli_auth_credentials_store="file"' login --device-auth
go run ./src
```

本机默认监听 `127.0.0.1:56439`。可指定 `CODEX_BIN` 的绝对路径。若不设置 `CLI_INSTALL_DIR`，面板可检查但不会修改本机 CLI；启用托管更新请将其设为专用可写的绝对目录，并安装 npm。

```sh
go test -race ./...
go vet ./...
# 真实 CLI 协议集成测试：独立临时目录 + 环回模拟上游，无需真实账号
TEST_CODEX_BIN="$(command -v codex)" go test -race -count=1 -timeout 2m ./...
```

集成测试覆盖重启后续聊、流式输出、用量差值、持久化历史、模型列表以及 CLI 调用生图工具并返回图片。推送代码时，GitHub Actions 在宿主机运行 Go 和 Node 检查。需要容器冒烟测试时，可手动运行 Gateway checks 并勾选 Compose 选项。
