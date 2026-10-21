# AGENTS.md

本文件约束 MailQuay 仓库以及同级 `frontend` 仓库的开发工作. 更具体的子目录规则优先于本文件.

## 1. 开始前

- 先阅读 `docs/architecture.md`, `docs/project-structure.md` 和 `docs/development.md`.
- 涉及接口, 数据模型或环境变量时, 先确认文档与代码一致.
- 不扫描无关目录, 不读取与当前任务无关的仓库.

## 2. 不可破坏的约束

- MailQuay 是已有邮箱的客户端, 不是 MTA, 不实现 SMTP 接收和服务器间投递.
- 收信使用 IMAP, 兼容 POP3; 发信使用 SMTP. 不引入其他私有协议.
- POP3 只作为兼容模式: 单个虚拟收件箱, 本地标志, 轮询, 可选服务器删除. 不模拟文件夹和服务端标志.
- IMAP, POP3, SMTP 的主机和端口都由账号配置, 默认值只用于预填, 不得写死或强制回退到默认端口.
- 部署形态是单用户自托管, 多邮箱账号. 不引入多租户模型.
- 新账号默认同步所有时间 (`initial_sync_range=all`), 下载范围默认不含附件的完整邮件 (`download_scope=full_no_attachments`); 正文拉取和索引都在后台异步执行.
- 同步范围和下载范围在设置中提供默认值, 新增账号预填但不锁定, 已有账号单独修改.
- 已有账号修改范围或下载范围时, 必须创建 `scope_reconcile` 异步任务补拉或清理; 不删除远端邮件.
- 清理时跳过标记, 本地保留和本地归档邮件, 不得误删其 blob 或搜索文档.
- 前端不直接连接 IMAP/POP3/SMTP, 不接触邮箱凭据.
- 界面不得引用第三方品牌名或商标. 环形任务指示器只使用通用命名和自主配色.
- 登录只使用用户名和密码, 不做邮箱绑定, 邮件验证码或验证链接.
- TLS 由外部反代终止; 只在可信代理来源信任 `X-Forwarded-Proto`, 防止伪造.
- 公开地址 (`MAILQUAY_PUBLIC_URL`), CORS (`MAILQUAY_CORS_ORIGINS`), 可信代理 (`MAILQUAY_TRUSTED_PROXIES`) 和非安全传输豁免 (`MAILQUAY_INSECURE_ALLOW`) 只通过环境变量配置, 不写入 `settings` 表, 也不在网页中修改.
- 非 HTTPS 且不命中豁免域名/CIDR 时, 前端顶部显示全宽红色提醒条, 不可关闭, 不可隐藏.
- 默认豁免 loopback 和私网 CIDR; `MAILQUAY_INSECURE_ALLOW` 可追加域名或 CIDR, 只有命中豁免才不渲染提醒条.
- 凭据和 OAuth token 必须加密存储, 不得写入日志, 错误响应或前端状态.
- 只有邮箱账号是 ID 化实体; 用户名, 密码, OAuth token, IMAP/POP3/SMTP 地址直接存在账号上, 不建独立凭据或连接表.
- 收信和发信分别鉴权: 账号保存收信用户名和密码, SMTP 可另外保存用户名和密码; SMTP 凭据留空时复用收信凭据.
- 添加账号使用引导流程: 先输入邮箱和密码, 再确认服务器, 两套凭据和同步范围.
- 服务器发现顺序为内置服务商表, MX 记录, 域名的 `autodiscover` 与 `autoconfig`; 发现结果只用于预填, 保存时必须重新校验.
- 域名可识别 OAuth 服务商时自动使用 OAuth2 并选中对应服务商; 不要求用户手工判断.
- 地址存 `域名:端口` 或 `IP:端口`, IPv6 用 `[地址]:端口`; 传输安全单独存, 不从端口推断.
- 发件身份属于账号, 主键为 `(account_id, email)`, 没有独立身份 ID; 使用账号 SMTP 发信.
- 允许使用任何账号的身份发送或回复; 回复默认使用原邮件所属账号的默认身份, 发送前可切换.
- 侧边栏顶部直接列出全局视图: 全部收件, 未读, 已发送, 发送中, 草稿, 垃圾箱, 已标记, 本地归档; 全局没有"其他".
- 邮箱账号列表在全局视图下面, 每个账号默认折叠; 展开后显示收件箱, 未读, 已发送, 草稿, 垃圾箱; 其余文件夹放入该组最下面的"其他", 默认折叠.
- 每个账号的"其他"展开状态独立保存; Junk, Archive, 服务商标签和自定义文件夹都放在这里.
- 全部视图排除已发送, 草稿和垃圾箱; 未读使用相同范围.
- 跨文件夹去重使用稳定 `dedupe_key`: 服务商消息 ID, 规范化 Message-ID, 原文哈希, UID 依次回退; 纯哈希不能作为首选.
- 同一 Message-ID 但原文哈希不同时, 去重键追加哈希, 避免误合并.
- 文件夹视图不去重; 全部, 未读, 标记和搜索结果去重; 已读和标记动作同步到同一去重键的所有副本.
- 标记只存在本地, 不映射 IMAP `\Flagged`; 标记时保留完整正文和全部附件.
- 删除账号默认保留标记邮件; 非标记邮件, 文件夹和索引异步清理, 不得误删保留邮件的 blob 或搜索文档.
- 本地归档是独立副本, 存 `local_archives`, 不级联账号或文件夹; 远端删除和删除账号都不影响, 不进入"全部".
- 删除默认移动到 Junk 交给服务商清理; `local_queue` 模式写入 `delete_queue`, 到期后永久删除并同步.
- 阅读器顶部左侧是回复, 转发和标记图标按钮, 右侧是下载原始邮件, 本地归档和删除图标按钮; 不显示文字标签.
- 已发送只拉取, 不回写标志, 移动和删除; 草稿和发送中默认只存在 MailQuay 后端.
- 支持定时发送: `scheduled_at` 存 UTC, 到期前不连接 SMTP; 编辑草稿取消定时.
- 自动重试次数由设置 `send_retry_limit` 控制, 默认 3; 永久错误不重试.
- 草稿本地自动保存用 2 秒防抖, 最长 10 秒; 输入过程中不得写远端 IMAP.
- 草稿同步默认 `pull`, 草稿夹可见时低频拉取; `mirror` 才推送, 且在停止输入 30 秒, 关闭窗口或手动保存时执行.
- 草稿推送前比较内容哈希, 未变化不发 `APPEND`; 发送成功后删除关联远端草稿.
- `headers` 下载范围只缓存邮件头, 正文和附件打开时联网拉取; 不能把正文当作已缓存数据.
- `full_no_attachments` 必须保留正文引用的 `cid:` 内嵌图片数据和附件元数据, 普通附件按需拉取; 缩小下载范围时不得删除内嵌图片.
- 并发默认自动检测 cgroup v1/v2 的 CPU 和内存限制, 不假设可以使用宿主机全部资源.
- 运行时可调参数保存在 `settings` 表并通过 `/settings/server` 修改; 环境变量只用于数据库, 密钥, 监听地址, 数据目录等启动引导参数.
- Bleve 写入固定单 writer; 可调的索引并发只体现为批大小, 不能并行写同一索引.
- 手动并发值必须校验范围和内存预算; 自动值, 检测来源和最终值写入启动日志.
- 实时推送使用 SSE. 没有明确的双向需求时不引入 WebSocket.
- PostgreSQL 是唯一数据库. 不引入 SQLite, 不维护第二套迁移或方言分支.
- 全文搜索使用 Bleve 内嵌索引. 索引是派生数据, 可从 PostgreSQL 元数据和 blob 重建, 不得作为唯一事实来源.
- `api/openapi.yaml` 是接口合同源. 不手改生成代码.
- 原始 MIME 存入内容寻址 blob, 不在数据库中重复保存完整原文.

