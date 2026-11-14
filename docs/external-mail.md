# 第三方邮箱接入

LanQin 使用 IMAP 收信、SMTP 发信。每个实例的管理员配置自己的 OAuth 客户端；密码接入使用授权码或应用专用密码。服务商预设用于填写建议值，密码账户仍可使用自己填写的服务器。Outlook 预设只推荐 OAuth；自建服务器的普通密码也使用 `app_password` 接入方式。

## 服务商与默认端点

| 预设 id | IMAP（TLS） | SMTP | 认证方式 | 默认发件副本 |
| --- | --- | --- | --- | --- |
| gmail | imap.gmail.com:993 | smtp.gmail.com:465，TLS | OAuth2、应用专用密码 | server |
| outlook | outlook.office365.com:993 | smtp.office365.com:587，STARTTLS | OAuth2 | server |
| yahoo | imap.mail.yahoo.com:993 | smtp.mail.yahoo.com:465，TLS | 应用专用密码 | append |
| icloud | imap.mail.me.com:993 | smtp.mail.me.com:587，STARTTLS | 应用专用密码 | append |
| qq | imap.qq.com:993 | smtp.qq.com:465，TLS | 授权码 | append |
| 163 | imap.163.com:993 | smtp.163.com:465，TLS | 授权码 | append |
| custom | 手工填写 | 手工填写，TLS 或 STARTTLS | 密码或应用专用密码 | append |

