# 部署版本源码快照 / Deployed source snapshot

本仓库发布用户指定 `sub2新站/sub2-demo` 目录中的源码。保留上游 LICENSE、署名、应用源码及测试；本次发布不修改应用功能，也不更换生产服务。

## 对应构建记录

| 项目 | 值 |
| --- | --- |
| 原部署镜像标签 | `sub2api:turnstate-auto-20260919-125957` |
| 原本地镜像 ID / 构建输出 OCI index digest | `sha256:5f37465d42c5331193f7d020c0297e3630d5a339467e9b4798664bb5159823bb` |
| 平台 | `linux/amd64` |
| 程序版本 / 原构建 Commit 字段 | `0.2.4` / `docker` |
| 程序内嵌构建时间 | `2026-09-19T05:00:17Z`（北京时间 13:00:17） |
| 原镜像 `/app/sub2api` SHA-256 | `159264cf11cacd603b05d441544c45fa70fadb75975d0d92b4d9461d35c8e1d2` |

原镜像构建时没有 Git 提交 ID；本仓库的首次提交是在镜像构建之后创建，不能把它误称为原镜像内嵌的 commit。镜像标签只标识已有的本地部署镜像，不代表已上传到 Docker Hub 或 GHCR。

## 从源码构建

在仓库根目录运行下列命令。固定原构建基础镜像 digest 与版本/时间字段，避免浮动标签造成差异：

```sh
docker buildx build --load --platform linux/amd64 \
  --build-arg NODE_IMAGE=node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 \
  --build-arg GOLANG_IMAGE=golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc \
  --build-arg ALPINE_IMAGE=alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507 \
  --build-arg POSTGRES_IMAGE=postgres:18-alpine@sha256:6c538e7206ea40ff740ef27883529390a690b6ead6ba96b44c67a9f7c638e8fd \
  --build-arg VERSION=0.2.4 \
  --build-arg COMMIT=docker \
  --build-arg DATE=2026-09-19T05:00:17Z \
  --build-arg GOPROXY=https://proxy.golang.org,direct \
  --build-arg GOSUMDB=sum.golang.org \
  --build-arg NPM_CONFIG_REGISTRY=https://registry.npmjs.org \
  -t smart-sub2:turnstate-auto-20260919 .

docker run --rm --entrypoint sha256sum \
  smart-sub2:turnstate-auto-20260919 /app/sub2api
```

构建需要能访问基础镜像及依赖仓库；代理配置由使用者自行提供，不存入源码。Dockerfile 前端语法、pnpm@9、apk 软件源并未完全锁定，因此不承诺任意未来环境中整个镜像的逐字节可重现性。

## 本次发布验证

- 从 Git 候选文件的干净导出目录完成 `linux/amd64` Docker 重建（使用原基础镜像 digest 和原版本/时间字段）。
- 重建程序 `/app/sub2api` 的 SHA-256 与上表原镜像程序 **完全一致**，包含嵌入的前端资源；这证明本次验证环境中的构建输入能够生成相同程序，而非仅依靠文件名推测对应关系。
- 重建的 OCI index digest 因新生成的 provenance 等元数据不同而不同；不将程序校验一致夸大为整个镜像 ID 必须一致。
- `openai_codex_turn_state_test.go` 与 `openai_current_turn_state_test.go` 中全部 19 个顶层回归测试通过；仓库状态缓存目标测试通过；`cmd/server` 编译检查通过。
- Docker 前端构建的 locale completeness 测试 3 项、Vue/TypeScript 类型检查、Vite 生产构建通过。
- 使用 gitleaks 8.30.1 和独立只读审查核对公开候选文件；告警经分类为测试夹具、示例、误报或上游 public-client 常量，未确认私人凭据泄露。
- 未执行完整端到端生产流量测试；GitHub Actions 的结果需以仓库实际运行记录为准，不宣称所有上游 CI 检查已经通过。

`SOURCE_SHA256SUMS` 覆盖本次提交的源码及文档（不包含校验清单自身）。在 Linux/WSL 的干净检出中运行 `sha256sum -c SOURCE_SHA256SUMS` 可检查文件是否与本快照一致。后续修改源码时应重新生成清单。

## 状态处理功能与限制

- 包含实验性的 `current_turn_state` 请求体状态捕获、缓存及 HTTP 292/312 分支处理。
- 包含 `x-codex-turn-state` 上游响应头透传、进程内缓存，以及满足 API key/session/account/turn 匹配条件时的**缺失请求头补齐**。不是持续轮询模型，也没有保活请求任务。
- 进程内请求头缓存不是持久化存储，重启后不能依赖原缓存继续工作。
- 本地默认一小时 TTL 是代理缓存策略，不是已验证的上游令牌有效期；再次记录同一个值也不能证明上游为它续期。
- 缓存到期会删除缓存项，但客户端主动携带的旧请求头仍可能透传；已有客户端请求头的同账号分支并不等于完整的跨 turn 校验。
- HTTP 312 的清理针对另一套请求体状态缓存，并不证明请求头缓存同步失效。
- 捕获/注入日志为零不能单独证明上游从未返回状态头；部分日志记录依赖 `turn_id`。
- 以上机制**未被验证能够消除限流、解除账号配额或提升模型能力**。请求成功、出现某个头或缓存命中，都不是“不降智/不限流”的证明。

主要实现与回归测试：

- `backend/internal/service/openai_codex_turn_state.go`
- `backend/internal/service/openai_codex_turn_state_test.go`
- `backend/internal/service/openai_current_turn_state.go`
- `backend/internal/service/openai_current_turn_state_test.go`
- `backend/internal/service/openai_gateway_forward.go`
- `backend/internal/service/openai_gateway_passthrough.go`
- `backend/internal/service/openai_gateway_response_handling.go`
- `backend/internal/service/openai_gateway_service.go`
- `backend/internal/repository/gateway_cache.go`
- `backend/internal/repository/gateway_cache_current_turn_state_test.go`

## 发布边界

不包含生产 `.env`、SSH/代理/账号凭据、数据库、Redis 数据、运行日志、请求抓包、镜像归档、构建缓存及本地交接资料。文件执行权限已按源码/脚本用途规范化，`.gitignore` 增加了本地运行产物排除规则。私有部署文件及本地专用辅助脚本不作为公共源码发布。

保留的上游文档、示例、工作流不代表仓库已配置对应服务；文档中的上游镜像地址不会自动变为本次快照。发布没有推送 `v*` 标签，也没有配置生产自动部署。

### 工作流触发说明

保留上游 `.github/workflows/release.yml` 作为源码的一部分：它只由 `v*` 标签或手工 `workflow_dispatch` 触发，具有发布包/Release 的写权限。普通 `main` 推送不触发该发布工作流。本次没有推送发布标签、触发手动发布或配置 Docker Hub 凭据；如以后启用，应先审核其中的镜像目标、密钥配置和回写默认分支步骤。

### 源码保真说明

原目录的少量 Vue 文件使用 CRLF；`.gitattributes` 对这些文件禁用换行自动转换，以保留部署构建输入、避免生产前端 SFC 哈希因首次 Git 提交而变化。原有源码中的行尾空白和 SQL 文件末尾空行保持原样；首次全量提交的 `git diff --check` 会报告这些既有格式问题，本次不为消除格式告警而改写部署源码。新增发布文档和忽略规则单独通过空白检查。
