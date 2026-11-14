# LanQin Email API

LanQin Email exposes versioned integration APIs under `/api/open/v1`. The original `/api/open` paths remain compatibility aliases.

这些接口用于外部系统集成，稳定版本入口为 `/api/open/v1`。原 `/api/open` 路径继续作为兼容别名。它们不是匿名公开接口，只接受 API Token，不接受浏览器登录 Session Cookie。

Machine-readable OpenAPI 3.1 contract: [`docs/openapi.json`](./openapi.json).

机器可读的 OpenAPI 3.1 契约见 [`docs/openapi.json`](./openapi.json)。

## Base URL

All API endpoints are relative to your LanQin Email instance:

所有接口地址都相对于你的 LanQin Email 实例：

```
https://your-instance.example.com
```

## HTTP Status Codes

The API uses standard HTTP status codes:

接口使用标准 HTTP 状态码：

| Code 状态码 | Meaning 含义 |
|------|---------|
| `200 OK` | Request succeeded / 请求成功 |
| `201 Created` | Resource created successfully / 资源创建成功 |
| `400 Bad Request` | Invalid request parameters or validation error / 请求参数无效或校验失败 |
| `401 Unauthorized` | Missing or invalid API token / 缺少或无效的 API Token |
| `403 Forbidden` | Token lacks required permissions / Token 缺少所需权限 |
| `404 Not Found` | Resource does not exist / 资源不存在 |
| `409 Conflict` | Idempotency key conflict or concurrent status change / 幂等键冲突或状态并发变化 |
| `429 Too Many Requests` | Rate limit exceeded / 超过频率限制 |
| `500 Internal Server Error` | Server error / 服务器错误 |

Validation failures — including a duplicate domain name — return `400 Bad Request`, not `409`.

校验失败（包括域名重复）会返回 `400 Bad Request`，而不是 `409`。

## Permission Group Fallback / 权限组回退规则

- Non-admin users without custom groups use the regular-user group. Explicit custom groups replace that fallback; their permissions are combined by union and their quota limits retain the existing merge rules (zero means unlimited).
- 未分配自定义组的普通用户使用“普通用户”默认组；已分配自定义组时，仅合并显式分配的组，不再叠加默认组。多组功能权限取并集，配额沿用原有合并规则（数值 `0` 表示不限额，而不是禁止使用）。
- A group with no permissions does not grant access to protected mail or admin APIs. It does not deny permissions granted by another explicit group, disable the account, or block public login pages.
- 零功能组不授予受保护的邮箱或后台 API 权限，但不会否决另一个显式组授予的权限，也不等同于停用账户或禁止访问公开登录页。已有登录会话和 API Token 在后续请求中使用最新权限。

**Upgrade note / 升级注意：** existing custom-group users no longer inherit regular-user permissions or higher default quotas. Before upgrading, explicitly add required permissions and quotas to their custom groups. This authorization tightening does not delete existing mailboxes or messages.

现有自定义组用户升级后不再继承普通组的功能权限或较高配额。升级前应核对并在自定义组中显式补齐所需权限与配额；该权限收紧不会删除已有邮箱或邮件。

## Backup Destination Administration / 备份地点管理

These session-authenticated routes require an actual super administrator (`user.role=admin`), not a permission group with `admin.settings.*`. They are not part of the bearer-token `/api/open/v1` API described by `openapi.json`.

以下接口仅允许真正的超级管理员，普通系统设置管理员不能调用；所有响应均不回显凭据。完整字段及密钥运维说明见 [备份地点管理](backup-admin.md)。已接通 SQLite 在线快照、PostgreSQL/MySQL 原生导出备份、每日计划、Drive OAuth、既有密文上传及历史，以及远端发现和隔离恢复。可选主机维护交接只接受预批准计划 ID 和已认证恢复任务，不给 API Docker socket 或任意命令权限；操作、停服及覆盖由独立 root helper 控制。

| Endpoint | Semantics / 语义 |
|------|---------|
| `GET /api/admin/backups/destinations` | `{encryptionConfigured, revision, items}`，仅返回配置元数据和凭据存在标志 |
| `POST /api/admin/backups/destinations` | 新增独立地点；传入当前 `revision` 及完整初始凭据，成功 `201` |
| `POST /api/admin/backups/destinations/{id}` | 更新地点；传入当前 `revision`，同地址下凭据留空保留，类型不可变 |
| `DELETE /api/admin/backups/destinations/{id}` | JSON body 为 `{revision}`；仅删除配置，不删除远端备份 |
| `GET /api/admin/backups/artifacts` | `{available, items, maxBytes}`，仅列出宿主机发布的认证密文目录；不接收文件路径 |
| `GET /api/admin/backups/capabilities` | `{onlineBackupAvailable, restoreAvailable, discoveryAvailable, restoreMaxBytes, driveOAuthAvailable, encryptionMode?, recipient?, reason?}`；独立发现不依赖本地归档目录，恢复大小上限以 `restoreMaxBytes` 为准；Drive 标志仅表示 OAuth 客户端配置可用，地点是否授权看 `credentialsSet`；不返回密码、私钥、客户端 secret 或本地路径 |
| `GET /api/admin/backups/encryption` | `{mode,configured,passwordSet,recipient,revision,environmentKeyConfigured}`；`mode` 仅为 `password`；未设置密码（含旧公钥配置）时 `configured/passwordSet=false`、`recipient=""`，初始无记录的 `revision="0"`；设置后 `recipient` 为不透明密码配置代，不返回密码；`environmentKeyConfigured` 为废弃兼容字段，恒为 `false` |
| `POST /api/admin/backups/encryption` | `{revision,mode,password,confirmPassword,currentPassword?}`；仅接受 `mode=password`，管理员须自行输入并确认新密码；已有密码时修改须原备份密码；不自动生成密码，不接受切换到公钥模式 |
| `GET /api/admin/backups/schedules` | `{revision, items}`，每地点独立计划，返回启停、执行周期及适用参数、IANA 时区、下次触发、最近入队 ID 和安全错误码；不返回私有锚点、配置代或凭据 |
| `POST /api/admin/backups/destinations/{id}/schedule` | `{revision,destinationsRevision,expectedRecipient,enabled,timezone,period?,time?,minute?,weekdays?,dayOfMonth?,interval?,unit?}`；周期参数见下文，启用前绑定当前加密配置及地点配置，CAS 防陈旧更新 |
| `POST /api/admin/backups/destinations/{id}/retention` | `{revision, keepLast, confirm}`；`keepLast` 为 0–100，0 禁用旧备份清理，正数须 `confirm=true`；地点配置版本须匹配，成功 `200 {ok:true}` |
| `GET /api/admin/backups/drive/client` | 返回 `{source,configured,editable,clientId,clientSecretSet,revision,redirectUri}`；来源为 `environment/database/none`，不返回 Client Secret |
| `POST /api/admin/backups/drive/client` | `{revision,clientId,clientSecret}`；仅未由环境变量管理时可写，加密保存后即时生效；同 Client ID 可留空保留密钥，换 ID 必须填写新密钥；成功 `200 {ok:true}` |
| `POST /api/admin/backups/destinations/{id}/drive/authorize` | `{revision}`，返回固定 Google `authorizationUrl`；不接受客户端令牌、私钥或回调 URL |
| `GET /api/admin/backups/drive/oauth/callback` | 同发起超级管理员会话完成授权；单次 state/PKCE，成功或失败均重定向固定管理页，不回显 provider 信息 |
| `POST /api/admin/backups/snapshots` | `{destinationIds, requestId, destinationsRevision, expectedRecipient}`；须已自设备份密码，绑定当前密码配置代并入队，立即返回 `202` 及任务，不接受本地路径或私钥 |
| `POST /api/admin/backups/discoveries` | `{destinationId, destinationsRevision, requestId, sourceFolderId?}`；单地点后台读取认证云索引，立即返回 `202` 的 `kind=discover` 任务；仅 Drive 可显式选原文件夹，不修改地点配置 |
| `GET /api/admin/backups/jobs/{id}/discovery?cursor=...` | 仅成功且 ready 的发现任务可读；每页最多 32 条 `{id,originalDestinationId,objectKey,sha256,bytes,completedAt}`，服务端返回 `nextCursor`，最多 128 条；游标不能跨任务 |
| `POST /api/admin/backups/restores` | `{sourceJobId,destinationId,expectedSHA256,destinationsRevision,requestId,encryptionMode?,identity?,backupPassword?,password,twoFactorCode?,confirm}`；`encryptionMode=key`（默认）用 `identity`，`password` 模式用 `backupPassword`，不可混传；`password` 字段始终是当前管理员重新认证密码；从发现结果恢复再附 `discoveryEntryId`；绝不接受对象名、endpoint 或本地路径 |
| `GET /api/admin/backups/maintenance/plans` | `{available,items:[{id,templateSHA256,driver,queueQuarantine}]}`；仅返回 root helper 预批准计划的安全元数据；未配置/不可用时 `200 {available:false,items:[]}`，不返回 socket、路径或密钥 |
| `POST /api/admin/backups/maintenance/requests` | `{sourceJobId,planId,templateSHA256,requestId,password,twoFactorCode?,confirm:true}`；源必须为成功、ready、未取消的在线恢复任务，driver 与计划匹配；密码/TOTP 重新认证和限流后提交本地主机 helper，返回 `202` 安全状态；不接受私钥、路径、命令、UID/GID 或 DSN |
| `GET /api/admin/backups/maintenance/requests/{id}` | 查询 helper 的持久安全状态 `{version,id,planId,webJob,state,phase,errorCode?,createdAt,updatedAt,progress?:{step,startedAt}}`；仅当前超级管理员会话，不依赖原恢复数据库仍保留旧任务或原发起人会话 |
| `GET /api/admin/backups/jobs?cursor=...` | `{items, nextCursor}`，每页最多 20 条；游标由服务端返回 |
| `POST /api/admin/backups/jobs` | `{artifactId, destinationIds, requestId, destinationsRevision}`，校验后入队并立即返回 `202` 及任务，不在 HTTP 请求中上传 |
| `GET /api/admin/backups/jobs/{id}` | 查询任务及逐地点结果；`?download=1` 下载同一脱敏 JSON 报告 |
| `POST /api/admin/backups/jobs/{id}/cancel` | 返回 `202`；排队任务移出队列，运行任务异步取消，不删除远端对象 |
| `POST /api/admin/backups/jobs/{id}/resume` | `{destinationsRevision}`；返回 `202` 及任务，仅续传符合条件的失败密文目标，保留已成功结果；限制见下文 |

