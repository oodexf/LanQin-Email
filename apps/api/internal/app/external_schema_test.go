package app

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestExternalFreshSchemasCoverAllTables(t *testing.T) {
	t.Parallel()
	want := append([]string(nil), externalSchemaTables...)
	sort.Strings(want)

	for name, statements := range map[string][]string{
		"postgres": postgresFreshSchema(),
		"mysql":    mysqlFreshSchema(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := schemaTableNames(statements)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("schema tables differ\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

func TestPostgresFreshSchemaPreservesConditionalIndexes(t *testing.T) {
	t.Parallel()
	schema := strings.Join(postgresFreshSchema(), "\n")
	for _, fragment := range []string{
		"idx_messages_mailbox_raw_path",
		"idx_messages_unregistered_raw_path",
		"idx_messages_folder_imap_uid",
		"idx_send_queue_mailbox_source_message_id",
		"WHERE raw_path<>''",
		"queue_id VARCHAR(64) NOT NULL REFERENCES send_queue(id) ON DELETE CASCADE",
		"mailbox_id VARCHAR(64) NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(schema, fragment) {
			t.Errorf("PostgreSQL schema is missing %q", fragment)
		}
	}
}

func TestMySQLFreshSchemaUsesPortableIndexesAndBinaryCollation(t *testing.T) {
	t.Parallel()
	statements := mysqlFreshSchema()
	schema := strings.Join(statements, "\n")

	for _, forbidden := range []string{
		"CREATE INDEX IF NOT EXISTS",
		"CREATE UNIQUE INDEX IF NOT EXISTS",
		"CREATE FUNCTION ",
		"CREATE TRIGGER ",
		"ALTER TABLE messages",
		"ALTER TABLE send_queue",
		"CREATE INDEX idx_messages_mailbox_raw_path ON messages(mailbox_id,raw_path)",
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("MySQL schema contains unsupported SQL %q", forbidden)
		}
	}
	for _, statement := range statements {
		trimmed := strings.TrimSpace(statement)
		if !strings.HasPrefix(trimmed, "CREATE TABLE ") && !strings.HasPrefix(trimmed, "CREATE INDEX ") && !strings.HasPrefix(trimmed, "CREATE UNIQUE INDEX ") {
			t.Errorf("MySQL schema contains unexpected or privileged DDL: %.80s", trimmed)
		}
		if strings.HasPrefix(trimmed, "CREATE TABLE ") && !strings.HasSuffix(trimmed, "ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin") {
			t.Errorf("MySQL table is missing engine or binary collation: %.80s", trimmed)
		}
		if (strings.HasPrefix(trimmed, "CREATE INDEX ") || strings.HasPrefix(trimmed, "CREATE UNIQUE INDEX ")) && strings.Contains(trimmed, " WHERE ") {
			t.Errorf("MySQL schema contains a partial index: %s", trimmed)
		}
		for _, line := range strings.Split(statement, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, " REFERENCES ") && !strings.HasPrefix(line, "FOREIGN KEY ") {
				t.Errorf("MySQL inline REFERENCES would not enforce a foreign key: %s", line)
			}
		}
	}
	for _, fragment := range []string{
		"CREATE TABLE system_settings (\n\t\t\t`key` VARCHAR(255) PRIMARY KEY",
		"CREATE TABLE mail_templates (\n\t\t\t`key` VARCHAR(255) PRIMARY KEY",
		"`system` INTEGER NOT NULL DEFAULT 0",
		"mailbox_raw_path_key BINARY(32)",
		"unregistered_raw_path_key BINARY(32)",
		"positive_imap_uid BIGINT GENERATED ALWAYS",
		"dedupe_message_id VARCHAR(512)",
		"FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE",
		"FOREIGN KEY (queue_id) REFERENCES send_queue(id) ON DELETE CASCADE",
		"FOREIGN KEY (mailbox_id) REFERENCES mailboxes(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(schema, fragment) {
			t.Errorf("MySQL schema is missing %q", fragment)
		}
	}
	var messagesTable string
	for _, statement := range statements {
		if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE messages ") {
			messagesTable = statement
			break
		}
	}
	if generated, foreignKey := strings.Index(messagesTable, "mailbox_raw_path_key BINARY(32)"), strings.Index(messagesTable, "FOREIGN KEY (mailbox_id) REFERENCES mailboxes(id)"); generated < 0 || foreignKey < 0 || generated > foreignKey {
		t.Error("MySQL messages generated indexes must be defined before its foreign keys")
	}
	if count := strings.Count(messagesTable, ") VIRTUAL"); count != 3 || strings.Contains(messagesTable, ") STORED") {
		t.Errorf("MySQL messages generated columns must be virtual; virtual count=%d", count)
	}
}