## 3. 目录职责

| 路径 | 职责 |
| --- | --- |
| `cmd/mailquay` | 子命令入口, 只做参数解析和调用 |
| `internal/httpapi` | 路由, 中间件, 参数校验, 错误映射 |
| `internal/auth` | 会话, 密码, CSRF, OAuth2 |
| `internal/sync` | 调度, 初次与增量同步, 操作队列 |
| `internal/imapclient` | IMAP 协议细节和连接生命周期 |
| `internal/pop3client` | POP3 协议细节, UIDL, TOP, DELE 和轮询 |
| `internal/smtpclient` | SMTP 提交和认证 |
| `internal/store` | 仓储接口, pgx/sqlc 实现, 事务 |
| `internal/search` | Bleve 映射, 索引任务, 查询, 重建 |
| `internal/events` | 事件总线与 SSE |
| `internal/settings` | 全局设置, 默认值与并发计算 |
| `internal/mime` | 解析, 正文提取, HTML 清洗 |
| `frontend/src/features` | 按业务资源组织的前端功能 |
| `frontend/src/lib/api` | 生成的客户端和错误处理 |

- 业务包不依赖 HTTP DTO.
- 基础设施包不反向依赖业务包.
- 不新增 `util`, `common`, `helpers` 这类无职责边界的包.