并发修改返回 `409`，错误/缺失加密密钥返回 `503`，无效配置返回 `400`，数量或配置体积达到上限返回 `413`。改变远端地址须重新填写完整凭据；更新 S3 密钥对时不会保留旧会话令牌。

备份新密码须为 8–128 个 Unicode 字符（按码点计数）、非全空白且不含控制字符，首尾空格不修剪；当前备份密码错误返回 `403`。密码通过 `LANQIN_BACKUP_SECRET_KEY` 加密后保存在既有 `system_settings`；GET 仅返回状态与不透明配置代。未设置密码时不能新建或启用计划备份，即使旧 `.env` 仍有 `LANQIN_BACKUP_RECIPIENT` 也不会自动启用或生成密码。更换密码只影响新归档，旧归档不会自动改密，仍需当时的历史密码或 age 私钥；历史公钥归档恢复及原始 CLI 显式公钥参数仍兼容。密码归档可用 CLI `restore --password-file` 跨服务器离线解密，但云端自动发现仍要求原索引主密钥与云端访问权限；`LANQIN_BACKUP_SECRET_KEY` 是加密配置/索引认证主密钥，不是归档恢复密码，备份密码不能替代索引信任。

8–128 字符限制仅用于新设置或修改的密码；旧规则下已保存的密码继续可读、可执行备份及验证旧密码，历史归档恢复也不受新长度限制影响。字符按 Unicode 码点计数，不按 UTF-8 字节或浏览器 UTF-16 长度计数，不进行 Unicode 归一化。

计划 `period` 省略时兼容旧 `daily`；仅提交所选周期适用的字段：

| period | 周期参数与语义 |
| --- | --- |
| `daily` | `time:"HH:MM"`，按 `timezone` 每日执行 |
| `hourly` | `minute:0..59`，每个本地小时的指定分钟；重复小时只选早一次 |
| `weekly` | `time` 与 `weekdays:[0..6]`，0 为星期日，至少一天，不可重复 |
| `monthly` | `time` 与 `dayOfMonth:1..31`；月份不足所选日时取月底 |
| `interval` | `interval` 与 `unit:"minutes"|"hours"|"days"`；分别为 5–527040 分钟、1–8760 小时或 1–366 天，以服务端新配置锚点计算固定时长，首轮在一个完整间隔后执行 |

所有周期仍要求明确 IANA `timezone`，`hourly/interval` 可省略 `time`；固定间隔按绝对时长计算，不受 DST 影响。客户端不能提交 `anchorAt`。暂不支持秒级计划、任意 cron 或脚本。保存新计划不会立即创建备份；立即执行复用 `POST /api/admin/backups/snapshots`，可在计划关闭时单地点手动创建，不改变计划游标。去重以地点、配置代和实际 UTC 触发点为准；错过多期只补最近一期，队列满时保留未消费的触发点。

上传任务最多选择 16 个已启用地点，`requestId` 为 16–80 个字母、数字、`_` 或 `-`，同一用户、同一 ID 和同一请求重试不重复建任务；更换请求内容返回 `409`。地点版本必须来自当前列表；最多 16 个待执行任务，队列满返回 `429`。逐地点失败不是 HTTP 入队失败，需查询任务结果。取消/进程中断时远端可能已接收数据，不能把取消当作回滚。地点列表的 `lastSuccessAt` 仅代表同一配置下最近被远端接受的上传，不代表恢复演练通过。

续传仅接受 `failed/partial/interrupted`、未请求取消、存在传输检查点且仍有失败密文目标的非恢复任务，每个任务最多续传 10 次。服务端重新验证本地认证密文、当前地点版本、失败目标配置及检查点；目标须仍启用，Drive 须仍授权。已进入排队或运行的续传请求重试返回同一任务。不符合续传条件返回 `409 backup_resume_unavailable`；仅恢复索引发布失败而密文全部成功时不能通过此接口重发索引。

逐地点结果可包含 `index_state`（`succeeded` 或 `failed`）和安全分类 `index_error_code`，表示密文完整回读后认证云端恢复索引的发布结果；它们与密文的 `state/object_key/completed_at` 分开。索引失败仍保留成功密文收据，任务报告 `partial` 和 `recovery_index_publish_failed`，该地点不执行本轮旧备份清理。旧任务没有这些字段，不追认为已发布独立恢复索引。索引主密钥和 HMAC 派生材料不进入响应，见 [恢复索引](backup-discovery.md)。

发现任务复用队列、跨进程租约、取消和中断处理，`phase=queued/discovering/completed`，返回 `discovery:{count,ready,sourceFolderId?}`。结果按 32 条独立加密分页，只在成功完成后开放，失败不返回部分库存。发现不改变最近上传成功时间或触发清理；从其条目恢复仍验证当前地点配置、签名来源、摘要/字节数和重新认证。Drive 保留索引原地点 ID 与原 folder，用当前已授权凭据下载，不把新配置 ID 当作旧媒体归属。索引认证仅证明观察到历史收据，不证明列表完整、最新或文件当前仍存在。

维护请求的 host ID 是 `SHA256(UTF8(user.id + ":" + requestId))` 的前 16 字节小写 hex，不带前缀；客户端应在 POST 前持有它，响应丢失后查询同一 ID，不自动重新 POST。`409 backup_maintenance_conflict` 表示原批准摘要/请求不匹配，`404 backup_maintenance_not_found` 不表示已回滚或安全失败，未知 helper 错误统一 `503 backup_maintenance_unavailable`。状态 `state` 仅为 `queued/preparing/maintaining/succeeded/failed/needsRecovery`，`phase` 仅为 `queued/preparing/importing/maintenance/completed/interrupted`；不能把已排队或 Web 验证完成当成生产切换成功。可选 `progress` 只返回固定阶段 `step` 与该阶段起始时间 `startedAt`，不返回执行器日志、主机路径或文件名；旧 helper/历史记录省略时以 `phase` 降级显示。阶段为 `queued/checking-source/copying/checking-copy/importing/preflight/stopping/installing/validating/committing/starting/health-check/rolling-back/recovering-original/completed`；数据库类型和失败恢复路径可跳过不适用阶段，阶段序号不代表百分比或剩余时间。主机停服会导致 API 短时不可达，安装会撤销旧会话；断连时客户端应明确显示最后已知状态与时间，不能把本地计时器当作实时进度。仅重试 GET，重新登录后才可继续认证查询；401、404 或网络恢复均不能视为切换成功。配置和部署边界见 [主机维护交接](backup-maintenance-host.md)。

