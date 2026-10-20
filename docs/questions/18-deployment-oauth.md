# 18 OAuth 应用凭据的配置方式

## 已确认

- TLS 由外部反向代理或平台入口终止, 应用容器只跑 HTTP.
- 发布走 GitHub Actions; 前端只打标签, 不生成 Release; 后端生成 Release 和 Docker 镜像.
- 后端二进制覆盖 Linux, macOS, Windows 的 amd64 和 arm64; Docker 只做 Linux amd64/arm64.

## 仍未回答

1. OAuth 应用凭据放环境变量, 还是放在设置页面加密入库?
2. 是否需要两者都支持, 环境变量优先?

## 当前默认

- OAuth 应用凭据放环境变量, 回调地址由 `MAILQUAY_PUBLIC_URL` 推导.
- Microsoft 使用 `common` 租户; Google 自用先加测试用户, 公开分发再走验证.
- 不在设置页面保存 OAuth 应用凭据.

## 回答

<!-- 直接回答 1/2 或补充说明 -->
