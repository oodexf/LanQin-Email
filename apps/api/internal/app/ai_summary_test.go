package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// All mail content below is synthetic test data.

type fakeAIServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	auth     []string
	reply    func(call int) (int, string)
}

func newFakeAIServer(t *testing.T, reply func(call int) (int, string)) *fakeAIServer {
	t.Helper()
	f := &fakeAIServer{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.requests = append(f.requests, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		call := len(f.requests)
		f.mu.Unlock()
		status, content := f.reply(call)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = io.WriteString(w, content)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}}})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAIServer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newAITestApp(t *testing.T, aiBaseURL string, mutate func(*Config)) (*App, *bytes.Buffer) {
	t.Helper()
	dir := newTestDir(t)
	cfg := Config{
		Addr:                  ":0",
		DBPath:                filepath.Join(dir, "lanqin.db"),
		DataDir:               filepath.Join(dir, "data"),
		CookieName:            "lanqin_test",
		SessionTTLHours:       24,
		AdminEmail:            "admin@lanqin.local",
		AdminPassword:         "ChangeMe123!",
		PublicHostname:        "mail.example.test",
		PublicBaseURL:         "https://mail.example.test",
		AllowInsecureHTTP:     true,
		NotificationSecretKey: "notification-test-secret",
		AIBaseURL:             aiBaseURL,
		AIAPIKey:              "sk-test-ai-key",
		AIModel:               "test-model",
		AITimeoutSeconds:      5,
		AIAllowPrivateHosts:   true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a := newTestAppWithConfig(t, cfg)
	stopTestWorkers(a)
	logs := &bytes.Buffer{}
	a.log = slog.New(slog.NewTextHandler(logs, nil))
	return a, logs
}

type fakeTelegram struct {
	*httptest.Server
	mu    sync.Mutex
	texts []string
	fail  int
}

func newFakeTelegram(t *testing.T, a *App, failFirst int) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{fail: failFirst}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"lanqin_test_bot"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail > 0 {
			f.fail--
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
			return
		}
		text, _ := body["text"].(string)
		f.texts = append(f.texts, text)
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	t.Cleanup(f.Close)
	a.telegramAPIBaseURL = f.URL
	return f
}

func (f *fakeTelegram) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func configureTelegramForAdmin(t *testing.T, a *App) *testClient {
	t.Helper()
	_, client := loginTelegramTestAdmin(t, a)
	if code := client.do("POST", "/api/me/telegram", map[string]any{"botToken": testTelegramBotToken, "chatId": "123456789", "enabled": true}, nil); code != http.StatusOK {
		t.Fatalf("save Telegram code=%d", code)
	}
	return client
}

func createAITelegramRule(t *testing.T, client *testClient, value string) int {
	t.Helper()
	return client.do("POST", "/api/me/rules", map[string]any{
		"name": "Telegram AI", "mailboxId": "", "matchMode": "all",
		"conditions": []map[string]string{{"field": "all", "operator": "equals", "value": "true"}},
		"actions":    []map[string]string{{"type": "telegram", "value": value}}, "enabled": true,
	}, nil)
}