新建备份复用同一队列与资源限额，不另开无限并发 worker。任务 `kind` 为 `upload`、`snapshot`、`restore` 或 `discover`，`status` 沿用排队/运行/终态。`phase` 是阶段而非百分比：备份/上传使用 `queued/collecting/encrypting/uploading/retaining/completed` 中适用的阶段，隔离恢复使用 `downloading/decrypting/completed`，发现使用 `queued/discovering/completed`。snapshot 未生成归档时 `artifact.id` 为空，不应展示虚假的 SHA 或时间；生成后才附入经过认证发布的 artifact。`summary` 只包含日期、计数、字节及备份运行态清理标志，不含原始清单、来源路径、邮件 ID 或凭据。所有运行阶段可请求取消；配置变化后排队任务不会悄悄更改加密目标，而会失败要求重新确认。

## Health Endpoints / 健康检查

Health endpoints do not require authentication and never include database DSNs, storage paths, or underlying errors in their responses.

健康检查接口无需认证，响应不会包含数据库 DSN、存储路径或底层错误。

| Endpoint | Semantics / 语义 |
|------|---------|
| `GET /livez` | Process liveness only; returns `200` while the API process can serve HTTP / 仅检查 API 进程存活，能处理 HTTP 时返回 `200` |
| `GET /readyz` | Checks database, schema, storage, and enabled workers; returns `503` when unavailable / 检查数据库、schema、存储和已启用 worker，不可用时返回 `503` |
| `GET /healthz` | Backward-compatible alias of `/readyz` / `/readyz` 的向后兼容别名 |

## Error Responses

All API errors return JSON with this structure:

所有接口的错误都以如下 JSON 结构返回：

```json
{
  "error": "error message"
}
```

**Important**: The API uses `DisallowUnknownFields()` for JSON parsing. Sending fields not defined in the request schema will result in a `400 Bad Request` error.

**重要提示**：接口在解析 JSON 时启用了 `DisallowUnknownFields()`。如果请求体中包含 schema 未定义的字段，将返回 `400 Bad Request` 错误。

Examples:

示例：

```json
{
  "error": "invalid token"
}
```

```json
{
  "error": "domain not found"
}
```

```json
{
  "error": "localPart is required"
}
```

## Authentication

Open API requests must use a Bearer API Token:

Open API 请求必须使用 Bearer API Token：

```http
Authorization: Bearer lq_xxx
```

Create tokens in **Profile / API Token**. The plain token is shown only once after creation, so store it securely and revoke it if it may have leaked.

请在 **个人中心 / API Token** 中创建 Token。明文 Token 只会在创建后显示一次，请安全保存；如果怀疑泄露，应立即撤销并重新创建。

Created token example:

创建后的 Token 示例：

```json
{
  "token": "lq_xxx"
}
```

Tokens created without a custom expiration default to 90 days. You can disable or revoke tokens from the same profile page.

如果没有自定义到期时间，Token 默认 90 天后过期。你可以在同一个个人中心页面中禁用或撤销 Token。

Each token has independent scopes. Scopes only reduce the permissions of the owning user; they never grant permissions the user does not already have. Existing tokens created before scope support are migrated to `*` for compatibility.

每个 Token 都有独立 scope。scope 只会收缩 Token 所属用户已有的权限，不会授予用户原本没有的权限。scope 功能上线前创建的 Token 会迁移为 `*`，以保持兼容。

| Scope | Purpose |
|---|---|
| `domains:read` / `domains:write` | View or manage sending domains |
| `mailboxes:read` / `mailboxes:write` | View or manage mailboxes; password reset is a write operation |
| `messages:read` / `messages:send` / `messages:manage` | Read messages/status, send, or retry/cancel |
| `aliases:read` / `aliases:write` | View or manage aliases |
| `dns:read` / `dns:check` | View required records or execute DNS checks |
| `*` | Compatibility wildcard; avoid for new integrations |

## Permissions

All Open API endpoints require an API token with appropriate permissions and role requirements:

所有 Open API 接口都需要具备相应权限和角色的 API Token：

| Endpoint group | Required scope | Role |
|---|---|---|
| Domains | `domains:read` or `domains:write` | admin |
| Mailboxes | `mailboxes:read` or `mailboxes:write` | admin |
| DNS | `dns:read` or `dns:check` | admin |
| Aliases | `aliases:read` or `aliases:write` | admin |
| Send / status / messages | `messages:send`, `messages:read`, or `messages:manage` | user or admin |

**Notes:**
- Admin endpoints check for `requireAdminAccess` (role must be `admin`).
- Mail sending/reading endpoints work for regular users but only for mailboxes they own.
- Users can only read messages from their own active mailboxes.

**说明：**
- 域名和邮箱管理接口会检查 `requireAdminAccess`（角色必须为 `admin`）。
- 发信/读信接口对普通用户也可用，但只能操作自己拥有的邮箱。
- 用户只能读取自己拥有的 active 邮箱中的邮件。

## Domains

### List domains

```http
GET /api/open/v1/domains
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`

**Response:**

```json
{
  "items": [
    {
      "id": "dom_xxx",
      "name": "example.com",
      "status": "active",
      "dkimSelector": "lanqin",
      "dkimPublicKey": "v=DKIM1; k=rsa; p=MIIBIjANBgkq...",
      "dnsStatus": "unchecked",
      "dnsCheckedAt": null,
      "createdAt": "2026-06-29T00:00:00Z"
    }
  ]
}
```

**Field descriptions:**
- `status`: `active` or `disabled`
- `dnsStatus`: `unchecked` (initial), `ok` (all DNS records verified), or `error` (verification failed)
- `dnsCheckedAt`: Timestamp of last DNS check (nullable)
- `dkimPublicKey`: Public key for DKIM signing (omitted in some contexts)

**字段说明：**
- `status`：`active` 或 `disabled`
- `dnsStatus`：`unchecked`（初始）、`ok`（所有 DNS 记录校验通过）或 `error`（校验失败）
- `dnsCheckedAt`：上次 DNS 检查的时间戳（可为 null）
- `dkimPublicKey`：用于 DKIM 签名的公钥（部分场景下会省略）

**Note:** This endpoint returns all domains without pagination.

**注意：** 该接口一次性返回所有域名，不分页。

### Create domain

```http
POST /api/open/v1/domains
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "name": "example.com"
}
```

**Status:** `201 Created`

**Response:**

```json
{
  "id": "dom_xxx",
  "name": "example.com",
  "status": "active",
  "dkimSelector": "lanqin",
  "dkimPublicKey": "v=DKIM1; k=rsa; p=MIIBIjANBgkq...",
  "dnsStatus": "unchecked",
  "dnsCheckedAt": null,
  "createdAt": "2026-06-29T00:00:00Z"
}
```

**Notes:**
- Domain name is automatically normalized to lowercase
- DKIM keys are generated automatically
- Initial `dnsStatus` is `unchecked`

**说明：**
- 域名会自动规范化为小写
- DKIM 密钥会自动生成
- 初始 `dnsStatus` 为 `unchecked`

### Get domain

