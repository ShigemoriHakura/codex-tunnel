# Codex Tunnel

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

已有 Codex 登录的实例可以直接沿用。远端客户端使用网关的 `dt_` 密钥，文本、看图和生图请求均由服务器上的 Codex CLI app-server 执行。若已在 CLI 中使用 API Key 登录，聊天仍使用该登录。0.170.0 版 CLI 对普通 API Key 不提供内置生图工具，网关会在开始推理前返回 `image_tool_unavailable`。

此部署用于从自己的设备操作服务器上的 Codex。CLI 的调用方式不会改变账户的授权范围。通过 Sign in with ChatGPT 接入应用时，套餐授权仅限所连接的应用，不能用来为其他工具提供通用 API。[OpenAI 授权条款 §3.1](https://openai.com/policies/sign-in-with-chatgpt-terms/)

如需在新实例使用项目 API Key，可通过标准输入登录 CLI。

```sh
printf '%s' "$OPENAI_API_KEY" | docker compose exec -T gateway codex -c 'cli_auth_credentials_store="file"' login --with-api-key
```

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

### 多层反向代理下的流式响应

网关会在 SSE 响应中发送 `X-Accel-Buffering: no`。客户端与网关之间有多层反向代理时，在需要继续转发该响应头的 Nginx 或 OpenResty 的 `server` 或 `location` 中添加以下指令，检查配置后重新加载。下一层代理收到该响应头后，就能及时向客户端转发生图保活消息和完成事件。

```nginx
proxy_pass_header X-Accel-Buffering;
```

## 面板与会话

登录页和面板页眉的语言选择器支持简体中文与英语。控制台默认跟随系统配色，在页眉可切换浅色或深色。手机上通过顶部菜单切换页面。首次访问按浏览器语言偏好选择，不匹配时使用简体中文；手动选择保存在当前浏览器。切换不刷新页面，不翻译用户消息、模型 ID 或上游原始数据。新增语言时，在 `src/web/locales/` 添加同键字典并在 `src/web/i18n.mjs` 注册。

| 页面 | 可执行操作 |
| --- | --- |
| 运行概览 | 按时段、请求类型和模型切换五项指标，查看请求量与平均耗时、计量覆盖率、诊断队列和最近活动 |
| 访问密钥 | 创建、查看、撤销或删除密钥，选择已有会话或新建会话并绑定 |
| 会话管理 | 查看归属、模型、最近活动；按页读取对话历史、查看该会话的请求或移除绑定 |
| 请求日志 | 查看入站请求与出站 Codex CLI 调用，搜索、筛选、分页、导出本页或复制单条详情。在采集设置中控制范围、容量、正文预览与暂停 |
| 用量统计 | 选择最近 1 小时至 90 天或全部历史，以 5 分钟、小时或天为粒度查看 Token、请求数和缓存命中率趋势。可按模型和客户端密钥查看明细 |
| 错误中心 | 按来源、错误码和状态归并失败事件，查看首次及最近发生时间、请求 ID、路径、模型和未读数，可标记已读或清空 |
| 模型与配额 | 查看网关 CLI 登录身份、套餐、限额窗口和重置卡明细，手动使用重置卡，刷新模型清单 |
| CLI 维护 | 检查当前版本及 npm 官方稳定版、验证安装后切换、回退到内置版本 |
| 接口调试 | 测量网关进程内存，或用客户端密钥新建会话、发送文本和生成图片 |
| 站点设置 | 保存公开域名和 URL 子目录，预览控制台地址与客户端 Base URL |

在“站点设置”填写域名（例如 `https://gateway.example.com`）和访问子目录（例如 `/tools/codex`），保存后控制台入口为 `https://gateway.example.com/tools/codex/`，客户端 Base URL 为 `https://gateway.example.com/tools/codex/v1`。空域名使用当前来源，空目录使用根路径。配置写入 SQLite 并即时生效。当前主机的根入口继续可用，方便从错误配置恢复。域名需解析到网关或反向代理，代理应保留完整请求路径。HTTPS 使用该域名对应的证书。跨域名首次访问需单独登录。

请求日志默认仅采集推理元数据，保留最近 2,000 条，可设为 1 至 10,000 条。切换到全部业务接口可排查模型列表、账户、会话等调用。出站记录包含网关交给 Codex app-server 的调用，同次请求的两个方向共用请求 ID。正文预览默认关闭。开启后会保存推理文本，每份预览最多约 8 KiB，对话中可能含有敏感信息。鉴权头、管理接口正文及图片字节均省略。

用量统计以每次执行的推理为单位。暂停采集、清空诊断日志或调整保留条数后，累计用量仍可查询。旧记录原有的请求数及用量会计入趋势，CLI 未报告的字段显示“未报告”。首个文本 Token 耗时以网关实际收到文本增量的时间计算。缓存命中率以有缓存计量的输入 Tokens 为分母。生图按成功和失败计数，并显示 CLI 报告的用量。美元费用按已知模型的标准短上下文文本单价折算，面板列出可估价的请求数。订阅账单以提供方为准。[OpenAI API 价格表](https://developers.openai.com/api/docs/pricing)

“模型与配额”读取网关所用 Codex 配置目录中的登录状态。Docker 部署请在上方的 `gateway` 容器内核对所用的 CLI 登录，再点击“刷新账户和配额”。查看和刷新通过只读 RPC 查询。点击某张重置卡右侧的“使用”，在弹窗中核对账户和卡片，再点“确认使用”。上游只报告数量时，列表显示一行汇总，弹窗会说明由上游分配一张可用卡。操作后页面会显示兑换结果，并刷新额度和卡数。

概览、用量统计和请求日志显示输出 TPS（token/s）。统计使用已记录速度的成功聊天，每次输出至少 100 个 token。单次速度以本轮输出 token 数除以首个文本增量至请求处理结束的秒数。缺少首个文本增量或该时间段短于 20 毫秒时，使用请求总耗时。每个请求贡献一个样本。平均值取各样本的算术平均，峰值取最大值。按时间、模型或密钥筛选时，样本数随之更新。没有样本时，平均值和峰值返回 `null`。

“接口调试 → 测试内存占用”通过管理员接口 `GET /admin/memory` 即时采样 Go 堆分配量。Linux 还会显示网关进程当前和启动以来的峰值常驻内存（MiB）。查看包含 CLI 子进程的整个容器用量可运行 `docker compose stats --no-stream gateway`。内存采样无需客户端密钥，不启动推理。

在 Chat Completions 接口中，未绑定的密钥默认使用临时线程，可发送完整消息序列。要显式续聊，先创建会话，随后每次带上 `X-Session-ID`，只发送本轮用户消息。

在“访问密钥 → 会话绑定”选择已有会话，或新建并绑定。该密钥的聊天请求会自动进入固定会话，可省略 `X-Session-ID`。指定其他会话会返回 `403`。客户端若每次重发完整对话，网关会在首次请求中传入这些上下文，之后只追加最后一条用户消息。绑定已有会话会授权该密钥读取并继续该会话，选择“未绑定”可解除授权。撤销或删除密钥即解除固定绑定并终止后续鉴权。删除还会清除凭据摘要并从列表移除密钥，原会话、用量记录和归属仍可在管理员面板查看。

网关启动时会与一个常驻 `codex app-server` 完成 stdio 握手。CLI 线程聊天请求复用这个进程，各线程的流式消息和用量独立分发。客户端断开时，网关向对应轮次发送 `turn/interrupt`，结束后通过 `thread/unsubscribe` 释放订阅。CLI 意外退出会结束当时正在处理的请求，下一个请求会启动新进程。网关不会自动重发已经提交的输入。更换 CLI 版本或更新登录文件、配置文件后，新请求使用新进程，旧进程等已有请求结束后退出。

生图使用启用了内置图像工具的独立 app-server 进程。模型列表、账户和配额查询复用常驻连接。网关通过 CLI 的 RPC 调用提交推理、接收事件及用量。

会话首次运行后固定模型，重启后可继续。同一会话的生成请求按入队顺序执行，不同会话和无状态请求可以并行。等待同一会话时不占用 CLI 执行槽，断开的请求会退出队列。`MAX_CONCURRENT=4` 时最多同时执行 4 个 CLI 请求，最多接纳 12 个生成请求，每个会话最多接纳 8 个。队列已满返回 `429` 并附带 `Retry-After`。删除正在使用的会话仍返回 `409`。生成中断可能已经改变上游会话，重试前可先读取历史。

聊天与生图的流式请求在排队和等待上游时每 3 秒发送 SSE 注释。经短超时中转发送非流式请求时，可设置 `JSON_KEEPALIVE=true`。网关等待超过 3 秒后会发送并刷新 JSON 前导空白，最后返回一个完整的 JSON 对象。此选项默认关闭。首个空白发出后 HTTP 状态固定为 200，客户端须检查 JSON 的 `error` 字段，网关日志仍记录实际错误状态。保活只能延长空闲读取超时，客户端设置的总请求时限仍然生效。

## 客户端 API

所有 `/v1/*` 请求都需要 `Authorization: Bearer <客户端密钥>`。以下假设已设置 `DT_KEY`：

```sh
BASE=http://127.0.0.1:56439
curl -fsS -H "Authorization: Bearer $DT_KEY" "$BASE/v1/models"

# 1. 建立会话；复制返回的 id 到 SID
curl -fsS -H "Authorization: Bearer $DT_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"产品讨论"}' "$BASE/v1/sessions"

# 2. 在该会话发送消息；未绑定密钥省略该请求头时进入无状态模式
curl -N -H "Authorization: Bearer $DT_KEY" -H 'Content-Type: application/json' \
  -H "X-Session-ID: $SID" \
  -d '{"messages":[{"role":"user","content":"给项目起一个简短的名字"}],"stream":true,"stream_options":{"include_usage":true}}' \
  "$BASE/v1/chat/completions"

# 3. 生图（返回 data[0].b64_json；不会公开托管图片）
curl -fsS -H "Authorization: Bearer $DT_KEY" -H 'Content-Type: application/json' \
  -d '{"prompt":"画一只墨绿色的纸鹤","n":1,"response_format":"b64_json","output_format":"png"}' \
  "$BASE/v1/images/generations" > image-response.json

curl -fsS -H "Authorization: Bearer $DT_KEY" "$BASE/v1/usage"
```

| 方法及路径 | 含义 |
| --- | --- |
| `GET /healthz` | 网关存活探针，不代表上游已登录 |
| `GET /v1/models` | 从 CLI `model/list` 分页拉取，不硬编码模型 |
| `POST /v1/chat/completions` | 文本、图片输入与 SSE；支持 `model`、`messages`、`stream`、`stream_options.include_usage` |
| `POST /v1/responses` | 创建 Responses 对象。支持 JSON、SSE、文本、内联图片、客户端工具调用和连续对话。 |
| `POST /v1/images/generations` | 通过原生 CLI 工具生成一张图片。省略 `model` 或提供 Codex 对话模型时，由 CLI 选择图像引擎。在 0.170.0 可精确指定 `gpt-image-2`。`gpt-image-2.5-sunburst`、固定尺寸或非自动质量会返回 400。支持 PNG、JPEG、WebP 和 JPEG/WebP 的 `output_compression`。`stream:true` 发送保活和最终的 `image_generation.completed`，`partial_images` 只能为 0。 |
| `POST /v1/sessions` | `name`、`model` 都可省略，发送 `{}` 即可 |
| `GET /v1/sessions` | 本密钥会话，`?offset=100` 翻页，每页最多 100 条 |
| `GET /v1/sessions/{id}/history` | 从新到旧读取对话轮次，每页 10 轮，用 `next_cursor` 作为下一页的 `cursor` |
| `DELETE /v1/sessions/{id}` | 解除绑定；不承诺物理擦除 CLI 历史或备份 |
| `GET /v1/usage` | 当前密钥已执行的推理请求汇总（含已报告用量的失败请求），并返回聊天速度统计 `avg_tps`、`peak_tps`、`tps_samples` |

### Responses API

使用模型列表中的模型 ID。OpenAI SDK 的 `base_url` 填到 `/v1`，Codex CLI 的自定义提供方使用 `wire_api = "responses"` 和 `supports_websockets = false`。

```sh
curl -N "$BASE/v1/responses" -H "Authorization: Bearer $DT_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.4","input":"用一句话说明这个项目的用途","stream":true,"store":false}'
```

省略 `stream` 返回一个包含 `output` 与 `usage` 的 JSON 对象。流式调用返回具名 SSE 事件，如 `response.output_text.delta`、`response.output_item.done` 和 `response.completed`。输入支持字符串、带角色的消息数组、内联 Base64 图片，以及与 `call_id` 匹配的 `function_call_output`。支持 `instructions`、`reasoning.effort` 和 `text.format` 的文本或 JSON Schema 输出。客户端可以声明非严格模式函数，每次回传一个调用结果。网关在临时 CLI 线程中重建该分支的历史，提交函数结果后让模型继续回答。未实现的参数、工具类型和强制工具选择会在请求校验时返回 400。

使用同一密钥传入 `previous_response_id` 可以续聊或从先前的一轮分支。网关默认在本机 SQLite 保存该轮输入及输出以供续聊，独立响应 ID 的有效期为 30 天。`store:false` 关闭按响应 ID 续聊。显式会话和密钥绑定会话会保留对话，后续请求可直接发送新输入。同一会话可以混用 Responses 与 Chat Completions，沿用原有模型、并发队列、用量统计和保活设置。后台任务、托管文件和 WebSocket 传输暂未提供。

### Chat Completions

消息 `content` 支持文本字符串或内容数组，最后一条消息的角色须为 `user`。网关接受客户端回传的 `reasoning_content` 字符串或 `null`，只把可见消息内容送入模型。仅含推理文本的历史助手消息会被跳过。

```json
{"messages":[{"role":"user","content":[{"type":"text","text":"图里有什么？"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGPoKDcGAAK9ATNQgxqrAAAAAElFTkSuQmCC"}}]}]}
```

用户图片通过 Base64 data URL 传入，支持 PNG、JPEG、WebP、GIF。使用图片链接的客户端需先读取图片并转换为 data URL。可选的 `image_url.detail` 支持 `auto`、`low`、`high` 和 `original`。图片识别需使用支持视觉输入的上游模型。Chat Completions 和 Responses 的请求上限为 24 MiB，按 Base64 编码后的整个 JSON 请求体计算；其他 JSON 接口上限为 1 MiB。超出上限返回 `413`。无状态的多条消息会作为带角色的上下文交给 CLI。Chat Completions 暂不接受客户端工具调用和 `temperature`。图像接口暂不支持批量生图和图片编辑。未知请求字段返回 `400`。推理超时 10 分钟。上游无计量时返回 `usage: null`，不将其伪装成零消耗。网关累计值是观测值，不含其他应用的调用，不是账单，也不能用来推算订阅余额。

## 部署、更新与安全

- Compose 默认仅发布到宿主机回环地址。远程访问请自行部署 HTTPS、网络访问控制、请求限速和预算限制；不要直接把管理面板暴露给匿名互联网。反向代理需关闭 SSE 缓冲，设置足够的响应超时。
- 容器采用非 root 用户、只读根文件系统、不授予额外 capabilities，不挂载宿主机仓库、个人 `~/.codex` 或 Docker socket。**使用专用且无自定义 MCP、插件、技能的 CLI 配置目录**。Go 通过 stdio 与 Codex app-server 通信，不拼接 shell 命令；线程使用只读沙箱、禁用审批和本地工具环境。管理员令牌不传入 CLI。
- 三个命名卷分别存 SQLite（WAL）、Codex 登录凭据与原始历史、托管 CLI 版本。备份前先停服务，三个卷一起备份并加密保护；不要对这些目录做静态文件发布。推理用量、原始历史及 CLI 生成图片没有自动过期，请制定磁盘容量与保留策略。`docker compose down` 保留卷，`down -v` **会删除数据和登录状态**。
- 只支持一个网关实例使用同一组卷（会话锁与并发池在进程内）。`MAX_CONCURRENT` 默认 4，可设 1–64。`PORT` 默认 56439。控制台使用有效期为 7 天的 HttpOnly 会话 Cookie。刷新可返回当前栏目，同一浏览器的新标签页和网关重启后也可继续使用。退出会撤销当前浏览器的会话，轮换 `ADMIN_TOKEN` 会使旧会话失效。HTTPS 下 Cookie 附带 Secure 属性。客户端密钥单独管理。
- 镜像默认固定 `@openai/codex@0.170.0`。面板安装只接受当次查询到的 npm 官方稳定版本，安装到独立目录且禁用 npm 生命周期脚本，验证版本、协议字段与握手后再切换。运行中的请求继续使用原版本。失败不切换，可用“恢复镜像内置版本”回退。CLI 新版可能迁移其内部存储，升级前应备份；二进制回退不等于数据回滚。
- `502` 优先检查 `docker compose logs gateway`、CLI 登录状态、所选模型及账户权限；`504` 表示请求上下文取消或超时。用量只统计已进入执行的推理请求。鉴权、校验或繁忙拒绝可在错误中心及相应采集范围的请求日志查看。

## 本地开发与验证

概览布局使用本地编译的 Tailwind 工具类。Go 直接嵌入生成的 CSS，浏览器无需访问 CDN。修改界面中的工具类后，在仓库根目录执行以下命令生成样式。

```sh
npx --yes tailwindcss@3.4.17 -i src/tailwind.css --content 'src/web/**/*.{html,js,mjs}' -o src/web/utilities.css --minify
```

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
