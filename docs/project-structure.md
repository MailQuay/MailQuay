# 项目结构

本文定义两个仓库的目录布局, 模块职责, 数据模型, HTTP API 和前端布局. 设计理由见 [architecture.md](architecture.md), 开发流程见 [development.md](development.md).

## 1. 仓库划分

| 仓库 | 职责 | 产物 |
| --- | --- | --- |
| `MailQuay` | Go 后端, 数据库迁移, API 合同, 部署文件, 文档 | `ghcr.io/mailquay/mailquay` |
| `frontend` | React 单页前端, 静态资源 | Cloudflare Pages 等静态部署, 或由含前端版后端内嵌 |

前端仓库不保存后端文档. 两份仓库并列检出即可本地联调:

```
GitHub-MailQuay/
├── MailQuay/
└── frontend/
```

## 2. MailQuay 目录

以下为 M1 的目标布局. 尚未创建的文件按此结构落地, 不另起约定.

```
MailQuay/
├── AGENTS.md
├── README.md
├── README_CN.md
├── LICENSE
├── Makefile
├── go.mod
├── go.sum
├── .env.example
├── .editorconfig
├── .gitignore
├── .golangci.yml
├── .dockerignore
├── Dockerfile                       # core/full 多阶段目标
├── docker-compose.yml               # 发布镜像 (ghcr.io), ./data 与 ./pg_data 挂载
├── docker-compose-dev.yml           # 本地构建 full 镜像, 单容器提供前后端
├── cmd/
│   └── mailquay/
│       └── main.go                  # serve, migrate, search, healthcheck 子命令
├── internal/
│   ├── app/                         # 依赖装配, 启动顺序, 优雅退出
│   ├── config/                      # 环境变量解析与校验
│   ├── httpapi/
│   │   ├── router.go                # 路由注册
│   │   ├── middleware/              # 请求 ID, 日志, 恢复, CSRF, 限流, 安全头
│   │   ├── handlers/                # 按资源拆分的处理器
│   │   ├── dto/                     # 请求与响应结构, 错误映射
│   │   └── oapi/                    # oapi-codegen 生成代码, 不手改
│   ├── auth/                        # 会话, Argon2id, CSRF, OAuth2 流程
│   ├── account/                     # 邮箱账号, 地址解析, 凭据加密, 连接测试
│   ├── identity/                    # 发件身份, 默认身份, 签名
│   ├── mailbox/                     # 文件夹树, 角色映射, 计数
│   ├── message/                     # 元数据, 标志, 移动, 删除, 线程
│   ├── compose/                     # 草稿, 发信, outbox
│   ├── sync/                        # 调度器, 每账号 worker, 操作队列
│   ├── imapclient/                  # go-imap 封装, IDLE, QRESYNC, 重连
│   ├── pop3client/                  # POP3 协议实现, UIDL, TOP, RETR, DELE, 轮询
│   ├── smtpclient/                  # SMTP 提交, STARTTLS, XOAUTH2
│   ├── mime/                        # 解析, 正文提取, 附件, HTML 清洗
│   ├── storage/                     # 内容寻址 blob 与正文文本缓存
│   ├── store/                       # pgxpool, sqlc 查询, 事务
│   ├── search/                      # Bleve 索引, 查询解析, 重建
│   ├── events/                      # 事件总线, SSE hub, 回放缓冲
│   ├── webassets/                   # full 构建嵌入前端; core 构建空实现
│   ├── crypto/                      # AES-256-GCM, 主密钥加载
│   └── platform/                    # 日志, 时钟, ID, 重试, cgroup 资源检测
├── migrations/
│   └── postgres/                    # goose SQL 迁移
├── api/
│   ├── openapi.yaml                 # API 合同, 唯一事实来源
│   └── oapi-codegen.yaml
├── docs/
│   ├── architecture.md
│   ├── project-structure.md
│   ├── development.md
│   └── questions/                   # 待确认问题, 按编号分隔
├── test/
│   └── integration/                 # PostgreSQL, IMAP/POP3/SMTP 测试夹具
└── .github/
    └── workflows/
        ├── ci.yml                   # lint, test, core/full 构建
        └── release.yml              # core 与 full 并行构建, 全部成功后发布
```

### 2.1 后端模块

| 包 | 职责 | 允许依赖 |
| --- | --- | --- |
| `internal/app` | 组装配置, 存储, 服务, HTTP 路由和后台 worker; 处理信号退出 | 所有内部包 |
| `internal/config` | 读取环境变量, 设置默认值, 启动时校验 | `platform` |
| `internal/httpapi` | 路由, 中间件, 参数校验, 错误到 HTTP 的映射 | `auth`, `account`, `identity`, `mailbox`, `message`, `compose`, `search`, `events`, `store` |
| `internal/auth` | 密码哈希, 会话, CSRF, OAuth2 授权码流程 | `store`, `crypto`, `platform` |
| `internal/account` | 邮箱账号, 地址解析, 凭据加密, 连接测试, 同步策略 | `store`, `crypto`, `imapclient`, `pop3client`, `smtpclient`, `platform` |
| `internal/identity` | 发件身份, 默认身份, 签名, 使用账号 SMTP | `store`, `account` |
| `internal/mailbox` | 文件夹角色映射, 未读计数, 同步状态查询 | `store` |
| `internal/message` | 元数据查询, 标志更新, 移动, 删除, 线程归并 | `store`, `events` |
| `internal/compose` | 草稿版本, 身份解析, MIME 组装, outbox 状态机 | `store`, `identity`, `account`, `mime`, `smtpclient`, `imapclient`, `storage`, `events` |
| `internal/sync` | 账号调度, 初次与增量同步, 操作队列, 对账 | `imapclient`, `pop3client`, `mime`, `store`, `storage`, `events`, `search` |
| `internal/imapclient` | 连接管理, IDLE, FETCH, STORE, MOVE, APPEND | `crypto`, `platform` |
| `internal/pop3client` | 连接管理, STAT, UIDL, TOP, RETR, DELE | `crypto`, `platform` |
| `internal/smtpclient` | TLS 建连, 认证, 提交 | `crypto`, `platform` |
| `internal/mime` | 流式解析, 纯文本提取, 附件元数据, HTML 白名单清洗 | `platform` |
| `internal/storage` | SHA-256 内容寻址 blob, 正文文本缓存, 临时文件回收 | `platform` |
| `internal/store` | 仓储接口与 pgx/sqlc 实现, 事务, 迁移入口 | `platform` |
| `internal/search` | Bleve 映射, 索引任务, 查询解析, 高亮, 重建 | `store`, `mime`, `storage`, `platform` |
| `internal/events` | 进程内发布订阅, SSE 序列化, 环形回放 | `platform` |
| `internal/webassets` | `full` 构建嵌入前端静态资源; `core` 构建返回空实现 | 无 |
| `internal/crypto` | 主密钥解析, 字段加解密 | 无 |
| `internal/platform` | `slog`, 时钟接口, UUIDv7, 退避, cgroup v1/v2 CPU 与内存检测 | 无 |
| `internal/settings` | 设置读写, 默认值, 并发计算, 资源快照 | `store`, `platform` |

