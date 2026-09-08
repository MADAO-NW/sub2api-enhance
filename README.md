# sub2api++

sub2api 的独立增强服务。当前已迁移第三方提示词审计核心及管理页面，并实现外置 HTTP/Responses WebSocket 采集、持久动作和管理员会话。已新增用户日/周限额跟随模块，默认关闭并以观察模式起步。

项目包含由 sub2api 改编的代码，相关派生部分继续遵循根目录 `LICENSE` 中的 GNU LGPL v3。仓库初始化时选择的 MIT 文本保存在 `LICENSE-MIT`，其适用边界见 `NOTICE`，不会覆盖第三方或派生代码的原许可。

原 sub2api 源码、构建、表结构与迁移台账保持独立。模型输入先保存到 `sub2api_enhance`，再转发到原版；用户停用和启用通过原版管理 API 完成，不直接写原用户表。额度跟随通过原版 API 归零日/周用量；保留自然日和周一规则，并由增强服务在周一受控结转旧周用量。结转仅更新原 OpenAI 配额的 weekly_usage_usd、weekly_window_start；缓存修复仅清理该用户 OpenAI 配额键及对应脏成员。

## 结构与构建

- `backend/`：Go 1.27.0、Gin、Wire、database/sql、PostgreSQL；独立 module `sub2api-enhance`。
- `frontend/`：Vue 3、TypeScript、Vite、Pinia、Vue Router、TailwindCSS，pnpm 管理固定依赖。
- `backend/migrations/001_prompt_audit.sql`：提示词审计初始结构。
- `backend/migrations/002_quota_follow.sql`：额度跟随自有表；两份 SQL 均不修改原版表结构。
- `deploy/`：环境变量和 Nginx 配置示例，不是生产配置。

依赖已固定在 go.mod/go.sum 与 pnpm-lock.yaml。全新环境按用户授权规则安装后可执行：

```bash
make install
make generate
make test
make build
```

前端构建产物由 Go 二进制嵌入。单独执行后端检查前，先执行 `make frontend`；产物位于 `backend/bin/sub2api-enhance`。构建和测试均不启动真实应用或连接实际业务数据库。

`go test` 中的 HTTP/WebSocket 测试使用测试进程内的假上游，数据库使用 SQLmock；它们不等于原版服务运行态联调或 PostgreSQL 真实执行验收。

## 方式一：脚本部署与在线更新

