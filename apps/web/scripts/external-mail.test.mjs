import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import test from "node:test"
import ts from "typescript"
import { sourceLoader, webRoot } from "./i18n-test-loader.mjs"

const profilePath = "src/pages/profile.tsx"
const profileSource = ts.createSourceFile(
  profilePath,
  fs.readFileSync(path.join(webRoot, profilePath), "utf8"),
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TSX
)

function findNode(root, predicate) {
  if (predicate(root)) return root
  return ts.forEachChild(root, (child) => findNode(child, predicate))
}

function topLevel(name) {
  const node = profileSource.statements.find(
    (statement) =>
      (ts.isFunctionDeclaration(statement) && statement.name?.text === name) ||
      (ts.isVariableStatement(statement) &&
        statement.declarationList.declarations.some((d) => d.name.getText() === name))
  )
  assert.ok(node, `${name} not found in profile.tsx`)
  return node.getText(profileSource)
}

function evaluate(code, exportName) {
  const js = ts.transpileModule(code, {
    compilerOptions: { module: ts.ModuleKind.None, target: ts.ScriptTarget.ES2020 },
  }).outputText
  return new Function(`${js}\nreturn ${exportName}`)()
}

// Synthetic URLs only: the check must accept the provider endpoint and nothing else.
test("OAuth start only follows the expected provider authorization endpoint", () => {
  const authorizationURL = evaluate(
    `${topLevel("externalOAuthAuthorizationEndpoints")}\n${topLevel("externalOAuthAuthorizationURL")}`,
    "externalOAuthAuthorizationURL"
  )
  const google = "https://accounts.google.com/o/oauth2/v2/auth?client_id=test&state=abc"
  const microsoft =
    "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=test&state=abc"
  assert.equal(authorizationURL("gmail", google), google)
  assert.equal(authorizationURL("outlook", microsoft), microsoft)
  for (const [provider, raw] of [
    ["gmail", microsoft],
    ["outlook", google],
    ["gmail", "http://accounts.google.com/o/oauth2/v2/auth?state=abc"],
    ["gmail", "https://accounts.google.com.example.test/o/oauth2/v2/auth"],
    ["gmail", "https://user:pass@accounts.google.com/o/oauth2/v2/auth"],
    ["gmail", "https://accounts.google.com/o/oauth2/v2/auth#fragment"],
    ["gmail", "https://accounts.google.com/signin/redirect?continue=https://example.test"],
    ["outlook", "javascript:alert(1)"],
    ["outlook", "not a url"],
  ]) {
    assert.equal(authorizationURL(provider, raw), "", `${provider} accepted ${raw}`)
  }
})

function externalMailMessages() {
  const effect = findNode(
    profileSource,
    (node) =>
      ts.isCallExpression(node) &&
      node.getText(profileSource).includes('params.get("externalMail")') &&
      node.expression.getText(profileSource) === "React.useEffect"
  )
  assert.ok(effect, "externalMail callback effect not found")
  const messages = findNode(
    effect,
    (node) =>
      ts.isVariableDeclaration(node) &&
      node.name.getText(profileSource) === "messages" &&
      node.initializer &&
      ts.isObjectLiteralExpression(node.initializer)
  )
  assert.ok(messages, "externalMail messages map not found")
  return new Function(`return (${messages.initializer.getText(profileSource)})`)()
}

