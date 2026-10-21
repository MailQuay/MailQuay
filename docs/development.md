# 开发文档

本文覆盖本地环境, 配置, 数据库迁移, 测试, 代码规范和发布流程. 系统设计见 [architecture.md](architecture.md), 目录与接口见 [project-structure.md](project-structure.md).

## 1. 前置工具

| 工具 | 版本 | 用途 |
| --- | --- | --- |
| Go | 1.27+ | 后端编译与测试 |
| Node.js | 24+ | 前端构建 |
| pnpm | 12+ | 前端依赖管理 |
| Docker Engine + Compose v2 | 当前稳定版 | 集成环境, 镜像构建 |
| PostgreSQL | 18 | 唯一数据库 |
| openssl | 任意 | 生成主密钥 |

本地开发同样需要 PostgreSQL. 搜索索引由 Bleve 内嵌在 Go 进程中, 不需要额外服务.

## 2. 获取代码

```sh
git clone git@github.com:MailQuay/MailQuay.git
git clone git@github.com:MailQuay/frontend.git
```

两个目录必须并列, 便于 Compose 与联调:

```
workspace/
├── MailQuay/
└── frontend/
```

## 3. 后端本地启动

### 3.1 PostgreSQL

```sh
docker run --rm -d --name mailquay-pg \
  -p 5432:5432 \
  -e POSTGRES_USER=mailquay \
  -e POSTGRES_PASSWORD=mailquay \
  -e POSTGRES_DB=mailquay \
  postgres:18-alpine
```

```sh
cd MailQuay
cp .env.example .env
openssl rand -base64 32
```

把输出写入 `.env` 的 `MAILQUAY_SECRET_KEY`, 确认连接串后执行:

```sh
go run ./cmd/mailquay migrate up
go run ./cmd/mailquay serve
```

默认监听 `:8080`, blob, 正文文本缓存和 Bleve 索引位于 `./data`. 首次启动会在日志输出一次性 setup token, 前端 `/setup` 页面需要填写该 token.

### 3.2 搜索索引

Bleve 索引默认位于 `./data/search/mailquay.bleve`, 首次启动自动创建. 重建:

```sh
go run ./cmd/mailquay search reindex --all
```

索引损坏, 映射版本变化或从备份恢复后执行重建. 重建期间搜索可能不可用, 邮件读写和同步不受影响.

### 3.3 子命令

| 命令 | 作用 |
| --- | --- |
| `mailquay serve` | 启动 HTTP 服务, 同步 worker 和发件 worker |
| `mailquay migrate up` | 应用全部迁移 |
| `mailquay migrate down 1` | 回滚一个迁移 |
| `mailquay migrate status` | 查看迁移状态 |
| `mailquay healthcheck` | 探测本地 `/healthz`, 供容器健康检查使用 |
| `mailquay search reindex --all` | 清空并重建 Bleve 索引 |
| `mailquay search reindex --since 2026-01-01` | 只重建指定时间之后的邮件 |
| `mailquay version` | 输出版本和提交号 |

## 4. 环境变量

### 4.1 后端

| 变量 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `MAILQUAY_LISTEN_ADDR` | 否 | `:8080` | HTTP 监听地址 |
| `MAILQUAY_PUBLIC_URL` | 否 | 空 | 绝对公开地址, 用于 OAuth 回调和 Cookie 判断 |
| `MAILQUAY_CORS_ORIGINS` | 否 | 空 | 允许的跨源来源, 逗号分隔 |
| `MAILQUAY_INSECURE_ALLOW` | 否 | 空 | 免除非安全传输提醒的域名或 CIDR, 逗号分隔 |
| `MAILQUAY_DATABASE_URL` | 是 | 空 | PostgreSQL 连接串 |
| `MAILQUAY_DATA_DIR` | 否 | `./data` | blob, 正文文本缓存, Bleve 索引与临时文件目录 |
| `MAILQUAY_SEARCH_PATH` | 否 | `$MAILQUAY_DATA_DIR/search` | Bleve 索引目录 |
| `MAILQUAY_SECRET_KEY` | 是 | 无 | 32 字节随机值的 base64, 用于字段加密 |
| `MAILQUAY_SETUP_TOKEN` | 否 | 首次启动随机生成 | 首次创建所有者账号的一次性 token |
| `MAILQUAY_LOG_LEVEL` | 否 | `info` | 启动日志级别, 之后可在网页中修改 |
| `MAILQUAY_LOG_FORMAT` | 否 | `text` | `text` 或 `json` |
| `MAILQUAY_TRUSTED_PROXIES` | 否 | 空 | 可信代理 CIDR, 逗号分隔 |