发布仓库固定为 [MADAO-NW/sub2api-enhance](https://github.com/MADAO-NW/sub2api-enhance)。以下下载命令在源码上传且首次 GitHub Release 成功后可用；当前源码目录不包含正式发布包。

服务器需要 Linux amd64/arm64、systemd，以及 curl、tar、sha256sum、jq、flock 等系统工具。沿用已经运行的 PostgreSQL 15+、Redis 7+ 与原版 sub2api；脚本不安装或重启这些依赖。

```bash
curl -fsSL https://raw.githubusercontent.com/MADAO-NW/sub2api-enhance/main/deploy/install.sh -o /tmp/sub2api-enhance-install.sh
sudo bash /tmp/sub2api-enhance-install.sh install
sudoedit /etc/sub2api-enhance/sub2api-enhance.env
sudo systemctl start sub2api-enhance
sudo systemctl status sub2api-enhance
sudo journalctl -u sub2api-enhance -f
```

首次安装创建独立系统用户、`/opt/sub2api-enhance`、环境文件和 systemd 服务，并启用开机启动；填写真实配置之前不启动应用。前端与迁移已嵌入二进制，服务器无需 Node、pnpm、Go 或源码，也不另建管理员账号。原版内部地址使用 `SUB2API_INTERNAL_URL`。

启动时自动创建 `sub2api_enhance` 及自有表；已有迁移按文件名与校验和跳过。数据库角色仍须具备下文列出的权限，脚本不擅自使用超级用户创建数据库或扩大原表权限。

两张增强页面顶部的“版本与更新”提供检测版本、查看发布说明、下载应用、备份恢复和重启。更新与恢复携带管理员确认的版本号，发布或备份变化时要求重新确认。仅 Linux Release 允许在线替换；源码构建仍可查看版本。重启沿用应用 shutdown 流程，由 systemd `Restart=always` 拉起，可能短暂中断增强代理连接；随后需从原版后台重新打开菜单恢复管理员会话。

命令行升级与指定版本安装：

```bash
sudo bash /tmp/sub2api-enhance-install.sh upgrade
# 将下面的 v1.2.3 替换为仓库中实际存在的稳定标签
sudo bash /tmp/sub2api-enhance-install.sh upgrade -v v1.2.3
sudo bash /tmp/sub2api-enhance-install.sh rollback
```

脚本先完成下载、校验及版本检查，再停止并替换增强服务。成功启动要求健康接口的服务标识和版本均匹配；失败保留备份并报错，不自动降级数据库。网页和脚本使用同一 `.update.lock`，进度与备份信息保存在安装目录 `.update-state.json`；进程重启后核对文件指纹恢复操作状态。配置文件、已保存业务数据和原 sub2api 的安装目录不被版本替换覆盖。

版本恢复仅允许迁移指纹相同的程序。若新版本已增加或修改数据库结构，优先发布修复版本；需要恢复旧数据库时另行安排备份恢复。在线更新只替换二进制，新环境变量或 systemd 配置要求仍应按该版本发布说明处理。

私有仓库或 API 限流时，用 `UPDATE_GITHUB_TOKEN` 注入具有该仓库只读权限的 GitHub 凭据。它与原版管理员 API Key 独立，不发送到前端。私有仓库先通过有权限的渠道取得安装脚本，再在授权环境中执行；普通公开 raw 下载命令不适用于私有仓库。

## GitHub 标签发布

`.github/workflows/ci.yml` 在 main 和 PR 上执行本地同类检查；`release.yml` 在推送 `v主版本.次版本.修订号` 稳定标签时运行校验、测试、前端构建及 GoReleaser。版本、提交与时间在构建时注入，不由程序运行时读 Git；本地普通构建显示 dev/source。

发布产物包括 Linux amd64/arm64 压缩包和 `checksums.txt`。每个包内包含 `sub2api-enhance`、根目录 `release.json`、许可证、README 及部署模板，下载与更新必须校验 SHA256。版本元数据与二进制均来自标签指向的同一源码；元数据在 GoReleaser 的独立临时输入目录生成，不占用其 `dist` 输出目录。归档路径按 [GoReleaser 文件打包规则](https://www.goreleaser.com/customization/package/archives/)配置。

仓库 Actions 使用自动提供的 `GITHUB_TOKEN` 发布 Release；无需把私人 Token 写进代码或 workflow。发布不会自动向源码分支写回版本文件，也不会构建 Docker 镜像或修改线上服务。

首次上传应包含 `.github/`、`.goreleaser.yaml`、`.gitignore`、LICENSE、README、Makefile，以及 backend/frontend/deploy 的源码、测试、锁文件和模板。`go.mod/go.sum`、`pnpm-lock.yaml` 与 Wire 生成源码必须保留。以下内容由 `.gitignore` 排除：本地方案 `my_local_doc/`、`.codegraph/`、node_modules、二进制、嵌入前端构建产物、dist 发布包、测试输出、日志、真实环境文件、私钥和更新运行状态。忽略只控制 Git 上传，不删除本地文件。

推送源码、提交、创建/推送标签及真正发布仍属于部署人员的明确操作；不能把 workflow 配置完成等同于已执行 GitHub 发布。

## 首次部署前提

1. 使用实际未改版 sub2api 验证身份表、分组字段、`/api/v1/auth/me`、管理员用户状态 API，以及原版会话 IP/UA 绑定。适配基线为本地 main 0.2.1，不代表所有上游版本都兼容。
2. 为增强服务准备同库独立 Schema `sub2api_enhance`，给予自己的表和迁移台账所需权限。对 `public.api_keys`、`users`、`groups`、`user_allowed_groups` 仅授予必要 SELECT；不授予原用户表 UPDATE 或 public DDL。使用额度模块时增加 `accounts`、`account_groups`、`user_platform_quotas`、`audit_logs` 的必要 SELECT。周一结转需另授予 `public.user_platform_quotas(weekly_usage_usd, weekly_window_start)` 两列 UPDATE；不会修改限额配置、日/月用量或原表结构。Redis 使用独立 ACL 账号。
3. 服务首次启动会在专用锁中执行自有 SQL migration；启动属于会触发数据库写入的动作，需提前确认。Schema 未预建时，启动角色还需具备目标数据库 CREATE 权限；也可预建由增强角色拥有的 schema，避免授予数据库级 CREATE。不要因此授予 public DDL 权限。
4. 将 `deploy/.env.example` 的值放入源码目录外的环境文件或 Secret，替换数据库凭据、内部地址、管理员 API Key 和独立加密密钥。管理员 Key 只在后端用于账号动作；页面身份始终使用访问者原版 JWT。
5. 修改 Nginx 前核对实际端口、可信代理链、输入大小限制、流式超时和全部模型别名。示例不提供自动旁路或 POST 重投；不要开放原版业务端口供外网绕过采集。

由用户启动或重启增强服务及原版测试实例后，才进行另行授权的运行态测试。本项目不提供自动启停原版服务的脚本。

## 页面与运行方式

原版管理员自定义菜单 visibility 设为 admin，URL 指向实际域名下的：

```text
/enhance/third-party-prompt-audit
```

安装或升级会把 Release 包中的菜单脚本放到 `/opt/sub2api-enhance/`。脚本从增强服务环境文件读取固定原版内部地址、公网 Origin 和管理员 API Key，幂等保留其他自定义菜单，并维护“第三方提示词审计”“用户额度跟随”两个管理员菜单及其 SVG 图标：

```bash
sudo bash /opt/sub2api-enhance/configure-sub2api-menus.sh /etc/sub2api-enhance/sub2api-enhance.env
```

原版会附带 token、theme、lang 等参数。增强页面立即清理 URL token，以 POST 交换短时 HttpOnly 会话；后续每次管理 API 调用重新向原版验证访问者身份。非本机页面要求 HTTPS。同域页面属于可信后台集成，不能作为权限隔离沙箱。

页面提供概览、事件、任务、原文采集及配置。事件和任务显示对应 Capture ID，可直接查看同一份采集原文。配置保留草稿、修订冲突、单节点试审、阈值和多节点聚合；模型凭据输入留空时保留已保存值，填写后替换。节点模型通过后端代理的 OpenAI 兼容 `/v1/models` 接口选择，节点名称由模型名自动生成，重复模型依次追加 `-1`、`-2`。

- `off`：透传，不创建新审核任务。流量仍经过增强代理时，代理进程仍是可用性依赖。
- `async`：原文保存成功后转发，后台审核。原文保存失败返回 503；模型故障不改变已转发请求。
- `blocking`：原文先保存，审核通过/复核才转发，违规拒绝；不能验证身份或协议时失败关闭。

第三方提示词审计只由增强服务自身的 `off`、`async`、`blocking` 模式控制，不读取或写入原版 `risk_control_enabled`。因此可以在不启用原版风控中心的情况下独立采集和审核。

新配置默认使用 50% 待复核/联合审核触发阈值和 80% 违规阈值；提醒与停用都默认关闭，启用时默认使用最近 10 次正式审核中 3 次违规提醒、累计 5 次违规自动停用。账号 API 结果未知时只读核对，避免重复写入；通知只能在账号动作确认后发送。人工恢复和重新审核不重复处罚，重新审核完成后更新原事件的最新结论。

配置页可从全部未删除用户（包括管理员）中选择不审核用户。排除仅跳过审核任务、模型调用、阻断和处罚，入口仍按可靠采集要求先保存原文。自动停用后的用户启用和累计清零集中放在配置页“用户违规累计管理”，不放入单条事件或任务详情。

## 当前协议边界

| 输入 | 已实现处理 | 尚需实环境验收 |
| --- | --- | --- |
| Responses、Chat Completions、Messages、Gemini 文本 | 字节留存、严格 JSON 提取、原样 HTTP/SSE 转发 | 真实客户端、取消与大正文 |
| Embeddings、Alpha Search、图像/视频/音频文本入口 | 对应文本提取与采集；未知协议保留原文并显示原因 | 厂商路径、载荷及媒体客户端 |
| multipart | 完整接收后转发；保存文本字段的名称、顺序、重复项和原字节封装，二进制仅保存描述 | 大文件、编码和故障注入 |
| Responses WebSocket | 逐文本消息保存；response.create 审核；控制帧传递与协议错误返回 | 分片、压缩、长连接和真实错误信封 |
| 其他 WebSocket（包括未适配的 Realtime） | 启用审计时明确返回 unsupported_websocket | 需要独立协议适配，不宣称已覆盖 |
| 动态 composite 分组 | 留存原文，标记实际平台未知，不自动处罚 | 需要原版可验证路由契约 |

保存 gzip/deflate 原始实体后解压提取；其他 Content-Encoding 留存原字节但不宣称可以审核。仅采集客户端实际携带的输入，不主动拉取上游隐含历史，不审核模型新生成输出。

大正文接收阶段使用临时文件，提交 BYTEA 与解析时仍需要内存，受 PostgreSQL 单值能力和实际资源限制。不能据此宣称无限输入容量或既定吞吐。数据库压力、媒体容量、断连、故障恢复以及所有协议的生产性能尚未实测。

## 验证状态与历史来源

已迁入原分支的政策、JSON/角色/轮次提取、联合裁决、复用、多模型、队列、检查点及前端测试；新增测试覆盖采集确认、HTTP/WS 保存前禁止转发、管理员会话、未知账号动作与迁移校验。

部署代码已通过 Go 单元/race、vet、前端类型/lint/20 个测试、构建及安装脚本离线测试；Linux amd64/arm64 交叉编译和不加载运行配置的版本输出也已通过，YAML 已解析核对。本地未安装 GoReleaser，未执行完整 Release 打包；真正的 GitHub Actions、systemd 安装和网页替换/重启仍须在发布及既有测试实例上验收。

尚未执行：实际 PostgreSQL migration、真实 sub2api 联调、浏览器验收、真实审核模型调用、SMTP 投递、账号状态操作、生产切流、旧数据导入。旧表不会自动导入或删除；当前不能据此标为 A01—A24 的生产验收全部通过。

迁移来源：`feat/third-party-prompt-audit` / `f5b4fa6f6d47d55896d729cddb6ae6a82ba673c3`。本项目不使用 Go replace、源码符号链接或共用 node_modules 依赖原仓库，保留来源 LICENSE。

## 额度跟随模块

菜单 URL 使用实际同域地址 `/enhance/quota-follow`，复用管理员会话、主题和语言。页面包含配置、紧随其后的重置记录、一致事件和账号状态。

当前实现一个配置分组，发现其中所有有效 OpenAI 账号，以及有 OpenAI 配额记录的有效普通用户。首次观测建立基线；只有所有账号的 `seven_day.resets_at` 均确认推进、旧边界已到达且候选相差不超过五分钟，才形成唯一事件。利用率下降只提示疑似，边界回退不覆盖可信基线。

默认检测间隔每轮重新随机为 10–15 分钟。启用周期、账号集合、日/周选择以及观察模式切换会重新建立基线。观察事件不创建用户交付，也不会在以后关闭观察模式时补发。

每个事件、用户、窗口只有一条交付，按顺序调用原版 reset API。请求前持久化 inflight；超时、断连、5xx、提交不确定或重启后的无终态发送均保持 uncertain，不自动重调。记录详情的“只读核对”只读取数据库/配额缓存；与原版成功审计准确关联后才确认未知调用的来源。

原版日志采用已处理审计 ID 去重补扫，同时保留 `(created_at,id)` 水位，防止异步迟到日志被高水位跳过。自然窗口变更至少等待下一次完整日志采集再推断；存在多份证据或不确定调用时保留待归因。列表不承诺完整还原停机期间全部自然重置。

额外环境配置：

- `QUOTA_FOLLOW_REDIS_URL`：只观察及 API 跟随时可选；周一结转与缓存修复必需。业务命令为 HGETALL、DEL、SREM；键限定 `billing:user_platform_quota:*` 和 `billing:upq:dirty`。需允许 SDK 的 AUTH、HELLO、SELECT 等必要握手；没有 SET/HSET/EVAL。
- `SUB2API_TIMEZONE`：原版实际 IANA 时区，用于周一调度与自然边界归因；缺失时阻断结转。
- `SUB2API_USER_PLATFORM_QUOTA_FLUSHER_ENABLED`：必须与原版实际配置一致；显式 `false` 才允许结转和缓存清理，空值/true 均阻断。增强服务不会修改原版 Flusher，也不能通过现有 API 自动读取其进程配置。

只读缓存按已核对的 schema_version=1、金额字段和 Unix 秒窗口校验。键缺失可记录 absent；window_matches 只代表窗口秒值一致，不保证并发用量完全同步。配置错误不影响提示词审计启动。原版归零成功后，陈旧或未确认缓存进入清理恢复；未知归零不擅自清缓存，待原版审计确认。缓存恢复只删除键和对应脏成员，不再次归零；失败每分钟重试，详情可手动重新排期。

额度的三个调度使用独立锁连接池（最多三条锁连接），避免后台池较小时因持锁连接占满而自锁；实际数据查询仍使用现有后台池。部署时将这三条连接计入 PostgreSQL 总连接预算。

周一结转已实现：同一启用周期、同一账号集合下保存周日前最后一次 DB 已提交用量；Flusher 关闭时以 DB 为来源，不取 Redis/DB 最大值。每秒检查周边界，按配置间隔保存快照，并在边界前最后一秒尝试补采。周一安全窗口及快照有效期均为一个最大检测间隔。

尚未懒重置时保留锁内最新用量并推进周窗口；已经自然重置时加回快照并保留周一新消费。金额使用精确十进制计算。同周唯一记录与原金额更新在同一事务提交，崩溃或提交未知不会重复相加。旧窗口用量下降、其他归零证据、账号不一致、快照过旧等情况均跳过。记录区展示“周一用量结转”和完整前后值。

跨 DB、Redis 与原版并发计费无法强一致：快照至边界间消费、缓存/DB 暂时不同步、DB 写失败、异步审计未落库或手工改库可能造成漏补/无法识别的冲突。错过安全窗口不追补历史。关闭模块/观察模式不再新增金额动作，但会收尾之前已提交动作的缓存清理。

新 migration 尚未实际执行。真实 API、归零、结转写入、Redis ACL、故障恢复、日志归因和浏览器仍需另行授权，并使用用户启动的既有实例联调。