## 4. 命令

后端:

```sh
go run ./cmd/mailquay serve
go run ./cmd/mailquay migrate up
go run ./cmd/mailquay search reindex --all
go test ./...
go test -race ./...
go test -tags=integration ./test/integration/...
go vet ./...
golangci-lint run
```

前端:

```sh
pnpm dev
pnpm build
pnpm lint
pnpm typecheck
pnpm test
pnpm e2e
pnpm generate:api
```

- 只运行覆盖本次改动的针对性测试, 不因小改动执行完整测试套件.
- 提交前至少运行受影响包的测试和一次真实路径的 smoke 验证.

## 5. 后端规则

- 所有阻塞调用接收 `context.Context`, 超时由调用方设置.
- 错误使用 `%w` 包装, 对外错误码在 `httpapi` 统一映射, 不泄露底层细节.
- 日志使用 `slog` 结构化字段. 禁止记录密码, token, Cookie, 完整正文和附件内容.
- 数据库访问必须经过 `store` 接口. 不在处理器中直接写 SQL.
- 多表写入使用事务. 乐观更新与远端对账逻辑必须可重入.
- 列表接口使用游标分页, 不使用 offset.
- 主键使用 UUIDv7, 时间统一 UTC.
- 新增或修改 IMAP/POP3 操作时, 必须说明失败重试, 连接中断后的行为; IMAP 还要覆盖 UIDVALIDITY 变化, POP3 要覆盖 UIDL 变化和 DELE 提交语义.
- 新增环境变量时, 同步更新 `docs/development.md` 和 `.env.example`.
- 新增迁移时只写 `migrations/postgres`, 覆盖升级和回滚.
- 邮件或标志变更必须在同一事务写入 `search_jobs`; 索引失败不得回滚邮件写入.
- Bleve 索引只允许一个进程打开, 启动和退出必须正确获取和释放索引锁.
- 修改搜索字段或分析器时, 提升 `search_mapping_version`, 并提供重建路径.
- 索引是派生数据, 不写入业务判断; 搜索结果必须回表 PostgreSQL 后再返回.

## 6. 前端规则