test("every OAuth callback result code has a toast translated in all languages", () => {
  const messages = externalMailMessages()
  // Codes the API redirects with; keep in sync with the backend callback.
  const codes = [
    "connected",
    "reauthorized",
    "denied",
    "invalid_state",
    "email_mismatch",
    "account_mismatch",
    "exchange_failed",
    "not_configured",
    "failed",
  ]
  assert.deepEqual(Object.keys(messages).sort(), [...codes].sort())
  const { translateUiText } = sourceLoader()("src/lib/language.tsx")
  const texts = Object.values(messages).flatMap((m) => [m.title, m.description].filter(Boolean))
  texts.push(
    "外部邮箱授权未完成",
    "需要重新授权",
    "重新授权",
    "授权地址无效，请联系管理员",
    "OAuth 账号的服务器与用户名由授权确定，需更换账号请使用“重新授权”。"
  )
  for (const text of texts) {
    for (const language of ["en", "zh-TW"]) {
      const translated = translateUiText(text, language)
      assert.notEqual(translated, text, `${text} is not translated to ${language}`)
      if (language === "en") assert.doesNotMatch(translated, /[一-鿿]/u)
    }
  }
})

test("OAuth accounts never submit a password and lock endpoint fields", () => {
  const dialog = topLevel("ExternalImapDialog")
  assert.match(dialog, /const endpointsLocked = account\?\.authMode === "oauth2"/)
  assert.match(dialog, /password: endpointsLocked \? "" :/)
  for (const field of ["username", "host", "port"]) {
    assert.match(
      dialog,
      new RegExp(`name="${field}"[\\s\\S]{0,200}readOnly=\\{endpointsLocked\\}`),
      `${field} is not read-only for OAuth accounts`
    )
  }
  assert.match(dialog, /value=\{tlsMode\}\s+disabled=\{endpointsLocked\}/)
})

test("compose only sends through external accounts the server reports as able to send", () => {
  const mailPath = "src/pages/mail.tsx"
  const mail = fs.readFileSync(path.join(webRoot, mailPath), "utf8")
  // The page passes only owned-mailbox accounts with canSend for the selected mailbox.
  assert.match(
    mail,
    /externalSenders=\{\s*selectedMailboxOwned\s*\?\s*externalAccountItems\.filter\(\s*\(account\) => account\.canSend && account\.mailboxId === selectedMailbox\?\.id/
  )
  // A stale or foreign selection falls back to the local mailbox instead of being submitted.
  assert.match(
    mail,
    /const externalAccountId = externalSenders\.some\(\(account\) => account\.id === senderAccountId\)\s*\?\s*senderAccountId\s*:\s*""/
  )
  assert.equal(
    (mail.match(/\.\.\.\(externalAccountId \? \{ externalAccountId \} : \{\}\)/g) || []).length,
    3
  )
})

test("unconfirmed external sends require explicit confirmation before resending", () => {
  const panel = topLevel("ExternalSendHistoryPanel")
  assert.match(panel, /onClick=\{\(\) => setRetryId\(item\.id\)\}/)
  assert.match(panel, /action: "retry"/)
  assert.doesNotMatch(panel, /window\.confirm/)
  const { translateUiText } = sourceLoader()("src/lib/language.tsx")
  const labels = evaluate(topLevel("externalSendStatusLabel"), "externalSendStatusLabel")
  for (const status of ["queued", "sending", "delivered", "failed", "unconfirmed", "canceled"]) {
    const label = labels(status)
    assert.ok(label, `${status} has no label`)
    assert.doesNotMatch(translateUiText(label, "en"), /[一-鿿]/u)
  }
})

test("provider presets show translated help and never apply to existing accounts", () => {
  const hint = evaluate(topLevel("externalProviderHint"), "externalProviderHint")
  const { translateUiText } = sourceLoader()("src/lib/language.tsx")
  for (const id of ["gmail", "outlook", "yahoo", "icloud", "qq", "163"]) {
    const text = hint(id)
    assert.ok(text, `${id} has no hint`)
    assert.doesNotMatch(translateUiText(text, "en"), /[一-鿿]/u, `${id} hint not translated`)
  }
  assert.equal(hint("custom"), "")
  const dialog = topLevel("ExternalImapDialog")
  assert.match(dialog, /const preset = account\s*\?\s*undefined/)
  assert.match(dialog, /\.\.\.\(account \? \{\} : \{ providerPreset: presetId \}\)/)
})