依赖方向必须保持单向: `httpapi` 和 `sync` 是入口层, `store`, `imapclient`, `pop3client`, `smtpclient`, `storage` 是基础设施层, 业务包不反向依赖 HTTP 类型.

### 2.2 数据访问

- 仓储接口定义在 `internal/store`, 业务包只依赖接口.
- 连接池使用 `pgx/v5` 的 `pgxpool`; 查询由 `sqlc` 生成, 不手写重复的扫描代码.
- PostgreSQL 使用 `JSONB`, `TIMESTAMPTZ`, 部分索引和唯一索引.
- 迁移只维护 `migrations/postgres`, 使用 goose; 不保留第二套方言.
- 事务边界在业务包, `store` 提供 `WithTx`; 索引任务和邮件写入在同一事务提交.
- CI 只启动 PostgreSQL 服务, 不维护 SQLite 测试矩阵.

## 3. frontend 目录

frontend 仓库的目标布局如下.

```
frontend/
├── README.md
├── README_CN.md
├── LICENSE
├── package.json
├── pnpm-lock.yaml
├── vite.config.ts
├── tsconfig.json
├── tsconfig.app.json
├── tsconfig.node.json
├── components.json                 # shadcn/ui 配置
├── index.html                      # 单页应用入口, 由客户端路由渲染各页面
├── .env.example
├── .editorconfig
├── .gitignore
├── public/
│   └── config.js                   # 运行时配置占位
├── src/
│   ├── main.tsx                    # 挂载 Provider 和 RouterProvider
│   ├── app/
│   │   ├── router.tsx              # 路由表
│   │   ├── AppLayout.tsx           # 侧边栏, 内容区顶栏, 列表, 阅读器
│   │   ├── SettingsLayout.tsx      # 设置二级页面独立外壳
│   │   ├── Providers.tsx           # Query, i18n, theme, toaster
│   │   └── ErrorBoundary.tsx
│   ├── pages/
│   │   ├── MailPage.tsx            # 邮件视图, 列表与阅读器
│   │   ├── LoginPage.tsx
│   │   ├── SetupPage.tsx
│   │   ├── SettingsPage.tsx
│   │   ├── ComposePage.tsx         # 新窗口模式
│   │   ├── NotFoundPage.tsx        # 未匹配路径
│   │   └── settings/
│   │       ├── AccountPage.tsx
│   │       ├── MailAccountsPage.tsx
│   │       ├── IdentitiesPage.tsx
│   │       ├── GeneralPage.tsx
│   │       ├── MailSettingsPage.tsx
│   │       ├── PerformancePage.tsx
│   │       ├── ShortcutsPage.tsx
│   │       └── AboutPage.tsx
│   ├── features/
│   │   ├── auth/
│   │   ├── accounts/
│   │   ├── identities/
│   │   ├── mailboxes/
│   │   ├── message-list/
│   │   ├── message-view/
│   │   ├── archive/                # 本地归档列表, 阅读, 附件
│   │   ├── compose/                # 写邮件浮层, 身份选择, 草稿
│   │   ├── search/
│   │   ├── tasks/                  # 任务环, 任务面板, 进度聚合
│   │   └── settings/
│   ├── components/
│   │   ├── ui/                     # shadcn/ui 生成组件
│   │   ├── layout/                 # Sidebar, TopBar, UserMenu, InsecureTransportBanner, SettingsSidebar
│   │   └── common/                 # Avatar, Badge, ConfirmDialog, Loading
│   ├── lib/
│   │   ├── api/
│   │   │   ├── schema.d.ts         # openapi-typescript 生成
│   │   │   ├── client.ts           # openapi-fetch 实例
│   │   │   └── errors.ts
│   │   ├── events/useEventStream.ts
│   │   ├── mail/                   # HTML 清洗, 引用折叠, 地址格式化
│   │   ├── format.ts
│   │   └── utils.ts
│   ├── hooks/
│   ├── stores/
│   │   ├── ui.ts                   # 侧边栏, 阅读器, 主题
│   │   └── compose.ts              # 草稿面板状态
│   ├── locales/
│   │   ├── en/
│   │   └── zh-CN/
│   └── styles/
│       └── index.css               # Tailwind v4, 主题变量, 字体
├── e2e/
│   └── mail.spec.ts
└── .github/
    └── workflows/
        ├── ci.yml
        └── release.yml              # 标签构建并生成 Release
```

### 3.1 前端模块