- 前端是单页应用: Vite 构建单个 `index.html`, 使用 `react-router` 客户端路由, 页面切换不重新加载文档.
- 每个页面有独立路径, 例如 `/`, `/unread`, `/archive`, `/settings/preferences`; 非静态资源路径由服务端回落到 `index.html`, API 路径仍返回 JSON 404.
- 账号和文件夹可以出现在页面路径中, 但只定位到文件夹. 打开文件夹页面只加载列表和元数据, 不自动选中或拉取邮件正文. 具体邮件 ID 不写入路径参数或查询参数, 不在 URL 中定位具体邮件, 避免打开链接就触发正文或附件拉取; 邮件选择只保存在页面状态.
- 搜索条件不写入 URL, 需要跨页面携带时使用 `sessionStorage`; 写邮件草稿 ID 使用 `window.name`.
- 服务端状态只放 TanStack Query; Zustand 只放 UI 状态.
- 组件不直接 `fetch`, 统一使用 `lib/api/client.ts`.
- 接口类型来自生成的 `schema.d.ts`, 不手写重复类型.
- 乐观更新必须有失败回滚和用户可见提示.
- HTML 邮件必须经 DOMPurify 清洗并放入不带 `allow-scripts` 的 sandbox iframe.
- 默认阻止远程图片和外链样式, 链接必须带 `rel="noopener noreferrer"`; 邮件自带的内联 `<style>` 块可以保留, 但必须移除 `@import`, 远程 `url()` 和可执行 CSS.
- 阻止远程图片时, 1x1 或 2x2 及更小的跟踪像素直接移除, 不显示占位符也不计入阻止数量.
- 被阻止的小图标 (宽高都不超过 24px) 直接隐藏但仍计入阻止数量; 其他占位符按原始尺寸显示.
- HTTPS 页面必须把 `http://` 图片地址升级为 `https://`, 避免混合内容拦截; HTTP 页面保持原地址.
- 表格的 `align`, `valign`, `width`, `height`, `cellpadding`, `cellspacing`, `border` 和 `bgcolor` 属于邮件布局, 清洗时必须保留.
- 表格的 `background` 图片地址改写为 `data-background-src`, 用户放行远程图片后再恢复, 未放行时不得请求.
- 邮件 `<html>` 和 `<body>` 上的背景声明必须提升到 iframe 根元素, 避免整页背景丢失.
- 用户可见文案走 i18n, 同时维护 `en` 和 `zh-CN`.
- 操作结果统一使用右下角通知条, 不在按钮旁或表单内显示成功/失败文字; 折叠时只显示最新一条, 悬浮展开最多 4 条, 最新的在最上面.
- 主题必须在首屏渲染前从本地 UI 状态应用, 暗色模式刷新不得先闪亮色.
- 交互元素必须具备可访问名称和键盘操作路径.
- 侧边栏使用可拖动宽度的左栏: 默认 `232px`, 最小 `180px`, 没有品牌标题, 全局视图常驻显示, 默认折叠的邮箱账号列表, 展开账号后的"其他", 激活态 `bg-accent`, 移动端 Sheet 抽屉; 左下角只显示同步状态, 设置只能从右上角账号菜单进入.
- 侧边栏, 邮件列表和阅读器三段都可通过竖直分隔条调整宽度; 最小宽度分别为 `180px`, `320px`, `360px`, 拖动时不得挤压其他段低于最小值, 宽度持久化在本地 UI 状态.
- 顶栏只覆盖内容区: 左侧写邮件; 右侧搜索, 刷新, 任务环, 明暗切换和登录用户头像; 侧边栏从顶部到底部独立.
- 失败任务进入任务环, 用红色弧段表示; 面板提供重试, 打开草稿, 打开账号设置和忽略, 不单独做通知中心.
- 任务色: 同步和发送用绿色, 正文/标记/归档拉取用青色, 索引用紫色, 清理任务用灰蓝色, 失败用红色.
- 右上角头像是登录用户, 不是邮箱账号; 悬停或点击展开紧凑卡片, 包含资料, 设置和退出. 邮箱账号通过侧边栏账号列表切换.
- 阅读器顶部左侧是回复, 转发和标记, 右侧是下载原始邮件, 本地归档和删除; 全部使用图标按钮, 带 `aria-label` 和 tooltip, 不放在全局顶栏.
- 设置以居中浮层对话框打开, 覆盖在主应用壳之上, 主壳的侧边栏和顶栏保持挂载; 每个分组是固定路径, 共用左侧导航, 搜索, 关闭按钮和 `Escape` 返回.
- 写邮件默认是应用内浮层; 设置中可改为浏览器新窗口; 左上角发送按钮打开身份选择, 发送请求携带 `identityAccountId` 和 `identityEmail`.
- 新窗口模式打开 `/compose`, 草稿 ID 通过 `window.name` 传递, 跨窗口同步使用 `BroadcastChannel`; 不使用路径参数或查询参数, 同一草稿已有窗口时聚焦已有窗口.
- 任务环按任务类型分色, 按工作量占比分段, 段内按完成百分比填充; 无任务时为白色整环.
- 任务环颜色只用 CSS token, 不在组件写死颜色; 悬停和键盘聚焦都要展开任务面板, `Esc` 关闭, 并支持 `prefers-reduced-motion`.
- 任务面板必须同时显示任务类型文字, 不能让颜色成为唯一信息; 环和面板要有 `role="progressbar"` 与 `aria-live`.
- 任务数据只来自 `GET /tasks` 和 `task.updated`, 不在前端重新计算业务总量.
- 搜索支持标题/全部模式和账号, 文件夹, 收件人, 发件人, 附件组合条件; 条件之间是 AND, 筛选状态放 Zustand 和 `sessionStorage`, 不写入 URL.
- 收件人同时匹配 To, Cc, Bcc; 搜索结果包含本地归档并显示标识.
- 不引入第二套组件库, 新组件优先放到 `components/ui` 或对应 feature.