func TestLinuxDoExternalSchemaContract(t *testing.T) {
	t.Parallel()
	postgres := strings.Join(postgresFreshSchema(), "\n")
	mysql := strings.Join(mysqlFreshSchema(), "\n")
	for name, schema := range map[string]string{"postgres": postgres, "mysql": mysql} {
		for _, fragment := range []string{
			"CREATE TABLE oauth_identities",
			"PRIMARY KEY(provider,subject)",
			"UNIQUE(user_id,provider)",
			"CREATE TABLE oauth_login_states",
			"CREATE TABLE oauth_registration_challenges",
			"idx_oauth_login_states_expires",
			"idx_oauth_registration_challenges_expires",
		} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s Linux.do schema is missing %q", name, fragment)
			}
		}
	}
	statements := externalSchemaV2Statements
	if len(statements) != 6 {
		t.Fatalf("v2 migration has %d statements, want 6", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "oauth_") {
			t.Errorf("v2 migration contains unrelated statement: %.80s", statement)
		}
	}
}

func TestRegistrationInviteExternalSchemaContract(t *testing.T) {
	t.Parallel()
	postgres := strings.Join(postgresFreshSchema(), "\n")
	mysql := strings.Join(mysqlFreshSchema(), "\n")
	for name, schema := range map[string]string{"postgres": postgres, "mysql": mysql} {
		for _, fragment := range []string{"CREATE TABLE registration_invites", "code VARCHAR(64) NOT NULL UNIQUE", "CHECK(max_uses > 0)", "idx_registration_invites_created"} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s registration invite schema is missing %q", name, fragment)
			}
		}
	}
	for name, schema := range map[string]string{"postgres": postgres, "mysql": mysql} {
		if !strings.Contains(schema, "CHECK(used_count >= 0)") || !strings.Contains(schema, "CHECK(used_count <= max_uses)") {
			t.Errorf("%s registration invite schema is missing portable usage constraints", name)
		}
	}
	statements := externalSchemaV3Statements
	if len(statements) != 2 {
		t.Fatalf("v3 migration has %d statements, want 2", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "registration_invites") {
			t.Errorf("v3 migration contains unrelated statement: %.80s", statement)
		}
		// V9 adds this column; creating it here as well breaks the V9 upgrade.
		if strings.Contains(statement, "permission_group_ids_json") {
			t.Errorf("v3 migration must create the v3 table, not the current one: %.80s", statement)
		}
	}
}

func TestTelegramNotificationExternalSchemaContract(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string]string{
		"postgres": strings.Join(postgresFreshSchema(), "\n"),
		"mysql":    strings.Join(mysqlFreshSchema(), "\n"),
	} {
		for _, fragment := range []string{
			"CREATE TABLE telegram_notification_settings",
			"CREATE TABLE telegram_notification_outbox",
			"event_key VARCHAR(255) NOT NULL UNIQUE",
			"idx_telegram_notification_outbox_due",
			"idx_telegram_notification_outbox_user",
			"ai_status VARCHAR(16) NOT NULL DEFAULT ''",
		} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s Telegram notification schema is missing %q", name, fragment)
			}
		}
	}

	statements := externalSchemaV4Statements
	if len(statements) != 4 {
		t.Fatalf("v4 migration has %d statements, want 4", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "telegram_notification_") {
			t.Errorf("v4 migration contains unrelated statement: %.80s", statement)
		}
		// V6 adds the lease columns and V14 adds ai_status; creating them here as well breaks those upgrades.
		if strings.Contains(statement, "lease_") || strings.Contains(statement, "ai_status") {
			t.Errorf("v4 migration must create the v4 table, not the current one: %.80s", statement)
		}
	}
}