除以上启动参数外, 会话有效期, 同步与 POP3 轮询间隔, 正文预取上限, 附件上限, OAuth 应用凭据, 日志级别和并发参数都保存在 `settings` 表, 通过设置页修改, 修改后立即影响新任务. 公开地址, CORS, 可信代理和非安全传输豁免只通过环境变量配置, 修改后需要重启.

启动时校验规则:

- `MAILQUAY_SECRET_KEY` 解码后必须正好 32 字节.
- `MAILQUAY_DATABASE_URL` 不能为空, 启动时必须能建立连接.
- `MAILQUAY_PUBLIC_URL` 配置后必须是绝对 URL, 且与 OAuth 回调登记一致.
- `MAILQUAY_SEARCH_PATH` 必须可写; 启动时获取 Bleve 索引锁失败则拒绝启动.
- 生产环境不允许 `MAILQUAY_LOG_LEVEL=debug` 输出请求体.

### 4.2 前端

| 变量 | 阶段 | 默认 | 说明 |
| --- | --- | --- | --- |
| `VITE_API_BASE_URL` | 构建 | 空 | 后端地址, 空表示同源 `/api/v1` |
| `MAILQUAY_API_BASE_URL` | 运行时 | 空 | 容器启动时写入 `/config.js`, 优先级最高 |
| `MAILQUAY_WEB_PORT` | 运行时 | `8080` | Compose 映射端口, 仅部署使用 |

优先级: 运行时配置 > 构建时配置 > 同源. 前后端同源部署时保持为空, 不需要 CORS.

## 5. 数据库迁移

- 迁移文件位于 `migrations/postgres`, 使用 goose 格式与编号.
- 只维护 PostgreSQL 一套迁移, 不引入方言分支.
- 已合并的迁移不得修改; 修复使用新迁移.
- 迁移必须向后兼容一个版本, 支持先迁移后滚动升级.
- 大表加列使用可空列或带默认值的在线操作, 避免长时间锁表.
- 索引创建与数据回填分开提交, 回填脚本必须可重入.

新增字段的检查清单:

1. `migrations/postgres` 的 `up` 和 `down`.
2. `sqlc` 查询与仓储接口.
3. API 合同和 DTO.
4. 前端类型与使用点.
5. `docs/project-structure.md` 数据模型表.
6. 如果改动搜索字段, 提升 `search_mapping_version` 并安排重建.
7. 迁移测试: 空库升级, 带数据升级, 回滚.

## 6. 前端本地启动

```sh
cd frontend
pnpm install
cp .env.example .env.local
pnpm dev
```

`vite.config.ts` 以单页模式构建, 只产出一个 `index.html`, 前端路由由 `react-router` 处理. 开发服务器直接提供 `/`, `/unread`, `/accounts/{accountId}/mailboxes/{mailboxId}`, `/settings/general`, `/login`, `/setup` 和 `/compose` 等路径, 未命中的非资源路径回落到 `index.html`. 默认把 `/api` 代理到 `http://127.0.0.1:8080`, 因此 `.env.local` 可以留空. 前后端分离调试时设置:

```dotenv
VITE_API_BASE_URL=http://127.0.0.1:8080/api/v1
```

此时在后端设置页的服务器分组中添加 `http://127.0.0.1:5173` 到允许来源, 并且 Cookie 策略随部署方式调整.

常用命令:

| 命令 | 作用 |
| --- | --- |
| `pnpm dev` | Vite 开发服务器 |
| `pnpm build` | 类型检查并生成静态资源 |
| `pnpm preview` | 预览构建结果 |
| `pnpm lint` | ESLint |
| `pnpm typecheck` | TypeScript 检查 |
| `pnpm test` | Vitest 单元测试 |
| `pnpm e2e` | Playwright 端到端测试 |
| `pnpm generate:api` | 从 `../MailQuay/api/openapi.yaml` 生成类型 |

## 7. API 合同