```http
GET /api/open/v1/domains/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

**Response:** Same as domain object in list response.

**响应：** 与列表接口中的 domain 对象结构相同。

### Update domain status

```http
POST /api/open/v1/domains/{id}
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "status": "active"
}
```

**Status:** `200 OK` or `404 Not Found`

**Request body:**
- `status`: Must be `active` or `disabled`

**请求体：**
- `status`：必须为 `active` 或 `disabled`

**Response:** Updated domain object.

**响应：** 更新后的 domain 对象。

**Note:** This endpoint uses `POST` (not `PATCH`/`PUT`) for simplicity in client implementations.

**注意：** 该接口使用 `POST`（而非 `PATCH`/`PUT`），以简化客户端实现。

### Delete domain

```http
DELETE /api/open/v1/domains/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`, `404 Not Found`, or `400 Bad Request`

**Response:**

```json
{
  "ok": true
}
```

**Error cases:**
- `400`: Domain still has mailboxes (must delete mailboxes first)
- `404`: Domain not found

**错误情况：**
- `400`：域名下仍有邮箱（需先删除邮箱）
- `404`：域名不存在

## Mailboxes

### List mailboxes

```http
GET /api/open/v1/mailboxes
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`

**Response:**

```json
{
  "items": [
    {
      "id": "mbx_xxx",
      "userId": "usr_xxx",
      "userEmail": "alice@example.com",
      "domainId": "dom_xxx",
      "localPart": "alice",
      "address": "alice@example.com",
      "displayName": "Alice",
      "quotaMb": 1024,
      "status": "active",
      "createdAt": "2026-06-29T00:00:00Z"
    }
  ]
}
```

**Note:** This endpoint returns all mailboxes without pagination.

**注意：** 该接口一次性返回所有邮箱，不分页。

### Create mailbox

The management UI's **Add mailbox** action (`POST /api/admin/mailboxes`) only attaches a mailbox to an active, existing `userId` and inherits that user's password. Create login users separately in **User Management** (`POST /api/admin/users`). It does not create users or change their password or role; legacy `password`, `role`, and `ownerEmail` fields are ignored. The public OpenAPI endpoint below retains its existing owner-resolution and password behavior.

管理界面的**添加邮箱**（`POST /api/admin/mailboxes`）仅向已启用的已有 `userId` 追加邮箱，并沿用该用户的密码。登录用户应在**用户管理**（`POST /api/admin/users`）中单独创建；添加邮箱不会创建用户，也不会修改用户密码或身份，旧客户端传入的 `password`、`role` 和 `ownerEmail` 字段会被忽略。下方对外 OpenAPI 保持原有的拥有者解析和密码行为。

```http
POST /api/open/v1/mailboxes
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "domainId": "dom_xxx",
  "localPart": "alice",
  "displayName": "Alice",
  "password": "Password123!",
  "quotaMb": 1024,
  "ownerEmail": "alice@example.com"
}
```

**Status:** `201 Created`

**Request fields:**

| Field 字段 | Required 必填 | Description 说明 |
|-------|----------|-------------|
| `domainId` | Yes | ID of an existing domain / 已存在域名的 ID |
| `localPart` | Yes | Local part of the address. Normalized to lowercase; only `a-z 0-9 . _ % + -` are kept, other characters stripped / 地址本地部分。会规范化为小写，仅保留 `a-z 0-9 . _ % + -`，其余字符会被移除 |
| `password` | Yes | At least 8 characters. Used as the mailbox password / 至少 8 位，用作邮箱密码 |
| `displayName` | No | Defaults to the mailbox address if omitted / 省略时默认使用邮箱地址 |
| `quotaMb` | No | Mailbox quota in MB / 邮箱配额（MB） |
| `ownerEmail` | No | Owner's email. See owner resolution below / 拥有者邮箱，见下方拥有者解析规则 |
| `userId` | No | Bind to an existing user by ID. Takes precedence over `ownerEmail` / 绑定到已有用户的 ID，优先级高于 `ownerEmail` |

**Owner resolution:**
- If `userId` is provided, the mailbox is bound to that existing user (must be an active user).
- Otherwise, if `ownerEmail` is provided, LanQin Email looks up an active user with that email.
- If `ownerEmail` is omitted, the mailbox address is used as the owner email.
- If no active user with that email exists, a new user is created automatically.

**拥有者解析规则：**
- 如果传了 `userId`，邮箱会绑定到该已有用户（必须是启用状态的用户）。
- 否则，如果传了 `ownerEmail`，系统会查找该邮箱对应的启用用户。
- 如果省略 `ownerEmail`，则使用邮箱地址作为拥有者邮箱。
- 如果不存在对应的启用用户，系统会自动创建一个新用户。

**Response:** Created mailbox object (same shape as list response).

**响应：** 创建后的 mailbox 对象（结构与列表接口相同）。

This endpoint is not blocked by the owner's permission group mailbox limits (`maxMailboxes`, `maxMailboxesPerDay`), but the mailbox still counts towards both, which may leave the owner unable to create further mailboxes themselves.

本接口不受拥有者所在权限组的邮箱数量上限（`maxMailboxes`）与每日新建上限（`maxMailboxesPerDay`）约束，但创建的邮箱仍会计入两者，可能导致拥有者之后无法再自助创建邮箱。

### Get mailbox

```http
GET /api/open/v1/mailboxes/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

**Response:** Mailbox object (same shape as list response).

**响应：** mailbox 对象（结构与列表接口相同）。

### Update mailbox

```http
POST /api/open/v1/mailboxes/{id}
Authorization: Bearer lq_xxx
Content-Type: application/json

{
  "displayName": "Alice Work",
  "quotaMb": 2048,
  "status": "active",
  "userId": "usr_xxx"
}
```

**Status:** `200 OK` or `404 Not Found`

All fields are optional. Omitted (or empty / non-positive) fields keep their current value. `status` can be `active` or `disabled`. When `userId` is provided, the target user must exist and be active.

所有字段均可选。省略（或为空 / 非正数）的字段会保留原值。`status` 可为 `active` 或 `disabled`。如果传了 `userId`，目标用户必须存在且处于启用状态。

**Response:** Updated mailbox object.

**响应：** 更新后的 mailbox 对象。

### Delete mailbox

```http
DELETE /api/open/v1/mailboxes/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK`, `404 Not Found`, or `400 Bad Request`

**Response:**

```json
{
  "ok": true
}
```

**Notes:**
- Deleting a mailbox also deletes all of its messages.
- If the token owner is deleting their own mailbox, it cannot be their last remaining mailbox (returns `400`).

**说明：**
- 删除邮箱会同时删除该邮箱下的所有邮件。
- 如果 Token 拥有者删除的是自己的邮箱，则不能删除最后一个邮箱（会返回 `400`）。

## Send Mail

```http
POST /api/open/v1/send
Authorization: Bearer lq_xxx
Idempotency-Key: invoice-2026-0001
Content-Type: application/json

{
  "mailboxId": "mbx_xxx",
  "to": ["bob@example.com"],
  "cc": [],
  "bcc": [],
  "subject": "Hello",
  "text": "Plain text body",
  "html": "<p>HTML body</p>",
  "attachments": [
    {
      "filename": "report.pdf",
      "contentType": "application/pdf",
      "contentBase64": "JVBERi0xLjQK..."
    }
  ]
}
```

**Status:** `201 Created`

**Request fields:**

| Field 字段 | Required 必填 | Description 说明 |
|-------|----------|-------------|
| `mailboxId` | Yes | Sending mailbox ID (must be owned by the token user) / 发信邮箱 ID（必须属于 Token 拥有者） |
| `to` | Yes | Recipient addresses (at least one recipient across to/cc/bcc, at most 100 after deduplication) / 收件人地址（to/cc/bcc 至少需有一个收件人，去重后最多 100 个） |
| `cc` | No | CC addresses / 抄送地址 |
| `bcc` | No | BCC addresses / 密送地址 |
| `subject` | No | Message subject / 邮件主题 |
| `text` | No | Plain text body / 纯文本正文 |
| `html` | No | HTML body / HTML 正文 |
| `attachments` | No | List of attachments, at most 20 (see below) / 附件列表，最多 20 个（见下方） |

**Attachment fields:**

| Field 字段 | Description 说明 |
|-------|-------------|
| `filename` | Attachment file name / 附件文件名 |
| `contentType` | MIME type, e.g. `application/pdf` / MIME 类型，如 `application/pdf` |
| `contentBase64` | Base64-encoded file content / Base64 编码的文件内容 |

Each attachment is limited by the sender's permission group (`maxAttachmentMb`, default 25 MB), and one message carries at most 20 attachments. Exceeding either returns `400`.

每个附件的大小受发信人所在权限组限制（`maxAttachmentMb`，默认 25 MB），单封邮件最多 20 个附件。超出任一限制都会返回 `400`。

**Response:**