func TestLoginRateLimitExternalSchemaContract(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string]string{
		"postgres": strings.Join(postgresFreshSchema(), "\n"),
		"mysql":    strings.Join(mysqlFreshSchema(), "\n"),
	} {
		for _, fragment := range []string{
			"CREATE TABLE login_rate_limits",
			"PRIMARY KEY(scope,subject_hash)",
			"CHECK(scope IN ('account','ip'))",
			"idx_login_rate_limits_updated",
		} {
			if !strings.Contains(schema, fragment) {
				t.Errorf("%s login rate-limit schema is missing %q", name, fragment)
			}
		}
	}

	statements := externalSchemaV5Statements
	if len(statements) != 2 {
		t.Fatalf("v5 migration has %d statements, want 2", len(statements))
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "login_rate_limits") {
			t.Errorf("v5 migration contains unrelated statement: %.80s", statement)
		}
	}
}

func TestQueueLeaseExternalSchemaContract(t *testing.T) {
	t.Parallel()
	for name, schema := range map[string][]string{
		"postgres": postgresFreshSchema(),
		"mysql":    mysqlFreshSchema(),
	} {
		for _, table := range []string{"send_queue", "status_webhook_outbox", "telegram_notification_outbox"} {
			fragment := ""
			for _, statement := range schema {
				if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE "+table+" ") {
					fragment = statement
					break
				}
			}
			if fragment == "" {
				t.Fatalf("%s schema is missing %s", name, table)
			}
			for _, column := range []string{"lease_owner", "lease_token", "lease_until"} {
				if !strings.Contains(fragment, column) {
					t.Errorf("%s %s is missing %s", name, table, column)
				}
			}
		}
	}

	statements := externalSchemaV6Statements
	if len(statements) != 9 {
		t.Fatalf("v6 migration has %d statements, want 9", len(statements))
	}
	for _, table := range []string{"send_queue", "status_webhook_outbox", "telegram_notification_outbox"} {
		count := 0
		for _, statement := range statements {
			if strings.Contains(statement, "ALTER TABLE "+table+" ADD COLUMN lease_") {
				count++
			}
		}
		if count != 3 {
			t.Errorf("v6 migration has %d lease columns for %s, want 3", count, table)
		}
	}
}

func schemaTableNames(statements []string) []string {
	names := make([]string, 0, len(externalSchemaTables))
	for _, statement := range statements {
		fields := strings.Fields(statement)
		if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") && strings.EqualFold(fields[1], "TABLE") {
			names = append(names, strings.Trim(fields[2], "`\""))
		}
	}
	return names
}

// TestEveryMigratedColumnExistsInFreshSchema is a structural guard against the trap
// CLAUDE.md warns about: adding a column to the migration chain but not to the fresh
// schema leaves upgraded databases correct while every *new* MySQL/PostgreSQL install
// crashes on the missing column. Local SQLite development cannot surface it, and the
// per-feature contract tests only cover columns somebody remembered to add them for.
//
// The migration statements live as literals inside external_schema.go, so they are read
// back from the source. That keeps the check automatic for future migrations rather than
// something each one has to opt into.
func TestEveryMigratedColumnExistsInFreshSchema(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("external_schema.go")
	if err != nil {
		t.Fatal(err)
	}
	alter := regexp.MustCompile("ALTER TABLE ([a-z_]+) ADD COLUMN ([a-z_]+)")
	matches := alter.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("found no ALTER TABLE ... ADD COLUMN statements; has the migration chain moved?")
	}
	for name, schema := range map[string][]string{
		"postgres": postgresFreshSchema(),
		"mysql":    mysqlFreshSchema(),
	} {
		for _, match := range matches {
			table, column := match[1], match[2]
			fragment := ""
			for _, statement := range schema {
				if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE "+table+" ") {
					fragment = statement
					break
				}
			}
			if fragment == "" {
				t.Errorf("%s fresh schema is missing table %s, which a migration alters", name, table)
				continue
			}
			if !columnDeclaredIn(fragment, column) {
				t.Errorf("%s fresh schema: %s.%s is added by a migration but absent from CREATE TABLE — new installs will fail on it", name, table, column)
			}
		}
	}
}

// columnDeclaredIn looks for a column declaration rather than a bare substring, so a
// name that merely appears inside another column, an index or a default value does not
// count as declared.
func columnDeclaredIn(createTable, column string) bool {
	for _, line := range strings.Split(createTable, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == column {
			return true
		}
	}
	return false
}