| 目录 | 职责 |
| --- | --- |
| `main.tsx` | 应用入口, 挂载 Provider 和路由, 不写业务逻辑 |
| `app` | 邮件壳, 设置壳, 路由表, Provider, 全局错误边界 |
| `pages` | 页面级组件, 由路由挂载, 只做数据编排和布局组合 |
| `features/auth` | 登录, 首次设置, 会话查询, 登出 |
| `features/accounts` | 邮箱账号, 连接地址, 凭据, OAuth 跳转, 连接状态 |
| `features/identities` | 发件身份, 默认身份, 签名 |
| `features/mailboxes` | 固定入口, 其他分组, 未读徽标, 同步模式 |
| `features/message-list` | 游标分页, 虚拟滚动, 多选, 批量操作, 快捷键 |
| `features/message-view` | 头部信息, 正文 iframe, 附件, 回复和本地归档操作 |
| `features/archive` | 本地归档列表, 归档阅读, 原始邮件和附件下载 |
| `features/compose` | 写邮件浮层, 身份选择, 草稿编辑, 附件上传, 发送状态 |
| `features/search` | 标题/全部模式, 账号/文件夹/收件人/发件人/附件组合筛选, 结果列表 |
| `features/tasks` | 任务环, 悬停任务面板, 失败详情, 重试和忽略, 进度聚合 |
| `features/settings` | 设置分组, 表单控件, 即时保存 |
| `components/layout` | `Sidebar`, `TopBar`, `UserMenu`, `SettingsSidebar`, 布局原子 |
| `lib/api` | 生成的类型与客户端, 错误归一化 |
| `lib/events` | SSE 连接, 重连, 事件到 Query 缓存的映射 |
| `stores` | 只放 UI 状态, 不放服务端数据 |

## 4. 前端布局

整体是左侧固定侧边栏加右侧内容区. 侧边栏顶部是全局收藏夹, 下面是邮箱账号列表; 每个账号默认折叠, 展开后显示系统文件夹和该账号的"其他".

```text
┌──────────────────┬───────────────────────────────────────────────────────┐
│ 邮件              │ [写邮件]          [搜索] [刷新] [任务环] [主题] [头像]│
│ ▾ 收藏夹          ├───────────────────────┬───────────────────────────────┤
│   全部收件    12  │ 列表工具栏             │ 阅读工具栏 [回复] [转发] ...  │
│   未读         3  │ 邮件列表               │ 发件人 / 主题                  │
│   已发送          │ (虚拟滚动)             │ 正文                           │
│   发送中       1  │                       │                               │
│   草稿         2  │                       │                               │
│   垃圾箱          │                       │                               │
│   已标记       4  │                       │                               │
│   本地归档        │                       │                               │
│                  │                       │                               │
│ ▸ me@yeqing.im    │                       │                               │
│ ▸ me@example.com  │                       │                               │
│ ▸ other@example   │                       │                               │
│                  │                       │                               │
│ 同步状态 / 设置   │                       │                               │
└──────────────────┴───────────────────────┴───────────────────────────────┘
```

尺寸与样式约定:

| 项 | 值 |
| --- | --- |
| 内容区顶栏 | `h-14 md:h-16`, 只覆盖内容区, `px-4 md:px-6`, 底部边框 |
| 侧边栏宽度 | `232px`, CSS 变量 `--sidebar-width` |
| 侧边栏头部 | `h-14 md:h-16`, 标题"邮件" |
| 导航条目 | `min-h-9`, `rounded-lg`, `px-3`, `gap-2.5`, 文字 `text-sm` |
| 激活态 | `bg-accent text-accent-foreground font-medium` |
| 图标 | `lucide-react`, `size-4`, 默认 `text-muted-foreground` |
| 列表栏宽度 | `clamp(320px, 28vw, 420px)`, 可拖拽调整并持久化 |
| 移动端 | 侧边栏放入 `Sheet side="left"`, 宽度 `232px`; 列表与阅读器分视图, 由 Zustand 切换 |

侧边栏结构:

1. 侧边栏标题"邮件".
2. 全局收藏夹分组: 可折叠, 默认展开; 条目为全部收件, 未读, 已发送, 发送中, 草稿, 垃圾箱, 已标记, 本地归档; 全局没有"其他".
3. 邮箱账号列表: 每个账号一行, 显示头像或颜色圆点, 邮箱和未读计数; 默认折叠, 点击展开.
4. 展开账号后重复显示系统文件夹: 收件箱, 未读, 已发送, 草稿, 垃圾箱, 其他.
5. 不在系统文件夹中的 Junk, Archive, 服务商标签和自定义文件夹, 全部放入该账号的"其他"; 其他在该组最下面, 默认折叠.
6. 底部: 同步状态和设置入口; 不放主题和登录用户菜单.

收藏夹规则:

- 全部收件包含收件箱, 垃圾邮件和自定义文件夹, 排除已发送, 草稿和垃圾箱; 是本地联合视图并按 `(account_id, dedupe_key)` 去重.
- 未读使用与全部收件相同的范围; 已标记是本地标记视图, 不受该范围限制.
- 已发送对应服务器 Sent, 只拉取; 发送中对应本地 outbox; 草稿对应本地草稿; 垃圾箱对应服务器 Trash.
- 每个邮箱组展开后只显示系统文件夹; 其余服务器文件夹全部进入该账号的"其他".
- 已标记数量为 0 时不显示该入口; 每个账号的展开状态和该账号"其他"的展开状态独立保存.
- 标记邮件显示完整正文和附件; 账号删除后保留的邮件在列表中带"本地保留"标识.
- 本地归档独立于账号和远端文件夹, 只读; 不进入"全部收件", 远端删除和删除账号都不影响.
- 本地归档按账号和 Message-ID 或原文哈希去重, 保存完整原文和附件元数据.
- 每个条目右侧显示未读数或待处理数, 选中项使用 `bg-accent`.

组件使用 shadcn/ui 的 `Button`, `Avatar`, `Badge`, `Collapsible`, `DropdownMenu`, `HoverCard`, `Popover`, `Sheet`, `Tooltip`, `Skeleton`. 不引入第二套组件库.

### 4.1 内容区顶栏