1. 先修改 `MailQuay/api/openapi.yaml`.
2. 后端执行 `make generate`, 重新生成 `internal/httpapi/oapi` (依赖 `sqlc` 和 `oapi-codegen`).
3. 前端执行 `pnpm generate:api`, 重新生成 `src/lib/api/schema.d.ts`.
4. 生成文件不手工修改, 差异随合同一起提交.
5. 破坏性变更必须升级路径前缀或提供兼容期, 不能直接改字段语义.

合同检查:

```sh
make api-lint
```

CI 会校验生成文件与合同一致, 不一致直接失败.

## 8. 测试

### 8.1 后端

```sh
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
```

集成测试:

```sh
MAILQUAY_TEST_DATABASE_URL='postgres://mailquay:mailquay@127.0.0.1:5432/mailquay_test?sslmode=disable' \
  go test -tags=integration ./test/integration/...
```

集成测试要求:

- 仓储测试只针对 PostgreSQL, 使用独立测试库和事务回滚.
- Bleve 测试覆盖字段映射, CJK 分析器, `SearchAfter` 分页, 重复写入幂等, 删除, 从 blob 重建, 以及索引损坏时的降级.
- IMAP 使用 `go-imap` 的服务端包或 GreenMail 容器模拟, 覆盖初次同步, 增量, 删除, 标志和 UIDVALIDITY 变化.
- 初始同步覆盖 `all`, `1d`, `3d`, `1w`, `1m`, `1y` 六种范围, 断言 cutoff 过滤, 后台任务进度, 以及中断后继续.
- 下载范围覆盖 `headers`, `full_no_attachments`, `full_with_attachments`, 断言仅头模式不缓存正文, 无附件模式按需拉附件, 完整模式缓存附件.
- 全部视图测试覆盖排除已发送, 草稿和垃圾箱, 并按账号加 Message-ID 去重.
- POP3 使用 GreenMail 或 Dovecot 测试实例, 覆盖 UIDL 增量, TOP 缺失, RETR, DELE 与 QUIT 提交, 以及轮询去重.
- SMTP 使用 Mailpit 或本地 sink 捕获邮件, 断言 MIME 结构, 附件和重试.
- 登录测试覆盖用户名和密码, 不涉及邮箱绑定或邮件验证码, 以及后续 OAuth2 登录的预留边界.
- 传输安全测试覆盖可信代理的 `X-Forwarded-Proto`, 伪造头被忽略, 默认 loopback/私网豁免, 额外 `MAILQUAY_INSECURE_ALLOW` 域名/CIDR, 以及红色提醒条不渲染/渲染.
- 迁移测试覆盖空库, 历史数据和回滚.
- 账号连接测试覆盖非标准端口和三种传输安全模式, 断言实际连接的是用户填写的端口.
- 静态资源测试覆盖 core 不带前端资源且根路径返回 404, full 内嵌前端并对客户端路由回落到 `index.html`, 缺失的 `assets` 和带扩展名文件返回 404, 非 GET/HEAD 返回 405, 两种构建 API 行为一致.
- 范围变更测试覆盖设置默认值只影响新账号, 扩大范围补拉, 缩小范围删除本地元数据和索引, 缩小下载范围删除正文/附件缓存, 以及标记和本地归档豁免.
- 去重测试覆盖服务商消息 ID, Message-ID, 原文哈希和 UID 的回退顺序, 代表副本优先级, 文件夹视图不去重, 以及已读/标记同步到所有副本.
- 账号测试覆盖密码/OAuth secret 不返回, `host:port` 地址解析, IPv6 地址, 端口范围校验, 连接测试只使用已保存值, 以及删除账号后的清理.
- 身份测试覆盖默认身份唯一, 账号 SMTP 发信, Reply-To, 固定 Bcc, Organization, 签名插入时机, 任意身份发送, 回复默认使用原邮件所属账号的默认身份, 以及删号后回退当前账号默认身份.
- 草稿测试覆盖 2 秒防抖, 输入过程中不写上游, `pull` 模式低频拉取, `mirror` 模式停止输入后才推送, 发送后移动或保留远端草稿, 以及冲突副本.
- 发送流程测试覆盖失败草稿原地保留, 成功后移入已发送, 取消发送保留草稿, 以及全程不删除草稿.
- 定时发送测试覆盖到期前不连接 SMTP, 到期后转 pending, 重启恢复, 修改时间, 编辑草稿取消定时, 以及取消发送.
- 重试测试覆盖 `send_retry_limit` 默认 3, 修改后生效, 达到上限进入失败任务环, 以及永久错误不重试.
- 任务环失败态测试覆盖发送失败, 同步失败, 认证失败和索引失败显示红色, 失败不自动消失, 以及重试和忽略.
- 标记测试覆盖完整正文和附件落盘, 标记任务失败进入任务环, 删除账号保留标记邮件, 非标记邮件和索引异步清理, 以及关闭保留开关后的行为.
- 本地归档测试覆盖完整副本落盘, 重复归档幂等, 远端删除和删除账号后仍可读, 以及不进入"全部".
- 删除测试覆盖默认移动到 Junk, 本地队列保留天数, 到期永久删除并同步, 服务商提前清理, 以及失败进入任务环.
- 资源检测测试覆盖 cgroup v2 `cpu.max` / `memory.max`, cgroup v1 quota / limit, cpuset, 无限制回退和无效值.
- 并发计算测试覆盖 Compose 资源限制映射后的自动值, 手动值范围校验, 内存预算收敛, 以及 Bleve 写入始终保持单 writer.