```json
{
  "id": "mail_xxx",
  "queueId": "snd_xxx",
  "status": "queued",
  "messageId": "mail_xxx",
  "rfcMessageId": "<msg_xxx@example.com>",
  "mailboxId": "mbx_xxx",
  "mailboxAddress": "alice@example.com",
  "subject": "Hello",
  "recipients": ["bob@example.com"],
  "attemptCount": 0,
  "maxAttempts": 5,
  "createdAt": "2026-06-29T00:00:00Z"
}
```

**Response fields:**

| Field 字段 | Description 说明 |
|-------|-------------|
| `id` | Send identifier; use it with `GET /api/open/v1/send/{id}` / 发信标识，可配合 `GET /api/open/v1/send/{id}` 使用 |
| `queueId` | SMTP queue item id. Omitted when the message was only `accepted` / SMTP 队列项 ID；仅 `accepted` 时不返回 |
| `status` | Delivery status, see values below / 投递状态，见下方取值 |
| `messageId` | Internal stored message id / 内部存储的消息 ID |
| `rfcMessageId` | RFC 5322 `Message-ID` header / RFC 5322 的 `Message-ID` 头 |
| `mailboxAddress` | Sending mailbox address / 发信邮箱地址 |
| `recipients` | Deduplicated recipients (to + cc + bcc) / 去重后的收件人（to + cc + bcc） |
| `attemptCount` / `maxAttempts` | Delivery attempt counters (queue only) / 投递尝试次数（仅队列项返回） |
| `nextAttemptAt` / `lastError` | Next retry time / last delivery error (present when applicable) / 下次重试时间 / 最近一次投递错误（在适用时返回） |
| `updatedAt` / `deliveredAt` | Update / delivery timestamps (present when applicable) / 更新 / 投递时间戳（在适用时返回） |

When SMTP delivery is not configured, the message can be stored as accepted without a queue item:

如果没有配置 SMTP 投递，邮件可能只会进入 `accepted` 状态，不会产生 `queueId`。

`id` is always the stable stored send id (`mail_*`). `queueId` is the queue item (`snd_*`) and may be absent. A repeated request with the same `Idempotency-Key` and identical body returns the original send with `200` and `Idempotency-Replayed: true`; reusing the key with a different body returns `409`. Keys are retained for 24 hours.

`id` 始终是稳定的发送邮件 ID（`mail_*`）；`queueId` 是队列项 ID（`snd_*`），可能不存在。相同 `Idempotency-Key` 与相同请求体重试时返回原发送结果、状态码 `200`，并带 `Idempotency-Replayed: true`；相同 key 配不同请求体返回 `409`。key 保留 24 小时。

Current status values:

当前状态取值：

- `accepted`: message was accepted and stored, but no SMTP queue item exists.
- `queued`: queued for SMTP delivery.
- `sending`: currently being delivered.
- `relayed`: the configured upstream SMTP server accepted the message; this is not final recipient delivery.
- `failed`: delivery failed and may be retried.
- `canceled`: delivery was canceled.
- `delivered`, `bounced`, `complained`, `rejected`, `deferred`: final per-recipient provider/DSN event.
- `partial`: final events currently differ between recipients or only cover part of the recipient list.

<br>

- `accepted`：邮件已被接受并存储，但没有 SMTP 队列项。
- `queued`：已进入 SMTP 投递队列。
- `sending`：正在投递中。
- `relayed`：配置的上游 SMTP 已接受邮件，但这不代表最终收件成功。
- `failed`：投递失败，可能会重试。
- `canceled`：投递已取消。
- `delivered`、`bounced`、`complained`、`rejected`、`deferred`：每个收件人的最终供应商或 DSN 事件。
- `partial`：不同收件人的最终状态不同，或当前只收到了部分收件人的事件。

**Error cases:**

| Status 状态码 | Cause 原因 |
|--------|-------|
| `400` | No recipients / more than 100 recipients / invalid MIME / attachment too large or too many attachments / 无收件人、收件人超过 100 个、MIME 无效、附件过大或附件数量过多 |
| `403` | Sender address is not authorized / 发信地址未被授权 |
| `404` | Mailbox not found or not owned by the token user / 邮箱不存在或不属于 Token 拥有者 |
| `429` | SMTP send rate limit exceeded / 超过 SMTP 发信频率限制 |
| `507` | Mailbox quota exceeded / 邮箱配额已满 |

Final delivery events are exposed in `recipientStatuses` and through `GET /api/open/v1/send/{id}/events`.

最终投递事件会出现在 `recipientStatuses`，完整时间线可通过 `GET /api/open/v1/send/{id}/events` 获取。

## Send Status