## 7. 测试

- 修复缺陷先写失败用例, 再修复.
- 测试行为, 边界, 状态转换和错误路径, 不测试实现细节与转发调用.
- 仓储测试只使用 PostgreSQL, 不引入 SQLite 测试矩阵.
- Bleve 测试覆盖映射, CJK 分析器, 分页, 幂等写入, 删除和重建.
- 任务环测试覆盖多任务分段, 空状态, 悬停和键盘展开, `Esc` 关闭, 减少动效, 以及任务完成后的移除.
- 写邮件测试覆盖身份选择, 默认身份, 签名切换, 草稿恢复和发送请求中的 `identityAccountId` 与 `identityEmail`.
- 发送流程测试覆盖失败草稿保留, 成功后移入已发送, 取消保留草稿, 以及全程不删除草稿.
- 任务环失败态测试覆盖发送, 同步, 认证和索引失败显示红色, 失败不自动消失, 重试和忽略.
- 设置页测试覆盖独立布局, 分组路径和深链接, 关闭返回和保存反馈.
- 资源检测和并发测试覆盖 cgroup v1/v2, Compose 限制, 回退值, 手动覆盖和内存预算.
- IMAP, POP3 和 SMTP 使用本地模拟服务, 测试不依赖真实邮箱账号.
- 异步测试使用条件等待, 不使用固定 `sleep`.
- 不为了通过测试而放宽断言或删除既有契约测试.

## 8. 文档

- 修改接口, 表结构, 环境变量, 部署方式或业务规则时, 同一提交更新相关文档.
- `README.md` 与 `README_CN.md` 保持内容对应, 不把 README 当变更日志.
- 移除功能时同步删除文档描述, 不保留 "已移除" 之类的历史段落.
- 发布后更新 `.release-notes.md`, 并同步到 GitHub Release, 保留自动生成的 changelog 链接.

## 9. Git

- 提交身份: `YeqingKy <me@yeqing.im>`.
- 使用 Conventional Commits: `feat`, `fix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`.
- 不提交 `.env`, 数据库文件, blob, 构建产物, 真实凭据或测试邮箱数据.
- 不执行 `git reset --hard`, `git clean -fd` 或强推, 除非用户明确要求.
- 发布只走 GitHub Actions; 前端和后端使用同一版本号, 各自标签生成 Release, 多平台二进制和 Docker 镜像.
- 后端必须提供 `mailquay` (含前端) 和 `mailquay-core` (仅 API) 两种构建; 含前端版在编译期嵌入前端, 不得用运行时开关伪装.
- 后端发布在 `.github/workflows/release.yml` 内先按同名 tag 构建一次前端, 再并行编译 core 与 full; 全部成功后才创建 Release, 不维护单独的前端版本文件.
- 后端 PR 和 `main` 的 CI 直接克隆前端仓库 `main` 最新提交验证内嵌构建, 不要求前端标签存在.
- CI 在 `main` 提交和同仓库 PR 上推送 commit 级镜像 `<sha7>` 与 `<sha7>-core`; fork PR 只构建不推送; `latest` 只在标签发布时更新.
- 后端二进制覆盖 linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64; Docker 只做 linux/amd64 和 linux/arm64.
- 代码和产物文件名使用 `darwin`, 用户界面和发布说明写 `macOS`; 不手工创建 Release 绕过工作流.
- 工作流的每个 job 必须设置 `timeout-minutes`; 联网步骤必须使用命令级 `timeout` 并最多重试 3 次; 平台构建矩阵使用 `fail-fast: false`.
- Docker 镜像统一发布到 `ghcr.io/mailquay/mailquay`: 含前端版使用 `vX.Y.Z`, `latest` 和 7 位短提交哈希标签; 只含后端版在标签末尾追加 `-core`.