### 8.2 前端

```sh
pnpm lint
pnpm typecheck
pnpm test
pnpm e2e
```

- 单元测试覆盖纯函数: HTML 清洗, 地址解析, 时间格式, 游标处理, 任务进度聚合.
- 组件测试使用 Testing Library, 覆盖任务环分段, 空状态, 悬停和键盘展开, `Esc` 关闭, 以及 `prefers-reduced-motion`.
- 搜索页覆盖标题/全部模式, 账号/文件夹/收件人/发件人/附件组合, 搜索条件在页面状态中的保持, 以及本地归档结果标识.
- 写邮件组件覆盖身份选择, 默认身份, 签名切换, 草稿恢复, 以及发送请求携带 `identityAccountId` 和 `identityEmail`.
- 写邮件窗口覆盖默认浮层, 设置为新窗口后的 `/compose` 通过 `window.name` 恢复草稿, 同一草稿聚焦已有窗口, 以及关闭前保存.
- 阅读器顶部左侧是回复和转发, 右侧是标记, 本地归档和删除, 全部为图标按钮.
- 设置页面覆盖独立布局, 分组路径和深链接, 关闭返回和保存反馈.
- Playwright 使用 mock API 覆盖登录, 列表, 阅读, 写邮件主流程, 设置跳转, 以及多任务同时运行时的任务面板.
- 不测试实现细节, 不复制后端校验逻辑.

### 8.3 断言原则

- 测试行为, 边界和状态转换, 不测试常量或转发调用.
- 修复缺陷时先写复现用例, 再修复.
- 异步测试使用显式等待条件, 不使用固定 `sleep`.
- 测试必须可并行, 不共享可变全局状态.

## 9. 代码规范

### 9.1 Go

- 使用 `gofmt` 和 `goimports`.
- 错误使用 `%w` 包装, 对外错误由 `httpapi` 映射.
- 所有 I/O 和网络调用传入 `context.Context`.
- 日志使用 `log/slog`, 字段结构化, 不拼接正文和凭据.
- 不使用包级可变状态; 依赖显式注入.
- 接口定义在使用方, 不提前抽象只有一个实现的接口.
- 并发使用有界 worker, 每个 goroutine 有退出路径.

### 9.2 TypeScript 与 React

- `strict: true`, 不使用 `any`; 未知输入先经 `unknown` 收窄.
- 组件函数不超过约 200 行, 超出时按职责拆分.
- 服务端状态只放 TanStack Query, UI 状态只放 Zustand.
- 组件不直接调用 `fetch`, 统一使用 `lib/api/client.ts`.
- 样式使用 Tailwind 工具类和主题变量, 不写内联颜色.
- 交互元素必须有可访问名称和键盘路径.
- 用户可见文案全部走 i18n, 不硬编码在组件中.

### 9.3 样式

- 设计令牌定义在 `src/styles/index.css`, 使用 CSS 变量.
- 明暗主题通过 `.dark` 类切换, 不复制两套组件样式.
- 侧边栏尺寸使用 `--sidebar-width`, 不散落魔法数字.
- 侧边栏使用固定宽度, 顶部品牌区, 分组折叠, 激活态 `bg-accent`, 移动端 Sheet.