```http
GET /api/open/v1/send/{id}
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

`id` can be the value returned by `POST /api/open/v1/send`. If a queue item exists, it can also be the queue id.

`id` 可以使用发信接口返回的 `id`；如果存在队列项，也可以使用 `queueId`。

**Response:** Same shape as the `POST /api/open/v1/send` response. Only messages belonging to the token user's mailboxes are returned; otherwise `404`.

**响应：** 结构与 `POST /api/open/v1/send` 的响应相同。只会返回属于 Token 拥有者邮箱的邮件，否则返回 `404`。

## Received Messages

```http
GET /api/open/v1/mailboxes/{id}/messages?folder=Inbox&limit=30&cursor=opaque&q=keyword
Authorization: Bearer lq_xxx
```

**Status:** `200 OK` or `404 Not Found`

Query parameters:

查询参数：

- `folder`: folder name. Defaults to `Inbox`; use `all` for all folders.
- `limit`: page size, defaults to `30`, maximum `100`.
- `cursor`: opaque stable cursor. Pass back `nextCursor` unchanged. Numeric offsets remain accepted for compatibility.
- `q`: optional search keyword. Matches subject, from, to, snippet, and body text.

<br>

- `folder`：文件夹名称。默认为 `Inbox`；使用 `all` 表示所有文件夹。
- `limit`：每页数量，默认 `30`，最大 `100`。
- `cursor`：数字偏移量。把上一次响应中的 `nextCursor` 传回即可获取下一页。
- `q`：可选搜索关键词。会匹配主题、发件人、收件人、摘要和正文。

Response:

响应：

```json
{
  "items": [
    {
      "id": "mail_xxx",
      "mailboxId": "mbx_xxx",
      "folder": "Inbox",
      "messageId": "<message@example.com>",
      "subject": "Hello",
      "from": "sender@example.com",
      "to": ["alice@example.com"],
      "receivedAt": "2026-06-29T00:00:00Z",
      "snippet": "Preview text",
      "isRead": false,
      "hasAttachments": false
    }
  ],
  "nextCursor": ""
}
```

`nextCursor` is empty when there are no more pages. Otherwise it contains the offset to pass as `cursor` for the next request.

当没有更多分页时，`nextCursor` 为空字符串；否则应将它原样作为下一次请求的 `cursor` 传入。

Users can only read messages from their own active mailboxes. Fetch message bodies and attachment metadata with `GET /api/open/v1/messages/{id}`; download an owned attachment with `GET /api/open/v1/attachments/{id}`.

用户只能读取自己拥有的 active 邮箱。

## Additional V1 Endpoints / 其他 V1 接口

- `GET /api/open/v1/send`: paginated send records.
- `GET /api/open/v1/send/{id}/events`: queue audit and final delivery events.
- `POST /api/open/v1/send/{id}/retry`: retry a failed queue item.
- `POST /api/open/v1/send/{id}/cancel`: cancel a queued or failed item.
- `POST /api/open/v1/mailboxes/{id}/password`: reset the owner user's password and all mailbox passwords owned by that user. This is a containment action: it also ends every session of that user and disables their API tokens.
- `GET /api/open/v1/domains/{id}/dns-records` and `POST .../dns-check`: DNS configuration and check.
- `/api/open/v1/aliases`: alias CRUD.

Domain names and mailbox addresses are immutable. Renaming them requires a storage/identity migration and is intentionally not exposed as a normal update operation.

域名名称和邮箱地址不可直接修改。重命名需要迁移存储路径及身份信息，因此不作为普通更新操作开放。

## Delivery Event Webhook / 投递事件回调

Configure `LANQIN_DELIVERY_WEBHOOK_SECRET`, then post up to 100 events to `POST /api/open/v1/delivery-events`. This endpoint does not accept an API Token. Set the Unix timestamp in `X-LanQin-Timestamp`, compute `HMAC-SHA256(secret, timestamp + "." + rawBody)`, and send the lowercase hexadecimal digest as `X-LanQin-Signature: sha256=<digest>`. Timestamps outside five minutes are rejected. `(provider, event id)` is idempotent.

配置 `LANQIN_DELIVERY_WEBHOOK_SECRET` 后，可向 `POST /api/open/v1/delivery-events` 一次提交最多 100 条事件。该接口不接受 API Token。将 Unix 时间戳放入 `X-LanQin-Timestamp`，计算 `HMAC-SHA256(secret, timestamp + "." + 原始请求体)`，再以 `X-LanQin-Signature: sha256=<小写十六进制>` 发送。超过五分钟的时间戳会被拒绝；`(provider, event id)` 具备幂等性。

Accepted event statuses: `delivered`, `bounced`, `complained`, `rejected`, `deferred`. Every event must identify an existing send using `queueId`, `messageId`, or `rfcMessageId`, and its recipient must belong to that send.

## Outbound Status Webhook / 主动状态推送

Set `LANQIN_STATUS_WEBHOOK_URL` and `LANQIN_STATUS_WEBHOOK_SECRET` to receive status changes proactively. Events are persisted in a SQLite outbox before delivery. Non-2xx responses are retried with backoff up to 10 attempts. Delivered and retry-exhausted records are removed after 30 days.

设置 `LANQIN_STATUS_WEBHOOK_URL` 和 `LANQIN_STATUS_WEBHOOK_SECRET` 后，可主动接收状态变化。事件会先持久化到 SQLite outbox，非 2xx 响应会按退避策略重试，最多 10 次；已送达和重试耗尽的记录会在 30 天后清理。

Outbound requests include `X-LanQin-Webhook-Id`, `X-LanQin-Timestamp`, and `X-LanQin-Signature`. Signature calculation is the same HMAC-SHA256 construction used by the inbound delivery-event endpoint: `HMAC(secret, timestamp + "." + rawBody)`. Event types include `send.accepted`, `send.queued`, `send.retry`, `send.delivered` (upstream SMTP accepted), `send.failed`, `send.canceled`, and `delivery.<final-status>`.

出站请求包含 `X-LanQin-Webhook-Id`、`X-LanQin-Timestamp` 和 `X-LanQin-Signature`。签名算法与入站投递事件相同：`HMAC(secret, timestamp + "." + 原始请求体)`。事件类型包括 `send.accepted`、`send.queued`、`send.retry`、`send.delivered`（上游 SMTP 接受）、`send.failed`、`send.canceled` 和 `delivery.<最终状态>`。

The target must be a public HTTPS URL by default. Redirects, URL credentials, loopback, private, link-local, and unspecified addresses are rejected. `LANQIN_STATUS_WEBHOOK_ALLOW_PRIVATE_HOSTS=true` relaxes this for explicitly trusted private deployments and also permits HTTP.

目标地址默认必须是公网 HTTPS。重定向、URL 用户信息、loopback、私网、链路本地和未指定地址都会被拒绝。只有明确可信的私有部署才应设置 `LANQIN_STATUS_WEBHOOK_ALLOW_PRIVATE_HOSTS=true`；开启后也允许 HTTP。

## Webmail Message Threads / Webmail 邮件会话

Authenticated Webmail users with `mail.read` can list one representative message per conversation and load all messages in a conversation:

- `GET /api/mail/threads?mailboxId={id}` lists the latest message for each thread in the selected mailbox.
- `GET /api/mail/messages/{messageId}/thread` returns all messages sharing that message's thread, ordered oldest first.

已登录且具备 `mail.read` 权限的用户可以按会话查看邮件：以上接口都会校验当前用户对邮箱的读取权限；跨用户或无权邮箱统一返回 `404`，不会因线程 ID 泄露其他用户邮件。

## Telegram Incoming Mail Notifications / Telegram 收件通知

Set `LANQIN_NOTIFICATION_SECRET_KEY` to enable per-user Telegram notification settings. The following authenticated endpoints require the `mail.rules.manage` permission:

- `GET /api/me/telegram`: read availability and the current non-secret settings.
- `POST /api/me/telegram`: save `{ "botToken": "...", "chatId": "...", "enabled": true }`. An empty `botToken` keeps the existing encrypted token.
- `DELETE /api/me/telegram`: delete the user's settings and pending notifications.
- `POST /api/me/telegram/test`: send one test message; requests are limited to one per user every 10 seconds.

配置 `LANQIN_NOTIFICATION_SECRET_KEY` 后，可启用用户级 Telegram 通知。以上接口均要求登录并具备 `mail.rules.manage` 权限；响应只返回 `tokenSet`，不会返回 Bot Token 明文。

Add `{ "type": "telegram" }` to a mail rule's `actions` array. Matching new Inbox messages are written to a persistent outbox and delivered asynchronously. The event key is unique per rule and message. Telegram rate-limit responses honor `retry_after`; other transient failures use exponential backoff, up to 10 attempts. Applying a rule to existing messages never sends Telegram notifications.

在收件规则的 `actions` 中加入 `{ "type": "telegram" }`。新进入 Inbox 且命中规则的邮件会先写入持久化 outbox，再异步发送；同一规则和邮件只会入队一次。Telegram 限流响应会遵循 `retry_after`，其他临时失败按退避策略最多重试 10 次。“应用于现有邮件”不会补发 Telegram 通知。

### AI summaries / AI 摘要

When the administrator configures `LANQIN_AI_BASE_URL` and `LANQIN_AI_MODEL` (an OpenAI-compatible `/chat/completions` endpoint), `GET /api/me/telegram` returns `"aiAvailable": true`. A telegram action may then carry `"value": "ai:zh-CN" | "ai:zh-TW" | "ai:en"`, for example `{ "type": "telegram", "value": "ai:en" }`. Saving such a rule while AI summaries are unavailable returns `400 AI summaries are not available`; any other `value` is normalized to `""` (plain notification).

The worker sends the subject, sender and the first `LANQIN_AI_MAX_INPUT_CHARS` characters of the body (never attachments) to the model once per outbox item, then delivers a plain-text summary with a priority marker and any verification codes that appear verbatim in the message. Links in model output are removed. If the model fails, times out, returns unusable output, or the user exceeds `LANQIN_AI_DAILY_LIMIT_PER_USER` calls per 24 hours, the regular notification is sent instead. Telegram delivery retries reuse the stored summary instead of calling the model again.

管理员配置 `LANQIN_AI_BASE_URL` 与 `LANQIN_AI_MODEL`（OpenAI 兼容 `/chat/completions` 接口）后，`GET /api/me/telegram` 返回 `"aiAvailable": true`，Telegram 动作可携带 `"value": "ai:zh-CN" | "ai:zh-TW" | "ai:en"`。AI 不可用时保存此类规则返回 `400 AI summaries are not available`；其他 `value` 会被归一为 `""`（普通通知）。worker 每个 outbox 任务只调用一次模型，发送主题、发件人和正文前 `LANQIN_AI_MAX_INPUT_CHARS` 字（不发送附件），推送纯文本摘要、重要度标记，以及在原邮件中逐字出现的验证码；模型输出中的链接会被移除。模型失败、超时、输出不可用，或用户 24 小时内调用超过 `LANQIN_AI_DAILY_LIMIT_PER_USER` 次时，改发普通通知。Telegram 投递重试会复用已生成的摘要，不会重复调用模型。

## Admin Delivery Queue / 管理员投递队列

Administrators with `admin.settings.view` can inspect the unified delivery queue via `GET /api/admin/delivery-queue`. Use `queueType=send|webhook|telegram`, `status=pending|failed|delivered|canceled`, `page` (1-based), and `limit` (1-100) filters. Responses contain only task metadata, attempt counters, timestamps, and a short redacted error; message MIME, webhook payloads, Telegram content, and credentials are never returned.

具有 `admin.settings.view` 权限的管理员可通过 `GET /api/admin/delivery-queue` 查看统一投递队列。支持 `queueType=send|webhook|telegram`、`status=pending|failed|delivered|canceled`、从 1 开始的 `page` 和 1-100 的 `limit` 参数。响应只包含任务元数据、尝试次数、时间戳和脱敏后的短错误信息，不会返回邮件 MIME、Webhook payload、Telegram 内容或任何凭据。

Retry and cancellation require `admin.settings.update`: `POST /api/admin/delivery-queue/{queueType}/{id}/retry` retries an eligible failed task, while `DELETE /api/admin/delivery-queue/{queueType}/{id}` cancels/cleans an undelivered task. Both operations use state and lease conditions, so delivered or concurrently claimed tasks are rejected.

重试和取消需要 `admin.settings.update` 权限：`POST /api/admin/delivery-queue/{queueType}/{id}/retry` 重新投递符合条件的失败任务，`DELETE /api/admin/delivery-queue/{queueType}/{id}` 取消并清理未投递任务。操作均带状态和租约条件，已投递或已被其他 worker 占用的任务会被拒绝。

## 域名统一收件

新增 /domains/{id}/collection 读写、/domain-collection-targets 候选邮箱和 /local-delivery-jobs 本地投递诊断接口。仅管理员加域名权限及相应 domains:read / domains:write scope 可调用。目标允许站内跨域，固定保留原件；目标生命周期变更受 409 引用保护。请求、响应、生产投递语义和迁移步骤见 [统一收件说明](DOMAIN_COLLECTION.md)。

## External Mailbox OAuth / 第三方邮箱 OAuth（PR1）

以下为浏览器会话接口（需要邮箱访问权限并开启外部 IMAP），不使用 `/api/open/v1` 的 API Token。账户只能由所属用户管理，不通过共享邮箱授权。PR1 仅接入收件和重新授权，SMTP/发件路由列仅预留。

| Endpoint | Semantics / 语义 |
|------|---------|
| `GET /api/me/external-imap-accounts?mailboxId=...` | 返回 `{items: ExternalIMAPAccount[]}` |
| `GET /api/mail/external-accounts` | 返回当前用户启用的账户，包含同样的认证状态字段 |
| `POST /api/me/external-imap-oauth/{provider}/start` | `provider=gmail\|outlook`；请求 `{mailboxId?, accountId?, name?, email?, storageMode?, syncReadState?, enabled?}`，返回 `{url}` 并设置浏览器绑定 cookie。新账户必须提供自有 `mailboxId`；`accountId` 为可选重新授权目标，须属于当前用户、`authMode=oauth2` 且 provider 一致；他人/不存在账户返回 `404`，认证方式或 provider 不符返回 `400`。重新授权可省略 `mailboxId`，保留账户元数据。`email` 非空时须与授权身份邮箱一致，默认不使用本地邮箱地址。 |
| `GET /api/external-imap-oauth/{provider}/callback` | provider 回调；无需登录会话，但须有未过期的一次性 state 和发起浏览器的绑定 cookie。使用 PKCE S256 和 OIDC nonce，校验 issuer/audience/expiry/subject；清除绑定 cookie，所有结果均 `302` 至 `{PublicBaseURL}/profile?tab=mailboxes&externalMail=<code>`，URL 不含原始错误。服务关闭时也以结果码返回。 |
| `POST /api/me/external-imap-accounts/{id}` | OAuth 账户的 `host`、`port`、`tlsMode`、`username` 不可修改，变化返回 `400`，错误为 `oauth account endpoints cannot be changed`。允许修改 `name`、自有 `mailboxId`、`storageMode`、`syncReadState`、`enabled`；忽略 OAuth 账户的 `password`。 |
| `POST /api/me/external-imap-accounts/{id}/test`、`/sync`、`/sync-folder` | `needs_reauth` 账户返回 `400` 和 `reauthorization required`，后台同步同样跳过。 |

`ExternalIMAPAccount` 新增必有字段：

- `authStatus`: `"ok"` 或 `"needs_reauth"`。刷新返回 `invalid_grant` 时要求重新授权；成功重新授权恢复 `ok` 并清空 `lastError`。
- `providerPreset`: 新建 OAuth 账户为 `"gmail"` / `"outlook"`，其他及历史账户默认 `"custom"`；历史 OAuth 账户重新授权后补写 provider。

账户继续返回 `id`、`mailboxId`、`name`、`host`、`port`、`tlsMode`、`username`、`authMode`、`oauthProvider?`、`oauthEmail?`、`oauthConfigured?`、`storageMode`、`syncReadState`、`enabled`、同步状态和时间字段；不返回密码、令牌、subject、generation 或 state 密文。

固定回调结果码：

| code | 含义 |
|------|------|
| `connected` | 新账户接入成功 |
| `reauthorized` | 既有账户重新授权成功；subject 必须相同，历史无 subject 账户必须匹配原 OAuth 邮箱 |
| `denied` | 已校验 state 的 provider 回调包含 `error` |
| `invalid_state` | state 缺失、已使用、过期、provider 或绑定 cookie 不符 |
| `email_mismatch` | 请求指定邮箱与授权邮箱不符 |
| `account_mismatch` | 重新授权身份与既有账户不符，或目标账户已不存在/不再匹配 |
| `exchange_failed` | code 换取令牌失败，或 token/OIDC 声明、返回 scope 校验失败 |
| `not_configured` | 有效回调对应 provider 未配置，或外部 IMAP 已关闭 |
| `failed` | 保存失败或授权期间目标邮箱已不可用 |

Outlook 新授权 scope 包含 `https://outlook.office.com/SMTP.Send`；旧账户需要重新授权才能获得此 scope。该 scope 在 PR1 不开启发件功能。Gmail 使用 `https://mail.google.com/`，授权请求带 `access_type=offline`、`prompt=consent`。