- 非 HTTPS 且未被豁免时, 应用最顶部先渲染一条全宽红色提醒条, 位于内容区顶栏之上, 不可关闭, 不可隐藏.
- 提醒条文案说明连接不是 HTTPS, 凭据和会话可能被窃听, 并提示配置 HTTPS 或添加免提醒域名.
- 提醒条只在请求命中豁免域名或 CIDR 时不渲染; 不用本地存储, 不提供关闭按钮.
- 顶栏只覆盖右侧内容区, 不横跨侧边栏; 侧边栏从页面顶部延伸到底部.
- 左侧只放写邮件主按钮, 左对齐; 点击打开二级写邮件窗口.
- 右侧依次为搜索入口, 刷新, 任务环, 明暗切换和登录用户头像; 头像最靠右.
- 搜索入口点击后展开输入框, 提交后跳转到 `/search` 页面, 搜索条件存 Zustand 和 `sessionStorage`, 不写入 URL; 移动端用图标按钮.
- 失败任务进入任务环, 用红色弧段表示, 不单独放通知铃铛.
- 登录用户头像悬停展开卡片, 显示头像, 名称, 邮箱, 设置入口和退出; 这是登录用户, 不是邮箱账号.
- 邮箱账号筛选在侧边栏顶部, 负责切换全部账号或单个邮箱账号.
- 回复, 转发, 标记, 本地归档, 删除只放在阅读器顶部, 不放在内容区顶栏; 全部回复放在回复菜单.
- 顶栏不参与列表滚动, 使用 `sticky top-0`, 半透明背景和底部边框.
- 移动端顶栏保留菜单按钮, 写邮件, 搜索, 任务环和头像; 次要按钮收进更多菜单.

### 4.2 环形任务指示器

- 直径 `36px`, 描边 `3px`, 命中区域 `40px`; 使用 SVG 多段圆环, 不用 Canvas.
- 无任务时整环为白色; 浅色主题加 1px `--border` 外轮廓, 保证可见.
- 每个任务按工作量占比获得弧段: `weight = total > 0 ? total : 1`.
- 段内按 `done / total` 填充; 未完成部分使用同色低透明度作为底; 任务完成或取消后弧段移除.
- 待运行任务保留低透明度弧段; 运行中任务使用实色; 失败任务整段使用 `--task-error` 红色, 并在面板中置顶.
- 失败任务不会自动消失, 直到用户重试, 打开草稿处理或手动忽略; 完成任务 2 秒后移除.
- 只有失败任务时环上显示红色弧段, 不是全白; 没有任何任务或失败时才显示白色整环.
- 顶部控制带右侧只显示环, 不显示文字百分比; 聚合百分比放在 `aria-valuenow` 和无障碍标签里.

任务类型与颜色 token:

| 任务类型 | token | 默认语义 |
| --- | --- | --- |
| `initial_sync` / `mailbox_sync` | `--task-sync` | 绿色, 同步文件夹和元数据 |
| `outbox_send` | `--task-send` | 绿色, 发件队列 |
| `body_fetch` / `mark_cache` / `archive_cache` | `--task-fetch` | 青色, 拉取正文和附件 |
| `search_index` | `--task-index` | 紫色, Bleve 索引 |
| `account_purge` / `delete_queue` | `--task-muted` | 灰蓝色, 清理任务 |
| 失败状态 | `--task-error` | 红色, 覆盖对应弧段 |

悬停任务面板:

- 鼠标进入环或键盘聚焦时, 从环下方展开一个右对齐的长方形面板, 距离环 `8px`.
- 宽度 `288px`, 最大高度 `320px`, 超出时内部滚动; `rounded-xl`, `border`, `bg-popover`, 阴影.
- 面板标题显示"任务"和运行中数量; 每行显示颜色点, 任务类型, 账号, 状态, 百分比, `done / total`.
- 失败任务排在最上面, 红色标识, 显示错误摘要和操作: 重试, 打开草稿, 打开账号设置, 忽略.
- 忽略只从任务环隐藏该失败, 不删除邮件; 重试成功后红色弧段移除.
- 待运行任务降低透明度; 已完成任务在 2 秒后移除; 失败任务不自动移除.
- 动画为 `translateY(-4px)` 加淡入, 时长 `160ms`; `prefers-reduced-motion` 下取消位移和填充动画.
- 鼠标离开后延迟 `120ms` 关闭, 避免从环移动到面板时闪烁; `Esc` 立即关闭.
- 面板是 `Popover`, 不阻塞页面操作; 键盘 Tab 可进入面板, 焦点离开后关闭.

无障碍与实现:

- 环使用 `role="progressbar"`, `aria-valuemin=0`, `aria-valuemax=100`, 聚合进度为 `sum(done) / sum(total)`.
- 面板使用 `aria-live="polite"`, 任务状态变化时读屏可感知; 颜色不是唯一信息, 每行同时显示类型文字.
- 任务数据来自 `GET /api/v1/tasks` 和 `task.updated` 事件; 组件不自己计算业务总数, 只做聚合和渲染.
- 颜色全部走 CSS 变量, 浅色和深色主题各自定义, 不在组件里写十六进制.

### 4.4 设置二级页面

- 设置的每个分组是独立全屏页面, 不复用主顶栏和主侧边栏.
- 左侧是设置专用侧边栏, 宽度 `248px`; 顶部是登录用户头像和名称, 下面是设置搜索框.
- 搜索框下面是分组导航, 分组标题使用小号弱化文字; 当前条目使用 `bg-accent` 高亮.
- 右侧内容区最大宽度约 `760px`; 顶部是页面标题和一句说明, 下面是分组内容和分隔线.
- 右上角有关闭按钮, `Esc` 或关闭按钮返回 `/`; 返回后邮件视图为默认状态.
- 设置页不显示任务环, 后台同步继续; 返回邮件后任务状态仍然存在.
- 每个分组有独立路径, 支持直接打开和深链接; 不使用查询参数.

设置侧边栏分组:

| 分组 | 条目 |
| --- | --- |
| 账号 | 个人资料, 邮箱账号, 身份与签名, 安全 |
| 偏好 | 外观, 语言与时区, 快捷键 |
| 邮件 | 同步与存储, 发送与草稿, 搜索, 性能与并发 |
| 服务器 | 网络与访问, OAuth 应用, 日志 |
| 关于 | 版本与诊断 |