## 10. Git 工作流

- 主干分支 `main`, 保持可构建.
- 功能分支短生命周期, 合并前 squash 或整理为清晰提交.
- 提交信息使用 Conventional Commits:

```
feat(sync): add UIDVALIDITY recovery
fix(compose): keep attachment order on retry
docs(architecture): clarify SSE replay window
```

- 允许的类型: `feat`, `fix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`.
- 提交身份: `YeqingKy <me@yeqing.im>`.
- 不提交 `.env`, 数据库文件, blob 目录, token 或任何真实邮箱凭据.

## 11. 发布

发布由 GitHub Actions 完成, 不手工上传发行版.

### 11.1 版本与发布顺序

- 使用 SemVer, 标签格式 `vX.Y.Z`; 前端和后端使用完全相同的版本号, 不维护单独的前端版本文件.
- 前端先打标签, 前端工作流执行检查, 构建并生成前端 Release; 后端随后打同名标签.
- 后端 `release.yml` 在同一个工作流内先按同名 tag 构建一次前端, 再并行编译 core 和 full 两种形态.
- 所有构建全部成功后, 同一个工作流的 `publish` job 才创建后端 Release; 任一条失败都不发布, 修复后重新运行整个工作流.

### 11.2 两种后端形态

| 形态 | 产物 | 内容 | 启动方式 |
| --- | --- | --- | --- |
| 含前端版 | `mailquay` | 后端 API 加已构建前端静态资源 | 单个 exe 或 Compose 同时提供前后端 |
| 不含前端版 | `mailquay-core` | 仅后端 API | 单独运行, 前端由外部 nginx 或静态托管提供 |

- 含前端版在编译时通过 Go `embed` 打入 `web/dist`, 使用构建标签 `full`; 不是运行时开关.
- 不含前端版不编译静态资源包, 不注册静态文件路由; 不是"监听端口但不服务页面".
- 含前端版同一端口提供 `/api/v1` 和静态资源; 静态文件命中时直接返回, 其余 GET/HEAD 路径回落到 `index.html` 交给客户端路由, `/api` 路径仍返回 JSON 404.
- 不含前端版只提供 API, 根路径返回 404 或 JSON 提示.
- 含前端版和不含前端版使用同一套业务代码, 差异只在静态资源嵌入和路由注册.

### 11.3 平台矩阵

| 平台 | GOOS | GOARCH | 支持级别 |
| --- | --- | --- | --- |
| Linux x64 | `linux` | `amd64` | 必须 |
| Linux ARM64 | `linux` | `arm64` | 必须 |
| macOS Intel | `darwin` | `amd64` | 推荐 |
| macOS Apple Silicon | `darwin` | `arm64` | 必须 |
| Windows x64 | `windows` | `amd64` | 必须 |
| Windows ARM64 | `windows` | `arm64` | 可选 |

- 代码, 工作流和产物文件名统一使用 `darwin`; 发布说明和用户界面写 `macOS`.
- 两种形态都覆盖上述平台.
- 产物命名: `mailquay_<version>_<goos>_<goarch>.tar.gz` 和 `mailquay-core_...`; Windows 使用 `.zip`.
- 使用 `CGO_ENABLED=0` 生成静态二进制, 通过 `-ldflags` 注入版本和提交号.
- 同时生成 `checksums.txt`.
- macOS 二进制暂不签名和公证; Windows 不提供安装器, 只提供压缩包.

### 11.4 Docker 镜像与 Compose

- 含前端镜像: `ghcr.io/mailquay/mailquay:vX.Y.Z`, `ghcr.io/mailquay/mailquay:latest`, `ghcr.io/mailquay/mailquay:<短提交哈希>`.
- 不含前端镜像: 同一镜像仓库, 标签追加 `-core`: `ghcr.io/mailquay/mailquay:vX.Y.Z-core`, `ghcr.io/mailquay/mailquay:latest-core`, `ghcr.io/mailquay/mailquay:<短提交哈希>-core`.
- 两个镜像都支持 `linux/amd64` 和 `linux/arm64` 多架构 manifest.
- 前端不提供容器镜像. 静态构建产物部署到 Cloudflare Pages 等静态平台, 或由含前端版后端内嵌.
- `docker-compose.yml`: 使用 `ghcr.io/mailquay/mailquay` 发布镜像, 前后端同端口提供; 状态目录使用仓库内挂载 `./data` (uid 100) 和 `./pg_data` (uid 70, 权限 750).
- `docker-compose-dev.yml`: 本地构建 `full` 镜像, 构建时从 `../frontend` 编译前端并内嵌; 单个 `mailquay` 容器同时提供前后端, 容器内不运行 Node 进程.