Schema 升至 V13（SQLite 同步补列）；原生 PostgreSQL/MySQL 备份使用 V13 清单，包含 OAuth state 表。原生恢复只接受当前 schema 版本，已有 V12 原生备份应先在对应 V12 版本恢复，再升级并重新备份；准备恢复安装时清除待回调 state，保持外部账户停用。

## External SMTP sending / 第三方邮箱发件（PR2）

仅浏览器会话接口支持外部账户发件（SMTP，含 XOAUTH2）；Open API `/api/open/v1/send` 不接受 `externalAccountId`。无 Gmail API / Microsoft Graph 发件通道。

账户 JSON 新增必有字段：`sendEnabled: boolean`、`smtpHost: string`、`smtpPort: number`、`smtpTlsMode: "tls"|"starttls"|""`、`smtpUsername: string`、`smtpPasswordSet: boolean`、`sentCopyMode: "server"|"append"|"none"`、`sendAddress: string`、`canSend: boolean`。不返回任何密码、令牌或密文。OAuth 的 `sendAddress` 为 `oauth_email`；密码账户优先使用邮箱格式的 `smtp_username`，否则使用邮箱格式的 IMAP `username`，均无效则为空。`canSend` 需要账户启用、允许发件、`authStatus="ok"`、发件地址有效、SMTP 端点完整且采用 TLS。

| Endpoint | 请求和响应 |
|------|------|
| `POST /api/me/external-imap-accounts` | 原 IMAP 创建字段之外，可提供 `sendEnabled?`（默认 false）、`smtpHost?`、`smtpPort?`、`smtpTlsMode?`、`smtpUsername?`、`smtpPassword?`、`sentCopyMode?`（新账户默认使用预设策略：gmail/outlook 为 server，其余为 append；已有账户不自动改变）。返回 `201 ExternalIMAPAccount`。 |
| `POST /api/me/external-imap-accounts/{id}` | 上述 SMTP 字段均可选；省略保留原值。`smtpPassword` 为空保留原值，从未设置时使用 IMAP 密码；`smtpUsername` 为空时认证使用 IMAP username。返回 `200 ExternalIMAPAccount`。账户管理实际使用 POST，不是 PUT。 |
| `POST /api/mail/send` | 原发送 payload 加 `externalAccountId?: string`。外部发送返回 `202 {"queued":true,"queueId":"snd_...","externalAccountId":"ximap_..."}`；本地发送仍返回 `201 MailMessage`。 |
| `POST /api/mail/schedule-send` | 原定时 payload 加 `externalAccountId?: string`，保存至 payload JSON；创建和列表响应也含此字段。创建时校验，到点再次校验。账户被停用、取消发件或要求重新授权时，定时任务变为 failed。 |
| `POST /api/mail/drafts`、`POST /api/mail/drafts/{id}` | 接受并校验 `externalAccountId?`。当前草稿存储尚不持久化或回读该字段，客户端重新打开草稿后需重新选择发件账户；定时发送独立完整保存该字段。 |
| `GET /api/me/external-imap-accounts/{id}/sends?limit=20` | 仅所有者，返回 `{"items":[{"id":"snd_...","status":"queued","subject":"合成主题","recipients":["recipient@example.test"],"createdAt":"...","updatedAt":"...","lastError":"","sentCopyStatus":"none"}]}`。limit 范围 1–100；subject 在入队时从 MIME 头进行 RFC2047 解码、去除控制字符并截断为最多 300 个 Unicode 字符，列表只读 subject 列，不读取 MIME。 |
| `POST /api/me/external-imap-accounts/{id}/sends/{sendId}/resolve` | 仅所有者，body `{"action":"delivered"|"retry"}`。仅 unconfirmed 状态有效；其他状态 `409`，他人或不存在对象 `404`。delivered 标记已投递；retry 代表用户接受可能重复发件并重新排队。成功返回 `200 {"ok":true}`。 |