内容区约定:

- 每行是"标题 + 说明 + 右侧控件"的结构, 分组之间使用分隔线.
- 下拉, 开关和输入框右对齐; 说明文字在标题下方, 使用 `text-muted-foreground`.
- 修改即时保存或显式保存, 同一分组内保持一致.
- 设置搜索只过滤左侧条目, 不跨页面搜索邮件; 没有匹配项时显示空状态.
- 身份与签名页面管理 `identities`: 显示名, 邮箱, 组织, Reply-To, 固定 Bcc, HTML/文本签名, 签名插入时机和默认身份; 发信使用账号 SMTP.
- 签名插入时机为 `all`, `new`, `reply`, `forward`, `none`; 签名位置由全局设置控制, 回复时默认放在引用上方.
- 同步与存储页面提供新账号的默认同步范围和下载范围; 已有账号在邮箱账号页面单独修改.
- 同步与存储页面提供"删除账号时保留标记邮件"开关, 默认开启.
- 邮箱账号页面的删除对话框说明异步清理范围和标记邮件保留规则.
- 发送与草稿页面控制已发送拉取开关, 草稿同步模式 (`local`/`pull`/`mirror`), 拉取间隔, 推送延迟, 写邮件窗口形态 (`in_app`/`new_window`) 和自动重试次数.
- 自动重试次数设置 `send_retry_limit` 默认 3, 范围 0-10; 永久错误不重试.
- 同步与存储页面提供删除模式 (`provider_auto`/`local_queue`) 和本地队列保留天数, 默认 30 天.
- 草稿本地自动保存 2 秒防抖, 最长 10 秒; 输入过程中不调用远端 IMAP.
- 性能与并发页面显示检测到的 CPU, 内存和 cgroup 版本, 以及自动计算的同步, 拉取, 搜索并发和索引批大小.
- 性能与并发页面支持自动/手动切换和"重新检测"; 手动值保存后立即作用于新任务.

### 4.5 写邮件窗口

- 默认点击顶栏"写邮件"打开应用内浮层, 保留背后的邮件列表和阅读器.
- 设置项 `compose_window_mode=in_app|new_window`, 默认 `in_app`; 改为 `new_window` 时用浏览器新窗口打开 `/compose`.
- 新窗口模式复用同一草稿和身份数据, 关闭窗口前自动保存; `/compose` 从 `window.name` 读取 `mailquay-compose:<draftId>`, 跨窗口草稿同步和窗口聚焦使用 `BroadcastChannel`; 同一草稿已有窗口时聚焦已有窗口, 不使用路径参数或查询参数.
- 浮层最小 `720x560`, 桌面端居中偏右, 移动端全屏.
- 左上角是发送按钮和下拉箭头. 点击发送按当前身份立即发送; 下拉菜单提供身份选择和"定时发送".
- 定时发送打开日期时间选择器, 按用户时区选择, 提交时转换为 UTC 写入 `scheduled_at`.
- 定时发送的草稿在到期前保持 `scheduled`; 编辑草稿会取消定时并回到编辑状态.
- 身份弹层宽度约 `320px`, 顶部搜索, 列表显示 `显示名 <邮箱>`, 按邮箱账号分组; 允许选择任何账号的身份.
- 回复, 全部回复和转发默认选择原邮件所属邮箱账号的默认身份; 新建邮件默认使用当前选中账号的默认身份.
- 原邮件所属账号已删除时, 回退到当前账号默认身份, 并提示这是本地保留副本.
- 选择身份后更新 `From`, `Reply-To`, 固定 Bcc 和签名; 发送请求携带 `identityAccountId` 和 `identityEmail`.
- 其余字段为收件人, 抄送, 密送, 主题, 正文, 附件和格式工具栏.
- 关闭窗口自动保存草稿, 2 秒防抖自动保存; 浮层打开的草稿 ID 保存在 `sessionStorage`, 刷新后恢复, 不使用查询参数.
- 发送成功后按身份所属账号的 `sent_copy_mode` 处理已发送副本.

### 4.6 页面入口

| 路径 | 页面 |
| --- | --- |
| `/` | 全部收件 |
| `/unread` | 未读 |
| `/sent` | 已发送 |
| `/sending` | 发送中 |
| `/drafts` | 草稿 |
| `/trash` | 垃圾箱 |
| `/marked` | 已标记 |
| `/archive` | 本地归档 |
| `/accounts/{accountId}/mailboxes/{mailboxId}` | 账号文件夹; 路径只定位到文件夹 |
| `/search` | 搜索; 条件存 `sessionStorage` |
| `/compose` | 浏览器新窗口模式下的写邮件页 |
| `/login` | 登录 |
| `/setup` | 首次创建所有者账号 |
| `/settings/general` | 外观, 语言, 时区 |
| `/settings/account` | 个人资料和密码 |
| `/settings/accounts` | 邮箱账号和连接 |
| `/settings/identities` | 发件身份和签名 |
| `/settings/mail` | 同步, 存储, 发送与草稿 |
| `/settings/performance` | 资源检测和并发 |
| `/settings/shortcuts` | 快捷键 |
| `/settings/server` | 网络, OAuth, 日志等运行时参数 |
| `/settings/about` | 版本与诊断 |

每类页面路径对应一个客户端路由, 由单个 `index.html` 和 `react-router` 渲染. 邮件视图共用 `MailPage`, 根据路径选择视图; 设置页面根据 `:group` 路径参数选择分组. 页面之间使用路由跳转, 不重新加载文档; 服务端把未命中的非资源路径回落到 `index.html`.

账号和文件夹可以出现在路径中, 但路径只定位到文件夹. 账号文件夹页面从路径读取 `accountId` 和 `mailboxId`, 打开后只加载该文件夹的列表和元数据, 不自动选中或拉取任何邮件正文. 具体邮件 ID 不写入路径参数或查询参数, 不在 URL 中定位具体邮件, 避免打开链接触发正文或附件拉取; 邮件选择只保存在页面状态. `/search` 的条件存 `sessionStorage`, 不写入 URL.