本地开发栈:

```sh
docker compose -f docker-compose-dev.yml up -d --build
```

- 前端和后端: `http://localhost:8080`, 同一容器提供页面和 `/api/v1`, 容器内先执行迁移再启动.
- 前端在构建镜像时编译; 修改 `frontend` 或后端代码后重新执行带 `--build` 的启动命令.
- 数据目录使用宿主机绑定挂载: 数据库为 `../pg_data` (uid 70, 权限 750), 运行数据为 `../data/dev` (uid 100). 首次启动前创建并 chown, 否则容器用户无法写入.
- 修改前端依赖后重新构建镜像, 不需要在宿主机执行 `pnpm install`.
- 默认数据库密码和主密钥只用于本地; 暴露到其他主机前必须覆盖 `POSTGRES_PASSWORD` 和 `MAILQUAY_SECRET_KEY`.
- Windows 和 macOS 用户使用独立二进制, 不使用容器.
- Compose 示例固定到具体版本, 不覆盖用户已有 `.env`.

### 11.5 工作流

- 后端: `.github/workflows/ci.yml`, `.github/workflows/release.yml`; 发布工作流按平台矩阵直接编译并上传产物.
- 前端: `.github/workflows/ci.yml`, `.github/workflows/release.yml`.
- 前端 `ci.yml` 在 PR 和 `main` 上执行检查并上传 `dist` artifact, 同时作为可复用工作流; 前端 `release.yml` 在标签上复用 `ci.yml`, 打包 `frontend-dist-<tag>.zip`, 生成校验文件并创建 Release.
- 后端 `ci.yml` 在 `main` 提交和同仓库 PR 上构建并推送 commit 级镜像: `ghcr.io/mailquay/mailquay:<7 位短提交>` 和 `:<7 位短提交>-core`; fork PR 只构建不推送, PR 使用 head commit 的短哈希.
- 镜像构建使用 BuildKit 多架构和 GHA 缓存; Dockerfile 的 Go 编译阶段按 `TARGETOS`/`TARGETARCH` 交叉编译, 只在运行阶段使用目标架构基础镜像.
- 后端 `ci.yml` 的 full job 直接克隆前端仓库 `main` 的最新提交, 构建 `dist` 后编译内嵌二进制并上传共享 artifact, 不要求前端已有标签.
- 后端 `release.yml` 的 full 分支按同名前端标签克隆; `binaries` 矩阵每个平台同时编译 `mailquay-core` 和 `mailquay`; `images` 矩阵并行构建 `full-prebuilt` 和 `core` 镜像.
- `binaries` 与 `images` 全部完成后, `publish` job 合并全部产物, 生成 `checksums.txt`, 创建或更新 Release.
- 前端标签缺失时 full 分支在三次 clone 重试后失败, 后端不创建 Release; 先推前端标签, 再重新运行后端发布.
- 镜像标签: 含前端版 `vX.Y.Z`, `latest` 和 7 位短提交哈希; 仅后端版在标签末尾追加 `-core`.
- 所有工作流的 job 必须设置 `timeout-minutes`: 后端 CI 40/30/45, 发布前端 20, 二进制矩阵 30, 镜像矩阵 45, 发布上传 10, 前端 CI 20.
- 联网步骤必须加命令级 `timeout` 和最多 3 次重试: 前端 clone 300 秒, `corepack pnpm install` 900 秒, `corepack pnpm build` 600 秒.
- 平台矩阵使用 `fail-fast: false`, 单个平台失败不取消其他平台产物.
- 前端依赖安装统一由 `corepack` 读取 `package.json` 的 `packageManager` 决定 pnpm 版本, 工作流不固定 pnpm 版本, 避免 `ERR_PNPM_BAD_PM_VERSION`.
- 标签工作流必须等测试和构建全部成功后再创建或更新 Release.
- 发布工作流失败时修复并重新运行, 不手工创建 Release 绕过工作流.