func insertAITestMessage(t *testing.T, a *App, subject, body string, now time.Time) (messageID, mailboxID string) {
	t.Helper()
	_, mailbox := defaultAdminUserAndMailbox(t, a)
	inboxID, err := a.ensureFolder(context.Background(), mailbox.ID, "Inbox")
	if err != nil {
		t.Fatal(err)
	}
	messageID, err = a.insertMessage(context.Background(), storedMessage{
		MailboxID: mailbox.ID, FolderID: inboxID, MessageUID: newID("uid"), MessageID: "<" + newID("ai") + "@example.test>",
		Subject: subject, From: "sender@example.test", FromName: "Synthetic Sender", To: []string{mailbox.Address},
		SentAt: now, ReceivedAt: now, Snippet: snippetFrom(body, ""), BodyText: body,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.applyInboundControls(context.Background(), messageID, mailbox.ID, "sender@example.test", subject)
	return messageID, mailbox.ID
}

func outboxAIStatus(t *testing.T, a *App, messageID string) string {
	t.Helper()
	var status string
	if err := a.db.QueryRow(`SELECT ai_status FROM telegram_notification_outbox WHERE message_id=?`, messageID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestParseAISummarySanitizesUntrustedOutput(t *testing.T) {
	source := "Your sign-in code is 482913. Order ABCDEF ships soon."
	raw := "```json\n" + `{"summary":"Sign in at https://evil.example/login or www.evil.example now, mail help@evil.example or visit secure-login.example.com` + "‮" + `.","priority":"URGENT","codes":["482913","999999","ABCDEF","482913"]}` + "\n```"
	got, ok := parseAISummary(raw, "en", source)
	if !ok {
		t.Fatal("expected summary to parse")
	}
	for _, leaked := range []string{"https://", "evil.example", "www.", "@", "secure-login.example.com", "‮"} {
		if strings.Contains(got.Summary, leaked) {
			t.Fatalf("summary kept %q: %q", leaked, got.Summary)
		}
	}
	if !strings.Contains(got.Summary, "[link removed]") {
		t.Fatalf("summary=%q, want link placeholder", got.Summary)
	}
	if got.Priority != "normal" {
		t.Fatalf("priority=%q, want normal for unknown value", got.Priority)
	}
	if len(got.Codes) != 1 || got.Codes[0] != "482913" {
		t.Fatalf("codes=%v, want only the digit code present in the source", got.Codes)
	}

	if _, ok := parseAISummary("not json at all", "en", source); ok {
		t.Fatal("non-JSON reply must be rejected")
	}
	if _, ok := parseAISummary(`{"summary":"   ","priority":"high"}`, "en", source); ok {
		t.Fatal("empty summary must be rejected")
	}
	long, ok := parseAISummary(`{"summary":"`+strings.Repeat("a", 1000)+`","priority":"high","codes":"482913"}`, "zh-CN", source)
	if !ok || len([]rune(long.Summary)) > aiSummaryMaxRunes+1 || long.Priority != "high" || len(long.Codes) != 0 {
		t.Fatalf("long summary=%d runes priority=%q codes=%v", len([]rune(long.Summary)), long.Priority, long.Codes)
	}
}

func TestTelegramActionValueNormalization(t *testing.T) {
	actions := normalizeRuleActions([]MailRuleAction{
		{Type: "telegram", Value: "ai:en"},
		{Type: "telegram", Value: " ai:zh-TW "},
		{Type: "telegram", Value: "ai:fr"},
		{Type: "telegram", Value: "https://example.test"},
		{Type: "telegram"},
	}, "")
	want := []string{"ai:en", "ai:zh-TW", "", "", ""}
	if len(actions) != len(want) {
		t.Fatalf("actions=%+v", actions)
	}
	for i, action := range actions {
		if action.Value != want[i] {
			t.Fatalf("action %d value=%q, want %q", i, action.Value, want[i])
		}
	}
}

func TestAISummaryRuleRequiresAvailableService(t *testing.T) {
	a, _ := newAITestApp(t, "", nil)
	newFakeTelegram(t, a, 0)
	client := configureTelegramForAdmin(t, a)
	var settings TelegramSettings
	if code := client.do("GET", "/api/me/telegram", nil, &settings); code != http.StatusOK || settings.AIAvailable {
		t.Fatalf("code=%d settings=%+v, want aiAvailable=false", code, settings)
	}
	if code := createAITelegramRule(t, client, "ai:en"); code != http.StatusBadRequest {
		t.Fatalf("AI rule without service code=%d, want 400", code)
	}
	if code := createAITelegramRule(t, client, "ai:unknown"); code != http.StatusCreated {
		t.Fatalf("invalid AI value code=%d, want 201 with value dropped", code)
	}
	var stored string
	if err := a.db.QueryRow(`SELECT actions_json FROM mail_rules WHERE name='Telegram AI'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "ai:") {
		t.Fatalf("stored actions=%s, want value normalized away", stored)
	}
}

func TestTelegramAISummaryIsGeneratedOnceAndSurvivesDeliveryRetry(t *testing.T) {
	ai := newFakeAIServer(t, func(int) (int, string) {
		return http.StatusOK, `{"summary":"Your build finished; review it at https://phish.example before Friday.","priority":"high","codes":["482913","000000"]}`
	})
	a, logs := newAITestApp(t, ai.URL+"/v1/", nil)
	now := time.Date(2026, time.November, 4, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	telegram := newFakeTelegram(t, a, 1)
	client := configureTelegramForAdmin(t, a)
	var settings TelegramSettings
	if code := client.do("GET", "/api/me/telegram", nil, &settings); code != http.StatusOK || !settings.AIAvailable {
		t.Fatalf("code=%d settings=%+v, want aiAvailable=true", code, settings)
	}
	if code := createAITelegramRule(t, client, "ai:en"); code != http.StatusCreated {
		t.Fatalf("create AI rule code=%d", code)
	}
	body := "Synthetic body: build 42 passed. Verification code 482913."
	messageID, _ := insertAITestMessage(t, a, "Build report", body, now)
	if status := outboxAIStatus(t, a, messageID); status != telegramAIStatusPending {
		t.Fatalf("ai_status=%q, want pending", status)
	}

	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(telegram.delivered()) != 0 || outboxAIStatus(t, a, messageID) != telegramAIStatusDone {
		t.Fatalf("first attempt delivered=%v status=%q", telegram.delivered(), outboxAIStatus(t, a, messageID))
	}
	now = now.Add(2 * time.Second)
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls() != 1 {
		t.Fatalf("AI calls=%d, want 1 across delivery retries", ai.calls())
	}
	texts := telegram.delivered()
	if len(texts) != 1 {
		t.Fatalf("delivered=%v", texts)
	}
	text := texts[0]
	for _, want := range []string{"AI summary", "Priority: High", "Codes: 482913", "[link removed]", "Subject: Build report", "From: Synthetic Sender sender@example.test"} {
		if !strings.Contains(text, want) {
			t.Fatalf("Telegram text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "phish.example") || strings.Contains(text, "000000") {
		t.Fatalf("Telegram text kept untrusted model output: %q", text)
	}

	ai.mu.Lock()
	request, auth := ai.requests[0], ai.auth[0]
	ai.mu.Unlock()
	if auth != "Bearer sk-test-ai-key" || request["model"] != "test-model" {
		t.Fatalf("AI request auth=%q model=%v", auth, request["model"])
	}
	messages, _ := request["messages"].([]any)
	if len(messages) != 2 || !strings.Contains(jsonEncode(messages[1]), "build 42 passed") || !strings.Contains(jsonEncode(messages[0]), "English") {
		t.Fatalf("AI messages=%v", messages)
	}
	if strings.Contains(logs.String(), "sk-test-ai-key") {
		t.Fatal("AI API key leaked into logs")
	}
}

func TestTelegramAISummaryFallsBackWithoutLeakingContent(t *testing.T) {
	const secretBody = "Synthetic confidential body 7731"
	ai := newFakeAIServer(t, func(call int) (int, string) {
		if call == 1 {
			return http.StatusInternalServerError, `{"error":"echo: ` + secretBody + `"}`
		}
		return http.StatusOK, "Sure! " + secretBody
	})
	a, logs := newAITestApp(t, ai.URL+"/v1", nil)
	now := time.Date(2026, time.November, 4, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	telegram := newFakeTelegram(t, a, 0)
	client := configureTelegramForAdmin(t, a)
	if code := createAITelegramRule(t, client, "ai:zh-CN"); code != http.StatusCreated {
		t.Fatalf("create AI rule code=%d", code)
	}
	first, _ := insertAITestMessage(t, a, "Quarterly numbers", secretBody, now)
	second, _ := insertAITestMessage(t, a, "Quarterly numbers 2", secretBody, now)
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls() != 2 {
		t.Fatalf("AI calls=%d, want 2", ai.calls())
	}
	for _, id := range []string{first, second} {
		if status := outboxAIStatus(t, a, id); status != telegramAIStatusFallback {
			t.Fatalf("message %s ai_status=%q, want fallback", id, status)
		}
	}
	texts := telegram.delivered()
	if len(texts) != 2 {
		t.Fatalf("delivered=%v, want plain fallback notifications", texts)
	}
	for _, text := range texts {
		if !strings.HasPrefix(text, "新邮件") || strings.Contains(text, "AI") {
			t.Fatalf("fallback text=%q", text)
		}
	}
	var lastErrors string
	if err := a.db.QueryRow(`SELECT GROUP_CONCAT(last_error) FROM telegram_notification_outbox`).Scan(&lastErrors); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), secretBody) || strings.Contains(lastErrors, secretBody) {
		t.Fatalf("AI failure leaked mail content: logs=%q errors=%q", logs.String(), lastErrors)
	}
	if !strings.Contains(logs.String(), "HTTP 500") {
		t.Fatalf("logs=%q, want sanitized status", logs.String())
	}
}

func TestTelegramAISummaryQuotaAndOwnership(t *testing.T) {
	ai := newFakeAIServer(t, func(int) (int, string) {
		return http.StatusOK, `{"summary":"A synthetic status update.","priority":"low","codes":[]}`
	})
	a, _ := newAITestApp(t, ai.URL+"/v1", func(cfg *Config) { cfg.AIDailyLimitPerUser = 1 })
	now := time.Date(2026, time.November, 4, 11, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	telegram := newFakeTelegram(t, a, 0)
	client := configureTelegramForAdmin(t, a)
	if code := createAITelegramRule(t, client, "ai:zh-TW"); code != http.StatusCreated {
		t.Fatalf("create AI rule code=%d", code)
	}
	deleted, _ := insertAITestMessage(t, a, "Removed before delivery", "Synthetic body one", now)
	if _, err := a.db.Exec(`DELETE FROM messages WHERE id=?`, deleted); err != nil {
		t.Fatal(err)
	}
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls() != 0 || outboxAIStatus(t, a, deleted) != telegramAIStatusSkipped {
		t.Fatalf("deleted message: AI calls=%d status=%q", ai.calls(), outboxAIStatus(t, a, deleted))
	}

	first, _ := insertAITestMessage(t, a, "Within quota", "Synthetic body two", now)
	second, _ := insertAITestMessage(t, a, "Over quota", "Synthetic body three", now)
	if err := a.processDueTelegramNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ai.calls() != 1 {
		t.Fatalf("AI calls=%d, want 1 with daily limit 1", ai.calls())
	}
	// Both items share a timestamp, so either may be processed first.
	statuses := map[string]bool{outboxAIStatus(t, a, first): true, outboxAIStatus(t, a, second): true}
	if len(statuses) != 2 || !statuses[telegramAIStatusDone] || !statuses[telegramAIStatusSkipped] {
		t.Fatalf("statuses first=%q second=%q, want one done and one skipped", outboxAIStatus(t, a, first), outboxAIStatus(t, a, second))
	}
	texts := telegram.delivered()
	if len(texts) != 3 || !strings.Contains(strings.Join(texts, "\n"), "重要度: 低") {
		t.Fatalf("delivered=%v", texts)
	}
}

func TestAIEndpointRejectsInsecureAndPrivateTargets(t *testing.T) {
	a, _ := newAITestApp(t, "http://ai.example.test/v1", func(cfg *Config) { cfg.AIAllowPrivateHosts = false })
	if _, err := a.aiChatCompletionsURL(); err == nil {
		t.Fatal("plain HTTP AI endpoint must be rejected without AI_ALLOW_PRIVATE_HOSTS")
	}
	for _, bad := range []string{"https://user:pass@ai.example.test/v1", "https://ai.example.test/v1?key=1", "not a url"} {
		a.cfg.AIBaseURL = bad
		if _, err := a.aiChatCompletionsURL(); err == nil {
			t.Fatalf("AI base URL %q must be rejected", bad)
		}
	}

	hits := 0
	local := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer local.Close()
	a.cfg.AIBaseURL = local.URL + "/v1"
	if _, err := a.callChatCompletion(context.Background(), "system", "user"); err == nil {
		t.Fatal("loopback AI endpoint must be rejected without AI_ALLOW_PRIVATE_HOSTS")
	}
	if hits != 0 {
		t.Fatalf("loopback AI endpoint received %d requests", hits)
	}
}