设置页使用独立的 `SettingsLayout`, 不套用主应用壳. 写邮件默认是应用内浮层; 新窗口模式打开 `/compose`, 草稿 ID 通过 `window.name` 传递.

### 4.7 状态边界

| 状态 | 位置 |
| --- | --- |
| 邮件, 文件夹, 账号, 设置 | TanStack Query |
| 当前页面视图 | 页面路径和 `react-router` 路由 |
| 账号文件夹定位 | 路径参数 |
| 选中邮件, 多选集合, 侧边栏折叠, 拖拽宽度 | Zustand |
| 搜索词, 模式, 账号, 文件夹, 收件人, 发件人, 附件条件 | Zustand + `sessionStorage`, 不写入 URL |
| 草稿编辑内容 | Zustand + 定时保存到后端 |
| 主题, 语言 | 本地存储 + 设置接口 |
| 任务列表, 进度, 失败 | TanStack Query + SSE, 服务端为准 |
| 传输安全状态 | `/auth/me` 的 `security.insecureTransport`, 不写入本地存储 |
| 任务环和任务面板展开状态 | 组件本地状态, 不进入全局 store |
| 写邮件窗口开关, 当前身份, 未保存输入 | Zustand + 草稿保存; 浮层草稿 ID 存 `sessionStorage` |
| 设置导航和当前分组 | 页面路径 |
| SSE 连接 | 单例, 挂在每个页面的根组件 |

## 5. 数据模型

所有表使用 UUIDv7 主键, 时间为 UTC. 使用 PostgreSQL `timestamptz` 和 `jsonb`. 所有表包含 `created_at`, 可变表包含 `updated_at`. 敏感字段以 `_enc` 结尾, 保存 AES-256-GCM 密文.

### 5.1 表

| 表 | 关键字段 | 说明 |
| --- | --- | --- |
| `users` | `id`, `username`, `display_name`, `password_hash`, `locale`, `theme` | 单行, 所有者账号; 登录只用用户名和密码 |
| `sessions` | `id`, `user_id`, `token_hash`, `expires_at`, `user_agent`, `last_seen_at` | 会话摘要, 不存明文令牌 |
| `accounts` | `id`, `name`, `email`, `color`, `provider`, `username`, `auth_type`, `password_enc`, `oauth_provider`, `oauth_scopes`, `oauth_refresh_enc`, `receive_protocol`, `imap_addr`, `imap_security`, `pop3_addr`, `pop3_security`, `smtp_addr`, `smtp_security`, `smtp_helo_hostname`, `sync_enabled`, `initial_sync_range`, `download_scope`, `initial_sync_cutoff_at`, `sent_copy_mode`, `deleted_at` | 唯一 ID 化实体; 地址存 `host:port`; `smtp_addr` 可空; 删除时写 `deleted_at` |
| `identities` | `account_id`, `email`, `name`, `organization`, `reply_to`, `bcc`, `signature_html`, `signature_text`, `signature_mode`, `is_default` | 发件身份属于账号, 主键为 `(account_id, email)`; 使用账号 SMTP 发信; `signature_mode` 为 `all`, `new`, `reply`, `forward`, `none` |
| `mailboxes` | `id`, `account_id`, `path`, `name`, `delimiter`, `role`, `attributes`, `local_only`, `sync_mode`, `uidvalidity`, `uidnext`, `highestmodseq`, `total`, `unread`, `sync_state`, `last_synced_at`, `last_error` | IMAP 文件夹或 POP3 虚拟收件箱; `sync_mode` 为 `full`, `pull`, `off` |
| `messages` | `id`, `account_id`, `mailbox_id`, `uid`, `pop3_uidl`, `modseq`, `message_id_header`, `provider_msg_id`, `dedupe_key`, `in_reply_to`, `references`, `thread_id`, `subject`, `from`, `to`, `cc`, `bcc`, `sent_at`, `received_at`, `size`, `flags`, `keywords`, `local_flag`, `preserved_at`, `snippet`, `has_attachments`, `raw_sha256`, `body_state`, `attachments_state` | `dedupe_key` 为账号内稳定去重键; `local_flag` 是本地标记; `preserved_at` 表示账号删除后保留的标记邮件 |
| `attachments` | `id`, `message_id`, `part_path`, `filename`, `content_type`, `size`, `sha256`, `disposition`, `content_id` | 附件元数据与 blob 引用 |
| `uploaded_attachments` | `id`, `filename`, `content_type`, `size`, `sha256` | 写邮件上传的附件, 发送时从 blob 读取 |
| `local_archives` | `id`, `origin_account_id`, `origin_account_email`, `origin_mailbox_path`, `message_id_header`, `subject`, `from`, `to`, `cc`, `bcc`, `sent_at`, `received_at`, `size`, `raw_sha256`, `text_sha256`, `has_attachments`, `attachments`, `archived_at` | 独立本地归档; 不级联账号或文件夹, 保存完整原文和附件元数据 |
| `delete_queue` | `id`, `message_id`, `account_id`, `mailbox_id`, `uid`, `delete_after`, `status`, `attempts`, `last_error`, `created_at` | 本地删除队列; `status` 为 `pending`, `deleted`, `failed` |
| `threads` | `id`, `account_id`, `subject_norm`, `last_message_at`, `message_count` | 会话归并结果 |
| `drafts` | `id`, `account_id`, `identity_account_id`, `identity_email`, `in_reply_to`, `headers`, `body_text`, `body_html`, `attachments`, `revision`, `content_hash`, `origin`, `remote_uid`, `remote_uidvalidity`, `sync_state`, `conflict_of`, `status`, `sent_message_id`, `last_error`, `updated_at` | `status` 为 `editing`, `sending`, `failed`, `copy_failed`, `sent`; `sent` 后只移出草稿列表, 不删除行 |
| `outbox` | `id`, `account_id`, `identity_account_id`, `identity_email`, `draft_id`, `message_id_header`, `status`, `scheduled_at`, `attempts`, `next_attempt_at`, `lease_until`, `last_error`, `sent_at` | `scheduled`, `pending`, `sending`, `sent`, `failed`, `copy_failed`, `cancelled`; `scheduled_at` 为 UTC |
| `pending_ops` | `id`, `account_id`, `mailbox_id`, `message_id`, `uid`, `kind`, `payload`, `status`, `attempts`, `next_attempt_at`, `last_error` | IMAP 写操作队列, `kind` 为 `flags`, `move`, `delete`, `append`; POP3 只用 `pop3_delete` |
| `search_jobs` | `id`, `message_id`, `op`, `status`, `attempts`, `next_attempt_at`, `lease_until`, `last_error`, `created_at` | Bleve 索引任务; `op` 为 `index` 或 `delete`, `status` 为 `pending`, `processing`, `done`, `failed` |
| `tasks` | `id`, `type`, `account_id`, `status`, `done`, `total`, `unit`, `message`, `error`, `started_at`, `updated_at`, `completed_at`, `dismissed_at` | 用户可见后台任务; `type` 包含 `scope_reconcile`; `status` 为 `pending`, `running`, `completed`, `failed`, `cancelled` |
| `oauth_states` | `state`, `provider`, `verifier_enc`, `redirect_after`, `expires_at` | 10 分钟过期, 用后删除 |
| `settings` | `key`, `value` | 全局设置; 包括同步间隔, 下载范围, 发送, 草稿, 标记保留, 签名, 写邮件窗口, 重试次数, 删除策略, 会话有效期, 附件与预取上限, CORS, 可信代理, 免提醒名单, OAuth 客户端与加密密钥, 日志级别和并发参数 |