### 11.6 发布说明

1. 前端打 `vX.Y.Z` 标签, 等前端 Release 和静态产物完成.
2. 后端打同名 `vX.Y.Z` 标签; full 分支按该标签克隆前端仓库.
3. 等待后端 core 和 full 两条分支; 两者都成功后 Release 自动创建.
4. 发布说明写入本地 `.release-notes.md`, 再同步到 GitHub Release.
5. 保留 GitHub 自动生成的 `Full Changelog: <previous>...<current>` 链接.

## 12. 故障排查

| 现象 | 排查 |
| --- | --- |
| SSE 没有事件 | 反向代理是否关闭缓冲, `proxy_read_timeout` 是否大于心跳间隔, 是否被 HTTP/2 中间层截断 |
| 登录后立刻 401 | `MAILQUAY_PUBLIC_URL` 与访问地址是否一致, Cookie 是否被 `Secure` 拒绝 |
| OAuth2 服务商认证失败 | 是否使用 OAuth2, 服务商是否启用 IMAP/POP3, scope 是否包含邮件权限 |
| OAuth 回调报 state 错误 | 回调地址是否登记一致, 系统时间是否准确, `oauth_states` 是否过期 |
| 某个文件夹反复全量同步 | 检查 `UIDVALIDITY` 是否变化, 服务端是否每次重建 UID |
| POP3 看不到已读/标记变化 | 预期行为, POP3 没有服务端标志, 这些状态只保存在 MailQuay |
| POP3 删除后服务器仍有邮件 | 默认只删本地; 确认已开启服务器删除, 且会话执行了 `QUIT` |
| 同一邮箱出现两套邮件 | 检查是否同时添加了 IMAP 和 POP3 账号, 两者数据不会自动合并 |
| 发件身份用了错误 SMTP | 身份使用所属账号的 SMTP; 检查账号 `smtp_addr` 和认证信息, 以及服务商是否允许该 From 地址 |
| 已发送出现重复 | 检查 `sent_copy_mode`; 服务商已自动复制时不要再用 `append` |
| 草稿在其他客户端看不到 | 预期行为; 草稿只存在 MailQuay, 不镜像服务器 Drafts |
| 全部视图出现重复邮件 | 检查去重键是否使用账号加 Message-ID, 以及是否错误同步了服务商 All Mail |
| POP3 新邮件延迟 | 轮询模式, 检查设置页的 POP3 轮询间隔; POP3 没有 IDLE |
| 任务环一直是白色 | 检查 `tasks` 是否有活动记录, SSE 是否收到 `task.updated`, 初始同步是否已启动 |
| 任务环进度不动 | 查看任务 `done/total` 是否更新, worker 是否卡在 IMAP/POP3 连接, 是否有索引锁冲突 |
| 任务面板不展开 | 检查 Popover 定位容器是否被 `overflow` 裁剪, 键盘焦点是否进入环, 以及 `Esc` 状态 |
| 自动并发不符合容器限制 | 检查 cgroup v1/v2 路径, Compose `deploy.resources.limits` 是否生效, 以及进程是否在宿主 cgroup 而不是容器内 |
| 同步或索引占满内存 | 降低 `fetch_concurrency` / `index_batch_size`, 或提高容器内存限制; 查看启动日志中的检测值和内存预算 |
| 修改并发后不生效 | 确认保存的是手动模式; 正在运行的任务继续使用旧值, 新任务才使用新值 |
| 搜索返回 `search_unavailable` | Bleve 索引锁是否被另一个进程持有, 索引目录权限是否可写, 进程是否优雅关闭 |
| 搜索索引落后或缺少新邮件 | 检查 `search_jobs` 积压和失败记录, 必要时执行 `mailquay search reindex --all` |
| 中文搜索命中少 | 确认字段使用 `cjk` 分析器, 正文已拉取并入库, 查询没有错误使用 keyword 字段 |
| 索引目录损坏 | 停止进程, 删除索引目录, 重新执行 `search reindex --all` |
| 发件重复 | 进程在 SMTP `250` 后崩溃属于至少一次语义, 检查 outbox 状态并核对 Sent |
| 本地时间与邮件时间偏差 | 统一使用 UTC 存储, 展示层按用户时区转换 |