账户及发件记录路由均要求 `requireAuth`、`PermissionMailAccess` 和启用外部 IMAP；发送保留 `PermissionMailSend`，定时保留 `PermissionMailSchedule`，草稿保留 `PermissionMailDrafts`。外部账户必须属于当前用户，关联本次自有邮箱，启用且允许发件，认证正常且发件地址有效；共享用户一律拒绝。发送校验失败返回 `403 "external account cannot send"`。Header From、信封 MAIL FROM 均强制为 `sendAddress`，忽略请求的 from 和别名；显示名可来自 fromName 或账户 name，Message-ID 使用外部地址域名。仍执行附件、收件人上限和 SMTP 配额校验；外部发件不写本地 Sent，也不进行本地投递。

密码账户开启发件时，SMTP 端点必须完整；仅允许 `tls` 或 `starttls`，最低 TLS1.2。错误文本分别为 `external smtp settings are incomplete`、`external smtp requires tls`、`external send address is invalid`。主机复用 IMAP 的 SSRF 检查与连接时 IP 复查；默认拒绝私网。既有 `LANQIN_EXTERNAL_IMAP_ALLOW_PRIVATE_HOSTS` 仅用于明确允许的私有部署，同时作用于 IMAP/SMTP。凭据仅发往账户保存的端点。

OAuth callback 新建及重新授权固定保存 Gmail `smtp.gmail.com:465 tls`、Outlook `smtp.office365.com:587 starttls`。SMTP 端点、用户名及 SMTP 密码不可改，修改返回 `400 "oauth account endpoints cannot be changed"`；SMTP 相关可修改项仅 `sendEnabled` 和 `sentCopyMode`。默认 `sentCopyMode=server`。

队列状态直接使用 `queued|sending|delivered|failed|unconfirmed|canceled`，无 sent→delivered 名称映射。DATA 之前 4xx/网络错误退避重试，5xx 永久失败；DATA Close 的明确 SMTP 错误码按 4xx/5xx 处理，网络或超时进入 unconfirmed，不自动重试。DATA 成功后 QUIT 失败仍视为 delivered。崩溃后无法确认结果的外部发送租约也进入 unconfirmed。账户删除时，同一事务把该账户 queued/可重试 failed 行设为永久失败，`lastError="external account removed"`。外部路由永不回退中继，即使全局 SMTP 未配置也会处理。

外部 SMTP 的 `lastError` 仅使用以下稳定错误码（删除账户的指定文本除外）：

- `external_smtp_auth_disabled`：535 且增强码为 5.7.139，或消息包含 SmtpClientAuthentication is disabled（不区分大小写）。普通 535（包括 5.7.3、5.7.30）为 external_smtp_auth_failed。
- `external_smtp_auth_failed`：认证永久失败。
- `external_smtp_rejected`：其他 5xx 永久拒绝。
- `external_smtp_temporary`：可重试失败。
- `external_smtp_unconfirmed`：投递结果不确定，需所有者确认。
- `external_account_unavailable`：账户不存在或不可发，永久失败。
- `external_reauthorization_required`：OAuth invalid_grant，需重新授权。

`sentCopyMode=server|none` 时 `sentCopyStatus=none`；append 在 SMTP 成功或用户确认 delivered 后置 pending，独立以 IMAP APPEND 写入带 `\Sent` special-use 的文件夹，缺失时查找常见 Sent/已发送等文件夹。最多尝试三次，成功 done、耗尽 failed；副本失败绝不重发 SMTP。计数 `sent_copy_attempt_count` 与既有队列租约共同持久化，计数和 subject 列均并入尚未发布的 V13，已发布 V12 可直接升级；原生备份 schema 版本为 13。SMTP 与副本状态独立，delivered 不保证副本成功。列表读取 subject 列；完整 MIME 仅在发送、重试、待确认或待处理 APPEND 时保留，处理结束后清理，详见下方保留策略。

管理端 `GET /api/admin/delivery-queue` 的条目新增 `route:"relay"|"external"`（非 send 任务为 relay），status 筛选支持 unconfirmed。管理员 retry unconfirmed 返回 `409`，只有所有者可通过 resolve 确认；其他外部任务 retry 保留 external 路由。原生备份恢复保留 unconfirmed 状态，不将其自动重排队。


## 第三方邮箱服务商预设（PR3）

`GET /api/me/external-mail-providers` 使用与 `/me/external-imap-accounts` 相同的会话认证、`PermissionMailAccess` 和外部 IMAP 启用中间件。返回 `200`：

```json
{"items":[{"id":"gmail","name":"Gmail","authMethods":["oauth2","app_password"],"oauthProvider":"gmail","oauthConfigured":true,"imapHost":"imap.gmail.com","imapPort":993,"imapTlsMode":"tls","smtpHost":"smtp.gmail.com","smtpPort":465,"smtpTlsMode":"tls","sentCopyMode":"server","requiresImapId":false}]}
```

`items` 按 gmail、outlook、yahoo、icloud、qq、163、custom 顺序返回完整七项；示例仅展示 Gmail。`authMethods` 只含 `oauth2`、`app_password`（custom 的该值也涵盖普通密码）。Gmail 同时支持两种，Outlook 只推荐 oauth2，其余只推荐 app_password。`oauthProvider` 为 gmail、outlook 或空字符串；`oauthConfigured` 仅在对应管理员 Client ID 和 Client Secret 均已配置时为 true，其余提供商为 false，不返回任何客户端密钥。custom 的两种 host 都为空字符串、两种 port 都为 0，TLS 模式默认 tls。端点和配置步骤见 [第三方邮箱指南](external-mail.md)。

创建和更新外部账户新增可选 `providerPreset`，只接受上述七个 id；未知值返回 `400 {"error":"unknown mail provider"}`。创建省略或空值默认为 custom；更新省略保留原值（密码账户显式空值重置为 custom）。选择预设不会覆盖密码账户提交的 IMAP/SMTP 端点，也不会自动变更已有发件副本策略。OAuth 创建/重新授权自动保存相应 provider；OAuth 更新省略 preset 使用其 oauth provider，显式提供的值必须等于该 provider，否则返回 `400 {"error":"oauth account provider does not match"}`，未知值仍返回 unknown mail provider。`ExternalIMAPAccount.providerPreset` 继续返回已存值。

163 预设的 `requiresImapId=true`，其他为 false。登录成功后，163 预设或主机为 imap.163.com、imap.126.com、imap.yeah.net 时，服务器若声明 ID 能力则发送 RFC 2971 ID，仅包含 name/vendor=LanQin、version=1；失败记录警告并继续使用会话。

发件内容保留策略：外部 send_queue 仅在 queued、sending、可重试 failed、unconfirmed 或 sent_copy_status=pending 时保留 mime_base64。投递完成且无待处理 APPEND、APPEND done/failed、用户 resolve delivered 且无待处理 APPEND、永久失败及取消时清空 MIME。主题仍保存在 V13 的 subject 列。MIME 已清理的永久失败记录不能管理员重试，返回 `409 delivery queue item is no longer eligible`，需重新撰写；结果待确认仍保留 MIME 以便显式 retry。原生备份准备会保留 unconfirmed 的状态与内容，取消的外部队列内容会清理。V13 同时包含 subject 和 sent_copy_attempt_count，不使用 V14。