func TestExternalMailV13SchemaAndSQLiteUpgrade(t *testing.T) {
	accountColumns := []string{"provider_preset", "auth_status", "oauth_subject", "oauth_generation", "smtp_host", "smtp_port", "smtp_tls_mode", "smtp_username", "smtp_password_ciphertext", "send_enabled", "sent_copy_mode"}
	queueColumns := []string{"route", "external_account_id", "sent_copy_status", "sent_copy_attempt_count", "subject"}
	for driver, statements := range map[string][]string{"postgres": postgresFreshSchema(), "mysql": mysqlFreshSchema()} {
		for table, columns := range map[string][]string{"external_imap_accounts": accountColumns, "send_queue": queueColumns} {
			var create string
			for _, sql := range statements {
				if strings.HasPrefix(sql, "CREATE TABLE "+table+" (") {
					create = sql
					break
				}
			}
			for _, col := range columns {
				if !columnDeclaredIn(create, col) {
					t.Errorf("%s missing %s.%s", driver, table, col)
				}
			}
			if strings.Contains(create, "external_account_id TEXT REFERENCES") || strings.Contains(create, "FOREIGN KEY (external_account_id)") {
				t.Fatal("send route account must survive account deletion")
			}
		}
		joined := strings.Join(externalSchemaV13Statements(driver), "\n")
		if !strings.Contains(joined, "CREATE TABLE external_mail_oauth_states") || !strings.Contains(joined, "idx_external_mail_oauth_states_expires") {
			t.Fatal("V13 state migration missing")
		}
		if driver == "mysql" && !strings.Contains(joined, "ADD COLUMN smtp_password_ciphertext LONGTEXT NOT NULL DEFAULT ('')") {
			t.Fatal("MySQL upgrade differs from fresh LONGTEXT defaults")
		}
	}
	// Historical table sets must stay frozen when the current set grows.
	for _, tables := range [][]string{externalSchemaTablesV1, externalSchemaTablesV2, externalSchemaTablesV3, externalSchemaTablesV4} {
		for _, table := range tables {
			if table == "external_mail_oauth_states" {
				t.Fatal("V13 table contaminated a historical table set")
			}
		}
	}
	a := newTestApp(t)
	stopTestWorkers(a)
	if _, err := a.db.Exec(`DROP TABLE external_mail_oauth_states`); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{"external_imap_accounts": accountColumns, "send_queue": queueColumns} {
		for _, col := range columns {
			if _, err := a.db.Exec("ALTER TABLE " + table + " DROP COLUMN " + col); err != nil {
				t.Fatal(err)
			}
		}
	}
	for range 2 {
		if err := a.migrate(t.Context()); err != nil {
			t.Fatalf("SQLite resumable upgrade: %v", err)
		}
	}
	_, mb := defaultAdminUserAndMailbox(t, a)
	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO external_imap_accounts(id,user_id,mailbox_id,name,host,port,tls_mode,username,password_ciphertext,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "schema-v13", mb.UserID, mb.ID, "Synthetic", "imap.example.test", 993, "tls", "person@example.test", "synthetic", now, now); err != nil {
		t.Fatal(err)
	}
	var preset, status, subject, smtpHost, tls, username, password, sentMode string
	var generation, port, enabled int
	if err := a.db.QueryRow(`SELECT provider_preset,auth_status,oauth_subject,oauth_generation,smtp_host,smtp_port,smtp_tls_mode,smtp_username,smtp_password_ciphertext,send_enabled,sent_copy_mode FROM external_imap_accounts WHERE id='schema-v13'`).Scan(&preset, &status, &subject, &generation, &smtpHost, &port, &tls, &username, &password, &enabled, &sentMode); err != nil || preset != "custom" || status != "ok" || subject != "" || generation != 0 || smtpHost != "" || port != 0 || tls != "" || username != "" || password != "" || enabled != 0 || sentMode != "server" {
		t.Fatal("SQLite V13 defaults differ", err)
	}
	if _, err := a.db.Exec(`INSERT INTO external_mail_oauth_states(token_hash,user_id,binding_hash,provider,mailbox_id,payload_ciphertext,expires_at,created_at) VALUES('synthetic',?,'binding','gmail',?,'encrypted',?,?)`, mb.UserID, mb.ID, now, now); err != nil {
		t.Fatal("SQLite state table missing", err)
	}
}