### 5.2 约束与索引

- `accounts(email)` 在 `deleted_at IS NULL` 时唯一, 允许删除后重新添加同一邮箱.
- `identities` 主键为 `(account_id, email)`; 每个账号最多一个 `is_default=true` 的身份.
- `mailboxes(account_id, path)` 唯一; `messages(mailbox_id, uid)` 在 `uid` 非空时唯一, `messages(mailbox_id, pop3_uidl)` 在 `pop3_uidl` 非空时唯一.
- `messages(account_id, received_at DESC, id DESC)` 支持统一收件箱游标分页.
- `messages(thread_id, sent_at)` 支持会话视图.
- `messages(mailbox_id, flags)` 部分索引用于未读计数; `messages(account_id, local_flag)` 过滤本地标记.
- `messages(account_id, dedupe_key)` 支持"全部/未读/标记"和搜索去重; `dedupe_key` 在入库时按稳定消息 ID, Message-ID, 原文哈希, UID 的优先级生成.
- `outbox(status, next_attempt_at)` 支持 worker 领取.
- `attachments(sha256)` 用于去重统计, blob 本体按哈希存储.
- 删除邮件只删除元数据和引用, 同时写入 `search_jobs` 的 `delete`; blob 由后台 GC 按引用计数回收.
- `search_jobs(status, next_attempt_at)` 支持 indexer 领取; 同一 `message_id` 只允许一个未完成任务, 已完成任务定期清理.
- `tasks(type, account_id)` 在 `status IN ('pending', 'running')` 时唯一; `tasks(status, updated_at)` 支持活动任务查询.
- `tasks(status, updated_at)` 支持活动任务查询; `tasks(type, account_id)` 只约束未结束任务.
- `local_archives(origin_account_id, message_id_header)` 在 Message-ID 非空时唯一; 另建 `raw_sha256` 唯一索引去重.
- `delete_queue(status, delete_after)` 支持到期 worker 领取.

## 6. HTTP API

约定:

- 前缀 `/api/v1`, JSON 字段使用 camelCase.
- 列表使用游标分页: `cursor`, `limit` (默认 50, 最大 200), 响应带 `nextCursor`.
- 时间为 RFC 3339 UTC 字符串.
- 写操作支持 `Idempotency-Key`, 主要用于发信和批量操作.
- 错误响应:

```json
{
  "error": {
    "code": "imap_auth_failed",
    "message": "The mail server rejected the credentials.",
    "details": {},
    "requestId": "01J..."
  }
}
```

- `api/openapi.yaml` 是合同源. 后端用 oapi-codegen 生成服务接口, 前端用 openapi-typescript 生成类型. 手写类型不得偏离合同.

### 6.1 认证

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/auth/setup` | 创建唯一所有者用户名和密码, 需要一次性 setup token, 仅首次可用 |
| `POST` | `/auth/login` | 用户名和密码登录, 返回会话 Cookie 和 CSRF 令牌 |
| `POST` | `/auth/logout` | 撤销当前会话 |
| `GET` | `/auth/me` | 当前用户, 是否需要初始化, 以及 `security.insecureTransport` |
| `POST` | `/auth/password` | 修改密码, 撤销其他会话 |

### 6.2 发件身份

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/accounts/{id}/identities` | 账号下的发件身份列表 |
| `POST` | `/accounts/{id}/identities` | 创建身份, 使用账号 SMTP 发信 |
| `PATCH` | `/accounts/{id}/identities/{email}` | 修改显示名, 组织, Reply-To, Bcc, 签名和签名时机 |
| `DELETE` | `/accounts/{id}/identities/{email}` | 删除身份, 默认身份不可删除 |
| `POST` | `/accounts/{id}/identities/{email}/default` | 设为账号默认身份 |

