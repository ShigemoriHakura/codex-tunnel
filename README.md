# Desk Tunnel

单实例、自托管的 **Go + Codex CLI API 网关**，附中文 Web 控制台。适合给受信任的内部客户端提供统一入口，不是匿名公共服务，也不是 OpenAI API 的完整替代。每个 API 密钥拥有独立会话，但共用管理员配置的上游账户、额度与并发池。

## Docker Compose 快速启动

需要 Docker Engine / Docker Desktop 和 Compose v2。首次在项目目录执行：

```sh
# 从示例创建 .env，并填入新的随机管理令牌（不要覆盖已有部署的 .env）
(umask 077; set -C; sed "s/^ADMIN_TOKEN=$/ADMIN_TOKEN=$(openssl rand -hex 32)/" .env.example > .env)
docker compose up --build -d
docker compose exec gateway codex -c 'cli_auth_credentials_store="file"' login --device-auth
docker compose exec gateway codex -c 'cli_auth_credentials_store="file"' login status
```

打开 `http://127.0.0.1:8080`，用 `.env` 中的 `ADMIN_TOKEN` 登录。到 **访问密钥 → 创建密钥**，立即复制一次性显示的 `dt_…`。SDK 的 `base_url` 使用 `http://127.0.0.1:8080/v1`，`api_key` 使用此客户端密钥，**不是**管理员令牌或上游凭据。更改 `.env` 后通过 `docker compose up -d` 重建容器配置。

也可用 API key 登录 CLI（从本机环境变量经 stdin 输入，不要把密钥写进命令参数）：

```sh
printf '%s' "$OPENAI_API_KEY" | docker compose exec -T gateway codex -c 'cli_auth_credentials_store="file"' login --with-api-key
```

**文本、配额和生图是不同的上游能力。** 固定版 CLI 的内置生图工具需要支持它的 Codex 后端登录、模型与账户；普通 API-key 登录能聊天，并不意味着可用该工具。配额查询也可能不受当前账户支持。网关不伪造图片或额度，不会静默切换到另一个计费账户。

## 面板与会话

| 页面 | 可执行操作 |
| --- | --- |
| 运行概览 | 查看请求数、错误数、Token、图片数和最近活动 |
| 访问密钥 | 创建、分页查看、撤销；数据库只保存随机密钥的 SHA-256 摘要 |
| 会话管理 | 查看归属、模型、最近活动；按页读取对话历史、查看该会话的请求或移除绑定 |
| 请求日志 | 按页查看模型、状态、耗时、计量；不复制提示词、回复或图片正文 |
| 模型与配额 | 重新从 CLI 拉取模型清单、读取账户限额原始字段 |
| CLI 维护 | 检查当前版本及 npm 官方稳定版、验证安装后切换、回退到内置版本 |
| 接口调试 | 用客户端密钥新建会话、发送文本或生成图片 |

**默认无状态**：每次创建临时 Codex 线程，可以传完整的文本消息序列。**显式续聊**：先创建网关会话，再带 `X-Session-ID`，只传本轮的新用户消息。不要重复发送整段历史。会话与密钥绑定，首次运行后固定模型；重启后通过 SQLite 中的绑定和 Codex 持久线程继续。并发写同一会话返回 `409`；并发槽满返回 `429`。生成中断可能已经改变上游会话，重试前可先读取历史，接口不保证幂等。

## 客户端 API

所有 `/v1/*` 请求都需要 `Authorization: Bearer <客户端密钥>`。以下假设已设置 `DT_KEY`：

```sh
BASE=http://127.0.0.1:8080
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
- 三个命名卷分别存 SQLite（WAL）、Codex 登录凭据与原始历史、托管 CLI 版本。备份前先停服务，三个卷一起备份并加密保护；不要对这些目录做静态文件发布。日志、原始历史及 CLI 生成图片没有自动过期，请制定磁盘容量与保留策略。`docker compose down` 保留卷，`down -v` **会删除数据和登录状态**。
- 只支持一个网关实例使用同一组卷（会话锁与并发池在进程内）。`MAX_CONCURRENT` 默认 4，可设 1–64。`PORT` 默认 8080。控制台登录令牌仅驻留页面内存，刷新需重新登录；轮换 `ADMIN_TOKEN` 不改变现有客户端密钥。
- 镜像默认固定 `@openai/codex@0.170.0`。面板安装只接受当次查询到的 npm 官方稳定版本，安装到独立目录且禁用 npm 生命周期脚本，验证版本、协议字段与握手后再切换。运行中的请求继续使用原版本。失败不切换，可用“恢复镜像内置版本”回退。CLI 新版可能迁移其内部存储，升级前应备份；二进制回退不等于数据回滚。
- `502` 优先检查 `docker compose logs gateway`、CLI 登录状态、所选模型及账户权限；`504` 表示请求上下文取消或超时。请求元数据只统计已进入推理执行的请求，不统计鉴权拒绝、校验错误或繁忙拒绝。

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

本机默认监听 `127.0.0.1:8080`。可指定 `CODEX_BIN` 的绝对路径。若不设置 `CLI_INSTALL_DIR`，面板可检查但不会修改本机 CLI；启用托管更新请将其设为专用可写的绝对目录，并安装 npm。

```sh
go test -race ./...
go vet ./...
# 真实 CLI 协议集成测试：独立临时目录 + 环回模拟上游，无需真实账号
TEST_CODEX_BIN="$(command -v codex)" go test -race -count=1 -timeout 2m ./...
```

集成测试覆盖重启后续聊、流式输出、用量差值、持久化历史、模型列表以及 CLI 调用生图工具并返回图片。GitHub Actions 另配置了 Linux 上的 Compose 构建、健康检查和 CLI 版本冒烟测试。