端点依据：[Google](https://developers.google.com/workspace/gmail/imap/imap-smtp)、[Microsoft Exchange Online](https://learn.microsoft.com/en-us/exchange/clients-and-mobile-in-exchange-online/pop3-and-imap4/pop3-and-imap4)、[Yahoo](https://help.yahoo.com/kb/imap-settings-sln4075.html)、[Apple](https://support.apple.com/en-ie/102525)、[QQ 客户端配置](https://help.mail.qq.com/detail/108/1089)、[网易服务器参数](https://help.mail.163.com/faqDetail.do?code=d7a5dc8471cd0c0e8b4b8f4f8e49998b374173cfe9171305fa1ce630d7f67ac22b85ac2e7c90cd63)。这些官方链接已核对页面内容；网易服务器参数以图片列出。

Microsoft 这里采用任务约定的 Exchange Online SMTP 端点；[个人 Outlook.com 文档](https://support.microsoft.com/en-us/outlook/pop-imap-and-smtp-settings-for-outlook-com)另列 `smtp-mail.outlook.com`，个人 Outlook 账户仍需实际验证当前固定端点。OAuth 账户不能修改 IMAP/SMTP 端点或用户名；密码账户不会因选择预设而被后端覆盖端点。

## 管理员准备与回调地址

先启用外部 IMAP 并设置 `LANQIN_EXTERNAL_IMAP_SECRET_KEY`，配置正确的 `PublicBaseURL`。在服务商后台登记完整回调地址，不能漏路径、使用前端页面地址或多加末尾斜杠：

```text
{PublicBaseURL}/api/external-imap-oauth/{provider}/callback
```

`provider` 为 `gmail` 或 `outlook`，例如 `https://mail.example.test/api/external-imap-oauth/gmail/callback`。公开部署使用 HTTPS。管理员在外部 IMAP 设置中保存 Client ID 和 Client Secret；也可通过 `LANQIN_EXTERNAL_IMAP_GMAIL_CLIENT_ID` / `LANQIN_EXTERNAL_IMAP_GMAIL_CLIENT_SECRET`、`LANQIN_EXTERNAL_IMAP_OUTLOOK_CLIENT_ID` / `LANQIN_EXTERNAL_IMAP_OUTLOOK_CLIENT_SECRET` 设置初始配置。预设接口只返回是否已配置，不返回这些值。

### Google / Gmail

1. 创建 Google Cloud 项目，在 API 库启用 Gmail API，配置 Google Auth Platform 同意屏幕及 Web application 类型的 OAuth 客户端。[Google 项目/API 配置指南](https://developers.google.com/workspace/gmail/api/quickstart/go)说明 API 启用和客户端创建步骤。
2. 仅供同一 Google Workspace 组织使用时可选择 **Internal**（组织策略仍可能要求管理员授权）。个人账号使用 **External**，长期自用时发布为 **In production**；符合个人使用例外的未验证应用可能保留警告与用户数量限制，不代表可以免验证公开运营。[受限作用域验证与例外](https://developers.google.com/identity/protocols/oauth2/production-readiness/restricted-scope-verification)说明适用条件。
3. **不要把 Testing 模式当作长期接入方式。** 外部应用处于 Testing 时，本功能请求的邮件作用域会使 refresh token 在 7 天后失效，需要重新授权。[Google refresh token 过期规则](https://developers.google.com/identity/protocols/oauth2)列出此限制；发布后也可能因撤销、密码变更或其他策略失效。
4. 邮件收发必须授权 `https://mail.google.com/`，另有身份校验用的 `openid email profile`。本功能经 IMAP/SMTP 使用 XOAUTH2，不能以仅 REST API 的 `gmail.readonly` 或 `gmail.send` 替代。[Gmail XOAUTH2 作用域](https://developers.google.com/workspace/gmail/imap/xoauth2-protocol)说明协议要求。
5. 将上面的 Gmail 回调地址填入 Authorized redirect URIs，把 Client ID/Secret 保存到 LanQin，再由用户发起连接。授权失效时重新授权原账户；已连接账户只能重新授权同一个服务商身份。

Gmail 的应用专用密码还受账号安全和组织策略限制；不能把普通 Google 登录密码当作通用替代方案。

### Microsoft / Outlook

1. 在 Microsoft Entra 注册应用。需要同时接入组织账号和个人 Outlook 时，选择支持多个组织目录及个人 Microsoft 账号的账户类型；当前实现固定使用 `/common` 授权和令牌端点。[注册应用说明](https://learn.microsoft.com/en-us/entra/identity-platform/quickstart-register-app)。
2. 添加 **Web** 平台的重定向 URI，填写完整 Outlook 回调地址。创建 Client Secret，保存其 **Value** 到 LanQin，不要填写 Secret ID；留意密钥有效期。
3. 配置并同意委托权限 `https://outlook.office.com/IMAP.AccessAsUser.All`、`https://outlook.office.com/SMTP.Send` 和 `offline_access`，并允许应用请求 `openid email profile` 身份信息。使用 Outlook 邮件资源的权限，不能以 Microsoft Graph 的 `Mail.Send` 代替 SMTP.Send。[微软 IMAP/SMTP OAuth 文档](https://learn.microsoft.com/en-us/exchange/client-developer/legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-by-using-oauth)。旧账户若未授予 SMTP.Send，需要重新授权。
4. 个人 Outlook 用户可能需要在网页邮箱中开启 IMAP。Microsoft 365 租户可能禁用 SMTP AUTH，OAuth 成功也不保证能发件。`external_smtp_auth_disabled` 表示明确的禁用响应（535 增强码 5.7.139 或消息含 `SmtpClientAuthentication is disabled`），请联系租户管理员核对该邮箱的 Authenticated SMTP 与组织策略；不要通过关闭全租户安全保护来试错。[SMTP AUTH 设置说明](https://learn.microsoft.com/en-us/exchange/clients-and-mobile-in-exchange-online/authenticated-client-smtp-submission)。普通 535（包括 5.7.3、5.7.30）作为 `external_smtp_auth_failed`，应排查凭据、授权及策略。

## 授权码与应用专用密码

- **QQ**：网页登录邮箱，在“设置 → 账号与安全 → 安全设置”开启 POP3/IMAP/SMTP 并生成授权码，通过账号要求的验证；将生成的授权码填写到密码栏，用户名使用完整邮箱地址。改账号密码后可能需要重新生成。[QQ 授权码说明](https://help.mail.qq.com/detail/106/985)。
- **163**：网页登录邮箱，在“设置 → POP3/SMTP/IMAP”开启 IMAP/SMTP，按提示完成手机验证并新增客户端授权码；客户端填写授权码，不是网页登录密码。入口缺失时依账号风控提示处理。[网易客户端协议与授权码帮助](https://help.mail.163.com/faq.do?m=list&categoryID=90)。LanQin 对 163 预设，及 `imap.163.com`、`imap.126.com`、`imap.yeah.net`，在登录后且服务器声明 ID 能力时发送 RFC 2971 ID（仅 name/vendor=LanQin、version=1），不发送用户信息。ID 被拒绝只记警告，其他风险策略仍可能导致 Unsafe Login。
- **iCloud**：Apple Account 需开启双重认证，在账户“登录和安全 → App 专用密码”生成专用密码。IMAP 用户名可按服务商要求填写；发信认证用户名应使用完整邮箱地址。更改主密码会撤销已有应用专用密码。[Apple 操作说明](https://support.apple.com/en-us/102654)。
- **Yahoo**：在账号安全设置生成并管理第三方应用密码，把应用密码填入 LanQin；用户名用完整 Yahoo 邮箱地址。能否生成密码由账号和服务商策略决定。[Yahoo 应用密码说明](https://help.yahoo.com/kb/generate-manage-third-party-passwords-sln15241.html)。

如单独指定 SMTP 密码，后续留空保留已存值；尚未设置独立 SMTP 密码时沿用 IMAP 密码。不要在问题反馈、截图或日志中发送授权码。

## 发件副本与待确认结果

`server` 表示依赖服务商保存已发送副本；Gmail、Outlook 默认采用此模式。`append` 表示 SMTP 成功后单独通过 IMAP 写入远端已发送文件夹；Yahoo、iCloud、QQ、163 和 custom 暂以此为默认，**这些默认策略尚未用真实账户人工验证**。`none` 表示 LanQin 不另存副本。已有账户保持原策略，不因升级或更换预设而自动重设。

外部发件不写本地 Sent，避免与 IMAP 同步出现的远端副本重复。APPEND 使用独立计数，最多尝试三次，失败不会重新 SMTP 投递。SMTP 成功并不保证 APPEND 成功；应核对发件记录中的 `sentCopyStatus`。如果服务器已经自行存副本又选择 append，或 APPEND 已接受但确认丢失后重试，都可能出现重复副本。

SMTP DATA 后断线、或发送期间租约失效，可能无法判断服务商是否已接受邮件。此时记录为 `unconfirmed`（结果待确认），**不会自动重试**。请先检查服务商的已发送目录、收件人反馈等证据；确认已发送后标记 delivered，需要重发时显式选择 retry 并接受重复发件风险。标记 delivered 只表示用户确认服务商接受，不证明邮件已进入对方收件箱。

队列仅在仍需发送、可重试失败、结果待确认或等待 APPEND 时保留完整 MIME（含附件）。投递完成且无待处理副本、APPEND 结束、永久失败或取消后清理外部 MIME。列表只读独立保存的已解码主题（去除控制字符、最多 300 个 Unicode 字符）；主题和收件人等记录元数据仍保留。永久失败清理 MIME 后不能从管理队列重新投递，需重新撰写。草稿暂不保存 `externalAccountId`，重开草稿需重新选择发件账户；定时发送会保存并在执行时重新校验它。

## 安全与运维

密码、SMTP 独立密码、OAuth token 与 OAuth 状态载荷在数据库中加密。**轮换 `LANQIN_EXTERNAL_IMAP_SECRET_KEY` 会使已存凭据失效**：请保管旧密钥与备份，并安排重新填写密码或重新 OAuth 授权；仅重启不会重新加密已有凭据。当前无自动密钥轮换迁移流程。

SMTP 必须使用 TLS 或 STARTTLS（至少 TLS 1.2）。保持私网主机访问开关关闭，除非明确需要受信任的内部邮件服务；端点会进行 SSRF 和连接时 IP 检查。OAuth token 只用于固定服务商端点。账户管理和发送记录只对所有者开放，共享邮箱权限不授予第三方账户发件权限；删除账户不会回退到全局中继。

V13 汇总本次尚未发布的 OAuth、发件和副本计数/主题字段；从已发布 V12 升级与新建数据库结构一致。曾在开发中初始化过 V13/V14 的未发布数据库需使用隔离副本核对结构，不能按已发布迁移路径直接用于生产。发布前仍应以真实 Gmail、个人 Outlook、QQ、163 等测试账户验证登录、收发与副本数量。
