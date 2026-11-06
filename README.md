# sub2api++

[sub2api-enhance](https://github.com/MADAO-NW/sub2api-enhance) 是 Sub2API 的独立增强服务，提供两个功能：

- **第三方提示词审计**：不保存请求原文，按“稳定指令块 + 当前回合用户提示词”内容寻址审核；支持异步审核、同步阻断、违规提醒、累计停用、指令块熔断和全库复用。
- **OpenAI 日/周额度跟随**：观察分组内账号的真实重置窗口，一致确认后重置参加用户的用量；保留原版自然日、周一规则，并支持周一旧周用量结转。

无需修改或重新编译 Sub2API。增强服务独立发布、独立运行，前端和数据库迁移包含在同一个二进制中。自有数据放在原数据库的 `sub2api_enhance` schema，不改原版表结构；账号状态和日/周用量归零通过原版管理 API 完成。**周一结转例外需要更新原版 OpenAI 配额表的两列用量/窗口字段，并清理相关 Redis 缓存**，并非所有功能都对原版数据只读。

[下载稳定版](https://github.com/MADAO-NW/sub2api-enhance/releases/latest) · [环境配置示例](deploy/.env.example) · [Nginx 示例](deploy/nginx.conf.example)

## 部署前准备

推荐在 Sub2API 所在的 Linux 主机上使用脚本部署。服务器不需要 Go、Node.js、pnpm 或项目源码。

| 项目 | 要求 |
| --- | --- |
| 操作系统 | Linux amd64 / arm64，使用 systemd |
| 系统工具 | bash、curl、tar、sha256sum、jq、flock、systemctl、install、getent、useradd、sort；菜单脚本还使用 awk |
| 原服务 | 已运行的 Sub2API、PostgreSQL 15+、Redis 7+；额度结转和缓存修复还需访问原版实际配额键所在逻辑库 |
| 访问入口 | 原版后台和增强页面使用同一个 HTTPS 域名，由 Nginx 等反向代理分流 |
| 兼容性 | 当前适配基线为 Sub2API 0.2.1；其他版本需核对管理员 API、身份及配额字段 |

数据库连接必须指向 **Sub2API 实际使用的同一个数据库**。建议使用独立数据库账号，权限按启用功能准备：

- 自有数据：拥有 `sub2api_enhance` schema 及其建表、迁移、读写权限。可由数据库管理员预建并指定所有者；若由服务自动创建 schema，连接角色还需要目标数据库的 `CREATE` 权限。
- 提示词审计：对 `public.api_keys`、`users`、`groups`、`user_allowed_groups` 授予必要 `SELECT`。
- 额度跟随：另外读取 `accounts`、`account_groups`、`user_platform_quotas`、`audit_logs`。
- 周一结转：另外授予 `public.user_platform_quotas` 的 `weekly_usage_usd`、`weekly_window_start` 两列 `UPDATE`。无需授予原用户表 `UPDATE` 或原版表结构修改权限。

安装脚本不安装数据库、Redis、Nginx，也不创建原版数据库账号或授予权限。

## 1. 安装增强服务

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/sub2api-enhance/main/deploy/install.sh -o /tmp/sub2api-enhance-install.sh
sudo bash /tmp/sub2api-enhance-install.sh install
```

脚本会下载最新稳定版、校验 SHA256 和包内版本，创建系统用户、安装程序及 systemd 单元，并启用开机启动。首次安装不会立即启动服务，先填写环境配置。

| 路径 | 用途 |
| --- | --- |
| `/opt/sub2api-enhance/sub2api-enhance` | 服务程序，包含前端和迁移 |
| `/etc/sub2api-enhance/sub2api-enhance.env` | 环境配置 |
| `/opt/sub2api-enhance/configure-sub2api-menus.sh` | 原版菜单及图标配置脚本 |
| `/etc/systemd/system/sub2api-enhance.service` | systemd 服务 |

## 2. 填写环境配置并启动

先在 **Sub2API → 系统设置 → 安全 → 管理员 API Key** 中取得管理员密钥，填入下方 `SUB2API_ADMIN_API_KEY`。这不是普通用户在“API 密钥”页创建的模型调用密钥，也不是审核模型供应商的 Key。已有其他服务使用该管理员密钥时，不要为了安装增强服务随意重新生成它。

```bash
sudoedit /etc/sub2api-enhance/sub2api-enhance.env
```

优先核对以下字段，完整说明见 [环境配置示例](deploy/.env.example)：

| 配置 | 填写方式 |
| --- | --- |
| `ENHANCE_LISTEN` | 增强监听地址，默认 `127.0.0.1:18081` |
| `ENHANCE_DATABASE_URL` | 原版数据库连接，使用为增强服务准备的账号 |
| `SUB2API_INTERNAL_URL` | 直达原版的内部地址，例如 `http://127.0.0.1:18080`；不要填写经过增强代理的公网地址 |
| `ENHANCE_PUBLIC_ORIGIN` | 原版后台的 HTTPS Origin，例如 `https://gateway.example.com`，不带路径 |
| `SUB2API_ADMIN_API_KEY` | 原版管理员 API Key，用于菜单配置、账号动作和额度操作 |
| `ENHANCE_ENCRYPTION_KEY` | 独立的 32 字节随机密钥，经 Base64 编码，用于保存审核节点凭据 |
| `ENHANCE_TRUSTED_PROXIES` | 实际反向代理的地址段；同机 Nginx 可使用 `127.0.0.1/32,::1/128` |
| `ENHANCE_DB_CONNECTIONS` / `ENHANCE_INGRESS_CONNECTIONS` | 后台与入口连接池大小，示例各为 `8`；额度模块另占最多三条锁连接 |
| `ENHANCE_REDIS_URL` | 审计回合键与 `previous_response_id` 映射使用的 Redis 连接；必须使用独立 ACL、逻辑库及 `sub2api_enhance:` 键前缀权限。有效期固定 30 分钟，不再通过环境变量配置 |

可用下面的命令生成加密密钥，将输出填入环境文件并妥善备份；更新版本时保持不变，否则已有审核节点凭据将无法解密：

```bash
openssl rand -base64 32
```

通知功能直接复用同一数据库中原版 Sub2API 的 SMTP 设置（`smtp_host`、`smtp_port`、`smtp_username`、`smtp_password`、`smtp_from`、`smtp_from_name`、`smtp_use_tls`），每次投递读取最新值；页面里的“管理员通知邮箱”是收件人，不是发件服务配置。增强服务环境文件中的 `SMTP_*` 仅作为本地开发回退，不作为线上唯一配置来源。

```bash
sudo systemctl start sub2api-enhance
sudo systemctl status sub2api-enhance --no-pager
curl -fsS http://127.0.0.1:18081/health
```

如果改了监听地址，健康检查 URL 也要对应修改。首次启动自动执行自有表迁移，以后按迁移校验和识别已执行版本，无需手工导入增强 SQL。启动失败时查看：

```bash
sudo journalctl -u sub2api-enhance -n 100 --no-pager
```

## 3. 配置公网反向代理

**菜单可打开，不代表模型请求已接入增强服务。** 提示词审计要求模型流量经过增强代理：

```text
浏览器访问后台 /api/、/admin/ 等 → 原版 Sub2API
浏览器访问 /enhance/            → 增强服务（管理页面和接口）
客户端访问模型 API              → 增强服务 → 原版 Sub2API → 模型供应商
```

将 [deploy/nginx.conf.example](deploy/nginx.conf.example) 中的分流规则合并到现有站点，替换域名、证书及两个内部端口。可先下载示例查看，**不要直接覆盖现有站点配置**：

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/sub2api-enhance/main/deploy/nginx.conf.example -o /tmp/sub2api-enhance-nginx.conf.example
```

- `/enhance/` 转到增强监听地址，保留示例中的客户端 IP、协议头和不记录 token 查询串的日志设置。
- `/v1/`、`/v1beta/`、`/responses` 等模型路径转到增强服务；完整路径及 WebSocket/SSE 设置见示例。核对客户端实际使用的别名是否也在分流范围内。
- 其他后台路径继续直达原版。`SUB2API_INTERNAL_URL` 必须绕过增强入口，否则会递归。
- 保留 WebSocket 升级头，关闭流式缓冲与模型 POST 的代理自动重试。示例已在 `http` 块设置 `client_max_body_size 256m;`（与原版默认上限一致）；Nginx 默认只放行 1MB，Claude Code 的长对话很容易超过。
- 原版业务端口只向本机/可信内网开放，避免客户端绕过审核。若原版在 Docker 中，给宿主机脚本提供受限的本机映射端口；容器内的 `127.0.0.1` 与宿主机不是同一地址。

增强进程停止或启动失败时，当前示例会让模型入口返回 502，这是故障可见性和审核不绕过的默认取舍。若部署方明确接受增强服务故障时回落原版，可只给模型 API location 单独配置备用 upstream：

```nginx
upstream sub2api_api {
    server 127.0.0.1:18081;
    server 127.0.0.1:18080 backup;
}

location ~ ^/(v1/|v1beta/|responses(/|$)|messages/) {
    proxy_pass http://sub2api_api;
    proxy_next_upstream error timeout;
    proxy_next_upstream_tries 2;
}
```

不要把 `/enhance/` 管理入口接入备用 upstream，也不要加入 `http_503` 回落条件；增强服务返回的审核不可用 503 必须保持为 503，不能借 Nginx 回落绕过审核。启用回落前需确认原版端口只允许本机访问，并用 `nginx -t` 检查配置后再 reload。

合并配置后检查并重载 Nginx：

```bash
sudo nginx -t && sudo systemctl reload nginx
```

完成同域分流后，原客户端通常继续使用原来的域名和模型 API Key，无需重新配 Key，也无需在原版添加一个指向增强服务的模型账号。

## 4. 在 Sub2API 中添加增强菜单

### 推荐：运行菜单脚本

```bash
sudo bash /opt/sub2api-enhance/configure-sub2api-menus.sh /etc/sub2api-enhance/sub2api-enhance.env
```

脚本通过原版设置 API 幂等维护两个管理员菜单和 SVG 图标，保留其他菜单及已有排序。它读取环境文件中的内部地址、公网 Origin 和管理员 Key；当前要求原版内部地址为本机 HTTP IP 加端口、公网地址为 HTTPS Origin。

运行成功后刷新原版后台，即可在侧边栏打开：

| 菜单 | 管理员自定义菜单 URL | 原版菜单路由 |
| --- | --- | --- |
| 第三方提示词审计 | `https://gateway.example.com/enhance/third-party-prompt-audit` | `/custom/enhance-prompt-audit` |
| 用户额度跟随 | `https://gateway.example.com/enhance/quota-follow` | `/custom/enhance-quota-follow` |

将示例域名替换为自己的 `ENHANCE_PUBLIC_ORIGIN`。菜单配置填写 `/enhance/` 对应的完整 URL，不能把 `/custom/` 外层路由当作嵌入地址。

### 手工配置

也可以在 **Sub2API → 系统设置 → 自定义菜单** 中添加上述两个完整 URL，将可见范围设为“管理员”。图标可填写 SVG；脚本已提供默认盾牌和时钟图标，无需修改原版前端源码。

从原版菜单打开后，增强页面复用原版管理员登录、主题和语言，不需要创建增强管理员账号，也不要手动在菜单 URL 中拼接 token。增强会话过期会尝试自动恢复；原版登录过期时点击“重新连接”。

## 5. 启用功能并验证接入

### 第三方提示词审计

v0.3.0 起不再保存请求原文：服务只保存去重后的稳定指令块和当前回合用户提示词，并按内容哈希全库复用审核结论。

1. 从原版侧边栏打开“第三方提示词审计”，进入“配置”。
2. 填写审核节点地址和供应商 Key，点击“获取模型列表”并选择模型；列表不可用时可手动输入模型名，节点名称自动生成。接口地址可填写服务根地址、尾部 `/`、尾部 `/v1` 或带前缀的 `/openai/v1`，增强服务会统一拼接模型列表和 Chat Completions 路径。已保存节点的 Key 留空会保留原值。
3. 需要关闭思考或传递供应商专有参数时，在“审核节点高级配置（providers.json）”中编辑 `parameters`，应用到表单后再保存。凭据不会显示或保存到这段 JSON，`model`、`messages`、`stream` 由增强服务固定管理。
4. 使用“测试此节点”确认节点能返回合法评分。“查看调用详情”可查看上游原始响应；节点测试不会生成正式审核任务，也不会执行用户处罚。
5. 配置平台/分组、不审核用户、运行模式、提醒与停用规则、记录保留天数后保存。
6. 用现有客户端向已分流的公网模型 API 发起一条普通请求，在“任务”页核对新建任务、审核目标和结论来源。只测试审核节点不能验证真实请求是否经过增强代理。

| 模式 | 行为 |
| --- | --- |
| 关闭 `off` | 不解析、不建任务、不保存任何内容，业务请求透传；流量经过增强代理时仍依赖其可用性 |
| 异步 `async` | 首次请求立即转发，任务在后台审核；结论写入回合键，同一回合后续续跑一旦查到阻断就拒绝。Redis 不可用时放行并记运行日志 |
| 同步 `blocking` | 首次请求等待任务完成（等待 Worker 槽位最多 5 秒）；判定违规即拒绝，无法完成审核按不可用处理；续跑只绑定已审回合 |

**审核目标**：每个稳定指令块（`system` / `developer` / `instructions` 文本，以及能识别的 AGENTS.md、CLAUDE.md 等项目指令文件）和当前回合用户提示词各自独立调用模型，不做联合审核。任务结论按 `block > review > pass` 取各目标中最严重的一项，并记录触发的目标类型。当前回合用户提示词是“最后一个 assistant / model / tool 条目之后的全部用户文本”，同一条消息拆成的多个文本块按原顺序合并。

**易变片段**只在匹配严格结构模板时剔除：Claude Code 的 `<env>`、git 状态、待办提醒，Codex 的 `<environment_context>`，Gemini 的会话上下文，以及 billing header 等。模板同时约束位置、字段和长度上限；不匹配模板的内容一律保留审核。客户端指纹（User-Agent、`Originator`）只用于选择规则集，不作为安全边界；规则表可在接口 `/contract` 中查看。

**回合与续跑**：回合键 = API key + 回合指纹（有序指令块哈希 + 用户提示词哈希），保存在 Redis，有效期 30 分钟，命中时刷新。首次请求重复提交会命中同一回合并沿用既有裁决；续跑请求不建任务、不计数，按历史中最后一段用户文本计算回合键，绑定不上（孤儿续跑）时按首次请求审核这段用户文本和新出现的指令块。使用 `previous_response_id` 的 Responses 请求通过上游响应 ID 的映射绑定回合，映射缺失时直接转发并记运行日志。

**处置**：指令块与用户提示词使用同一套提醒、累计违规和停用逻辑，归属到发起请求的用户。指令块违规按“用户 + 指令块哈希”只计一次；同一指令块累计让 2 个不同用户违规即熔断，暂停该哈希的累计与停用，但 blocking 模式仍然阻断、async 模式的回合阻断照常生效。熔断只能由管理员在“信任与熔断”页“信任”或“确认违规”结束，确认后不再熔断；对从熔断列表信任的哈希取消信任会恢复为熔断中。计数重置不清除已计记录。全部窗口提醒适用于被审核用户，累计违规、自动停用和累计重置只适用于普通用户，因为原版 Sub2API 拒绝停用管理员。

**重新审核**在任务上追加轮次，可强制跳过历史缓存和并发复用；信任优先，已信任的指令块不调用模型；重审轮次本身不触发新的处置。

**复用**：审核结论按“内容哈希 + 评估语义（政策、固定协议版本、目标类型、启用节点语义）”全库复用，不含原请求协议，允许跨协议复用；同一结论正在审核时后到的任务共享结果。缓存命中与并发复用都不占节点并发位，也不更新节点健康。

**节点失败与重试**：已有目标判定 `block` 时任务结论已经确定，可直接结束；否则只要有目标失败任务就没有结论，async 只重试失败的目标，成功目标的结论保留。上游拒绝、空响应等协议失败会在当前节点做一次格式修正，仍失败则记录原始响应。节点凭据不会写入任务、日志或调用元数据。

**记录保留**：默认 30 天，可在 1–365 天之间配置；每小时分批清理过期任务、轮次、目标和真实调用流水，处置状态、信任记录、熔断记录与“已计”记录不清理。指令块的审核结论被清理后，同一指令块再次出现会重新审核一次。

**运行概览**只展示基于审核任务的指标：首次请求数（含孤儿续跑）、裁决分布、失败、重新审核、等待队列、处置动作库存、blocking 等待耗时，以及按客户端指纹统计的指令块缓存未命中率；未命中率只展示、不告警。

**隐私边界**：不审核用户和全局 `off` 时不保存任何提示词内容、不创建任务、不执行处置。被拒绝的续跑只打一行运行日志，不落库、不展示。

**已知边界**：工具结果内容、媒体内容（如图片中的文字）不审核；只审核当前回合的用户文本，分散在多轮里的违规意图无法发现；严格模板内的短自由文本（如 git 提交说明）在长度上限内不审。

### OpenAI 日/周额度跟随

1. 在原版“账号管理/分组管理”中准备目标分组的 OpenAI 账号；参加用户需是有效普通用户，有该分组访问权限及 OpenAI 配额记录。
2. 在增强“用户额度跟随”页选择目标分组，勾选日/周窗口，先启用“观察模式”并保存。
3. 核对有效账号、参加用户、时区和重置边界；默认每轮随机间隔 10–15 分钟。所有有效账号周窗口一致确认推进后，才产生跟随事件，利用率下降本身不代表重置。
4. 确认观察结果后关闭观察模式，后续新事件才会调用原版接口归零用户所选日/周用量。观察期间的旧事件不会补发，模式切换会重新建立基线。
5. 需要管理员立即处理时，点击“立即重置”并确认；系统会按当前目标分组和已勾选的日/周窗口，逐个调用原版 Sub2API 重置接口处理全部参加用户。该操作不等待自动检测边界，也不受观察模式限制；部分用户失败时页面会保留逐项结果。

如需周一旧周用量结转及缓存修复，还需完成：

| 配置 | 要求 |
| --- | --- |
| `QUOTA_FOLLOW_REDIS_URL` | 指向原版实际使用的 Redis 实例及数据库编号，不要使用另一个空数据库；账号允许 `HGETALL`、`DEL`、`SREM` 及必要连接握手，键限定为 `billing:user_platform_quota:*` 和 `billing:upq:dirty` |
| `SUB2API_TIMEZONE` | 与原版实际 IANA 时区一致，例如 `Asia/Shanghai` |
| `SUB2API_USER_PLATFORM_QUOTA_FLUSHER_ENABLED` | 核实原版 `database.user_platform_quota_flusher_enabled` 确实关闭后填 `false`；这个环境变量不会改变原版配置，空值或 `true` 会阻止结转及缓存清理 |
| 数据库权限 | 具备前述配额表两列的 `UPDATE` 权限 |

原版自然日和周一规则继续保留；周一结转保留或加回旧周已用额度，再等待 OpenAI 账号窗口确认推进后归零。快照过旧、窗口冲突或错过安全窗口会跳过，不追补历史。普通额度归零调用原版管理 API，因此在原版“操作日志”留下 `audit_logs` 证据，但它不是模型调用，不会出现在原版模型“使用记录”中；周一结转直接在增强事务内维护配额，也不会生成原版操作日志。DB、Redis 与原版并发计费不能保证跨系统强一致；结果不确定时使用记录详情中的只读核对，不盲目重复归零。

## 更新与日常维护

升级到 v0.3.0 前先确认两件事：环境文件中的 `ENHANCE_REDIS_URL` 已配置且独立 ACL/逻辑库可连接（缺少或不可达时新进程拒绝启动，`ENHANCE_AUDIT_CACHE_TTL` 已下线，可从环境文件删除）；审计处置动作里没有未完成的执行或通知。v0.3.0 是不兼容版本：迁移 `013` 会删除旧采集表（`captures`）、旧任务与 Outcome 表、批次表和旧 `model_attempts`，并把处置表的来源列改为新任务、提醒窗口游标置 0。

- 升级前备份数据库；处置动作未完成时迁移会主动中止并给出错误，请先在管理页面处理完成。
- 迁移会清理旧审计数据，旧采集原文无法恢复；需要留档请在升级前自行导出。
- 已下线的功能：请求原文采集与下载、采集恢复、批次审核、“仅采集、等待管理员审核”、“审核关闭时仍采集原文”、`audit_scope` 配置以及 `effective_behavior` 分阶段结论。
- 升级后首次进入页面请检查保留天数（默认 30 天）和管理员通知邮箱；未配置 `admin_email` 时熔断通知只在页面显示。

点击增强页面标题旁的 **版本号** 打开更新面板：检测更新 → 下载并应用 → 确认重启。新进程和目标版本就绪后页面会自动刷新；超过两分钟未确认恢复会提供手动连接入口。旧版页面升级到支持自动恢复的版本时，可能仍需手动刷新一次。

命令行也可升级；`/tmp` 中的脚本可能被系统清理，使用前重新下载：

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/sub2api-enhance/main/deploy/install.sh -o /tmp/sub2api-enhance-install.sh
sudo bash /tmp/sub2api-enhance-install.sh upgrade
```

使用 `-v` 可指定一个已发布稳定标签，例如 `upgrade -v v0.1.8`；不加版本号则安装最新稳定版。恢复本地备份：

```bash
sudo bash /tmp/sub2api-enhance-install.sh rollback
```

更新保留环境配置和业务数据。恢复旧二进制只允许迁移集合一致，不能代替数据库恢复。网页更新只替换程序；环境变量、systemd、Nginx 和菜单脚本有调整时需按发布说明更新。私有仓库或 API 限流时可使用仅用于 GitHub 发布查询/下载的 `UPDATE_GITHUB_TOKEN`；命令行执行时需让安装脚本进程获得该变量。

| 现象 | 优先检查 |
| --- | --- |
| `/enhance/` 页面打不开 | 服务健康状态、反向代理路由、HTTPS 域名与 Origin |
| 菜单能打开，但没有任务 | 客户端实际 API 路径是否转到增强服务，是否绕过了代理 |
| 请求已转发但没有任务 | 运行模式是否为 `off`、是否在不审核用户名单、分组/平台范围、身份或协议是否可解析（续跑请求本来就不建任务） |
| 模型列表或节点测试失败 | 节点地址、模型及供应商 Key，调用详情中的 HTTP 状态和原始响应 |
| 邮件未发送 | SMTP 发件配置、收件地址及动作投递状态 |
| 额度不跟随 | 模块开关、观察模式、账号一致边界、参加用户及分组权限 |
| 周一结转被跳过 | 原版 Flusher 实际状态、Redis、时区、列权限及快照/窗口原因 |

## 范围与限制

已适配 Responses、Chat Completions、Messages、Gemini 等文本请求及 Responses WebSocket，并支持 identity、gzip、deflate、zstd 请求正文；解压后正文上限固定 64MB，超过直接返回 413 且不转发。入口从临时文件按类型化解码，只取出审核目标后立即丢弃正文，不构造整棵 `map[string]any` 结构。仅审核客户端实际携带的输入，不主动获取上游隐含历史，不审核模型新生成的输出。multipart 只提取文本字段。未知协议或身份不可用时，blocking 按 `eligibility_unknown` 拒绝、async 直接转发且不落库。未适配的 WebSocket（包括部分 Realtime）在启用审计时明确报错。

任务列表支持按时间、用户/API Key/分组、状态、裁决、轮次类型、运行模式、平台和请求 ID 筛选，并可选择每页 20、50、100、200 条。“任务”页详情按目标展示指令块或用户提示词正文、结论来源（模型 / 缓存 / 并发复用 / 信任）、节点投票与真实调用；“信任与熔断”页管理信任列表和熔断列表。

动态 composite 分组可能无法可靠确认最终平台；大正文、媒体和长连接的资源需求取决于数据库及部署环境，需按实际客户端验收。旧分支的审计数据不会自动导入或删除。

## 从源码构建

开发环境需要 Go 1.27.0 和 pnpm。后端为 Gin / PostgreSQL，前端为 Vue 3 / TypeScript / Vite。

```bash
make install
make generate
make test
make build
```

输出为 `backend/bin/sub2api-enhance`。前端资源嵌入二进制；单独运行后端检查前先执行 `make frontend`。测试覆盖前端、Go race/vet、模拟上游和 SQLmock、部署脚本静态行为，不替代真实数据库及生产链路验收。

仓库通过 GitHub Actions 在推送稳定版本标签时测试并发布 Linux amd64/arm64 归档和 `checksums.txt`。实际发布结果见 [Releases](https://github.com/MADAO-NW/sub2api-enhance/releases)。

## 许可证

本项目包含从 Sub2API 改编的代码，派生部分遵循 [GNU LGPL v3](LICENSE)。原始 MIT 文本保存在 [LICENSE-MIT](LICENSE-MIT)，适用边界见 [NOTICE](NOTICE)。