### 6.3 账号与 OAuth

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/accounts` | 账号列表与连接状态 |
| `POST` | `/accounts` | 新建账号: 用户名, 密码或 OAuth, 收信协议, IMAP/POP3/SMTP 地址, 同步范围和下载范围 |
| `GET` | `/accounts/{id}` | 账号详情, 含地址, 安全模式, 同步设置和身份 |
| `PATCH` | `/accounts/{id}` | 修改账号配置; 范围或下载范围变化时异步创建 `scope_reconcile` 任务 |
| `POST` | `/accounts/{id}/reconcile` | 手动重新对账同步范围和下载范围 |
| `DELETE` | `/accounts/{id}` | 软删除账号并异步清理; 默认保留标记邮件 |
| `POST` | `/accounts/{id}/test` | 测试账号保存的 IMAP/POP3 和 SMTP 连接 |
| `POST` | `/accounts/{id}/sync` | 触发立即同步 |
| `GET` | `/oauth/{provider}/start` | 生成授权 URL 并重定向 |
| `GET` | `/oauth/{provider}/callback` | 授权回调, 保存账号的 OAuth token |

### 6.4 文件夹与邮件

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/mailboxes` | 返回侧边栏树: 虚拟入口, 系统文件夹, 其他分组, 计数和同步模式 |
| `GET` | `/mailboxes/{id}` | 文件夹详情与同步状态 |
| `POST` | `/mailboxes/{id}/sync` | 同步单个文件夹 |
| `GET` | `/messages` | 列表, 支持 `accountId`, `mailboxId`, `view=all|unread|marked`, `cursor` |
| `GET` | `/messages/{id}` | 元数据与标志 |
| `GET` | `/messages/{id}/body` | 解析后的正文, 首次触发远端拉取 |
| `GET` | `/messages/{id}/raw` | 原始 MIME 下载 |
| `GET` | `/messages/{id}/attachments/{attachmentId}` | 附件下载 |
| `PATCH` | `/messages/{id}` | 更新已读, 本地标记等状态; 归档使用独立动作 |
| `POST` | `/messages/{id}/move` | 移动到目标文件夹 |
| `DELETE` | `/messages/{id}` | 按删除设置移入 Junk 或写入删除队列 |
| `POST` | `/messages/{id}/archive` | 复制完整邮件到本地归档 |
| `POST` | `/messages/batch` | 批量已读, 标记, 归档, 移动, 删除 |
| `GET` | `/archive` | 本地归档列表 |
| `GET` | `/archive/{id}` | 本地归档详情 |
| `GET` | `/archive/{id}/body` | 本地归档的正文和附件元数据 |
| `GET` | `/archive/{id}/raw` | 本地归档原始 MIME |
| `GET` | `/archive/{id}/attachments/{attachmentId}` | 本地归档附件 |
| `GET` | `/delete-queue` | 待永久删除的本地队列 |
| `GET` | `/threads/{id}` | 会话内邮件列表 |

### 6.5 草稿, 发件与搜索

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/drafts` | 草稿列表 |
| `POST` | `/drafts` | 新建草稿, 可带 `identityAccountId` 和 `identityEmail` |
| `GET` | `/drafts/{id}` | 草稿详情 |
| `PATCH` | `/drafts/{id}` | 保存草稿, 校验 `revision`, `identityAccountId` 和 `identityEmail` |
| `DELETE` | `/drafts/{id}` | 删除草稿 |
| `POST` | `/drafts/{id}/sync` | 手动推送或拉取单个草稿 |
| `POST` | `/messages/send` | 提交发件, 携带身份和可选 `scheduledAt`; 草稿保留到发送成功 |
| `GET` | `/outbox` | 发件队列, 定时任务和失败状态 |
| `PATCH` | `/outbox/{id}` | 修改定时发送时间 |
| `POST` | `/outbox/{id}/retry` | 重试失败记录, 继续使用原草稿 |
| `POST` | `/outbox/{id}/cancel` | 取消发送或定时, 草稿保留 |
| `GET` | `/search` | 结构化搜索: `q`, `mode=subject|all`, `accountId`, `mailboxId`, `recipient`, `sender`, `hasAttachment`; 游标分页 |
| `GET` | `/tasks` | 活动任务, 失败任务和最近完成任务的进度, 供任务环聚合 |
| `POST` | `/tasks/{id}/retry` | 重试失败任务对应的同步, 发件或清理 |
| `POST` | `/tasks/{id}/dismiss` | 忽略一个失败任务, 只影响任务环显示 |
| `POST` | `/attachments` | 上传草稿附件, 返回附件元数据 |
| `GET` | `/settings` | 读取全局设置, 检测到的资源和生效并发 |
| `PATCH` | `/settings` | 更新全局设置 |
| `POST` | `/settings/concurrency/detect` | 重新检测 cgroup 限制并返回建议并发, 不改变手动值 |
| `GET` | `/events` | SSE 事件流 |

### 6.6 SSE

请求: `GET /api/v1/events`, `Accept: text/event-stream`, 可选 `Last-Event-ID`.

事件字段:

```
id: <单调递增事件 ID>
event: <事件类型>
data: <JSON>
```

事件类型与载荷:

| 事件 | 载荷 |
| --- | --- |
| `hello` | `{"userId":"...","cursor":10231}` |
| `message.new` | `{"accountId":"...","mailboxId":"...","messageId":"...","unread":true}` |
| `message.updated` | `{"messageId":"...","mailboxId":"...","fields":["flags","bodyState"]}` |
| `message.removed` | `{"messageId":"...","mailboxId":"..."}` |
| `mailbox.updated` | `{"mailboxId":"...","unread":12,"total":340,"syncState":"idle"}` |
| `account.updated` | `{"accountId":"...","state":"connected","error":null}` |
| `outbox.updated` | `{"outboxId":"...","status":"sent","error":null}` |
| `task.updated` | `{"taskId":"...","type":"body_fetch","accountId":"...","status":"running","done":120,"total":500,"unit":"messages"}` |
| `task.removed` | `{"taskId":"..."}` |

## 7. 命名与边界约定

- Go 包名短小, 小写, 不使用 `util`, `common`, `helpers` 这类无边界名称.
- 对外错误使用领域错误类型, 由 `httpapi` 映射为错误码, 不把数据库或 IMAP/POP3 原始错误直接返回前端.
- 所有阻塞调用接收 `context.Context`, 超时由调用方设置.
- 前端不直接拼接 API URL, 统一走生成的客户端.
- 组件不直接 `fetch`, 不把 Query 数据复制进 Zustand.
- 同一个功能的后端处理器, 业务逻辑和前端 feature 目录使用一致的资源命名.
