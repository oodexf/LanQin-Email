package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const externalSchemaVersion = 12

var externalSchemaTables = []string{
	"domain_collections",
	"domain_collection_audit",
	"local_delivery_jobs",
	"aliases",
	"api_tokens",
	"attachments",
	"blocked_senders",
	"contacts",
	"delivery_events",
	"domains",
	"external_imap_accounts",
	"external_imap_folder_states",
	"external_imap_messages",
	"external_imap_sync_runs",
	"folders",
	"imap_events",
	"login_challenges",
	"login_rate_limits",
	"oauth_identities",
	"oauth_login_states",
	"oauth_registration_challenges",
	"registration_invites",
	"mail_labels",
	"mail_rules",
	"mail_signatures",
	"mail_templates",
	"mailbox_share_audit_events",
	"mailbox_share_folders",
	"mailbox_share_labels",
	"mailbox_shares",
	"mailboxes",
	"mailbox_creation_events",
	"message_labels",
	"messages",
	"permission_groups",
	"pop3_events",
	"scheduled_sends",
	"schema_migrations",
	"send_as_grants",
	"send_audit_events",
	"send_idempotency_keys",
	"send_queue",
	"sent_message_dedupe_keys",
	"sessions",
	"smtp_send_events",
	"status_webhook_outbox",
	"system_settings",
	"telegram_notification_outbox",
	"telegram_notification_settings",
	"user_permission_groups",
	"users",
	"user_notifications",
	"mailbox_push_requests",
}

// initializeExternalSchema initializes an empty PostgreSQL or MySQL database.
// Existing unversioned databases are intentionally rejected: importing SQLite
// data requires an explicit data migration and must never happen at startup.
func initializeExternalSchema(ctx context.Context, db *sql.DB, driver string) error {
	driver = strings.ToLower(strings.TrimSpace(driver))
	var statements []string
	var migrationName string
	switch driver {
	case databaseDriverPostgres:
		statements = postgresFreshSchema()
		migrationName = "external_schema_v1_postgres"
	case databaseDriverMySQL:
		statements = mysqlFreshSchema()
		migrationName = "external_schema_v1_mysql"
	default:
		return fmt.Errorf("external schema: unsupported database driver %q", driver)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("external schema: reserve connection: %w", err)
	}
	defer conn.Close()

	unlock, err := lockExternalSchema(ctx, conn, driver)
	if err != nil {
		return err
	}
	defer unlock()

	tables, err := listExternalSchemaTables(ctx, conn, driver)
	if err != nil {
		return err
	}
	if len(tables) != 0 {
		if err := migrateExternalSchemaV2(ctx, conn, driver, tables, migrationName); err != nil {
			return err
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
		if err := migrateExternalSchemaV3(ctx, conn, driver, tables); err != nil {
			return err
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
		if err := migrateExternalSchemaV4(ctx, conn, driver, tables); err != nil {
			return err
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
		if err := migrateExternalSchemaV5(ctx, conn, driver, tables); err != nil {
			return err
		}
		if err := migrateExternalSchemaV6(ctx, conn, driver); err != nil {
			return err
		}
		if err := migrateExternalSchemaV7(ctx, conn, driver); err != nil {
			return err
		}
		if err := migrateExternalSchemaV8(ctx, conn, driver); err != nil {
			return err
		}
		if err := migrateExternalSchemaV9(ctx, conn, driver); err != nil {
			return err
		}
		if err := migrateExternalSchemaV10(ctx, conn, driver); err != nil {
			return err
		}
		if err := migrateExternalSchemaV11(ctx, conn, driver); err != nil {
			return err
		}
		if err := migrateExternalSchemaV12(ctx, conn, driver); err != nil {
			return err
		}
		pushTable := strings.Replace(mailboxPushExternalTable(driver), "CREATE TABLE ", "CREATE TABLE IF NOT EXISTS ", 1)
		if _, err := conn.ExecContext(ctx, pushTable); err != nil {
			return err
		}
		idx := `CREATE INDEX IF NOT EXISTS idx_mailbox_push_recipient ON mailbox_push_requests(to_user_id,status,created_at DESC)`
		if driver == databaseDriverMySQL {
			idx = `CREATE INDEX idx_mailbox_push_recipient ON mailbox_push_requests(to_user_id,status,created_at)`
		}
		if _, err := conn.ExecContext(ctx, idx); err != nil && !(driver == databaseDriverMySQL && strings.Contains(strings.ToLower(err.Error()), "duplicate key name")) {
			return err
		}
		if driver == databaseDriverPostgres {
			if _, err := conn.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_mailbox_push_one_pending ON mailbox_push_requests(mailbox_id) WHERE status='pending'`); err != nil {
				return err
			}
		} else {
			if _, err := conn.ExecContext(ctx, `ALTER TABLE mailbox_push_requests ADD COLUMN pending_mailbox_id VARCHAR(64) GENERATED ALWAYS AS (CASE WHEN status='pending' THEN mailbox_id ELSE NULL END) VIRTUAL`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				return err
			}
			if _, err := conn.ExecContext(ctx, `CREATE UNIQUE INDEX idx_mailbox_push_one_pending ON mailbox_push_requests(pending_mailbox_id)`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate key name") {
				return err
			}
		}
		tables, err = listExternalSchemaTables(ctx, conn, driver)
		if err != nil {
			return err
		}
		return validateExternalSchema(ctx, conn, tables, migrationName)
	}

	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("external schema: begin PostgreSQL migration: %w", err)
		}
		executor = tx
		defer func() { _ = tx.Rollback() }()
	}
	for i, statement := range statements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("external schema: apply v%d statement %d: %w", externalSchemaVersion, i+1, err)
		}
	}
	for _, migration := range []struct {
		version int
		name    string
	}{{1, migrationName}, {2, "external_schema_v2_linuxdo_sso"}, {3, "external_schema_v3_registration_invites"}, {4, "external_schema_v4_telegram_notifications"}, {5, "external_schema_v5_login_rate_limits"}, {6, "external_schema_v6_queue_leases"}, {7, "external_schema_v7_message_threads"}, {8, "external_schema_v8_mailbox_quota"}, {9, "external_schema_v9_invite_groups_mailbox_rate"}, {10, "external_schema_v10_session_indexes_totp_replay"}, {11, "external_schema_v11_smtp_rate_recipients"}, {12, "external_schema_v12_domain_collection"}} {
		marker := fmt.Sprintf("INSERT INTO schema_migrations(version,name,applied_at) VALUES(%d,'%s',CURRENT_TIMESTAMP)", migration.version, migration.name)
		if _, err := executor.ExecContext(ctx, marker); err != nil {
			return fmt.Errorf("external schema: record v%d: %w", migration.version, err)
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("external schema: commit PostgreSQL migration: %w", err)
		}
	}
	return nil
}

type externalSchemaExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Keep one column per line and the closing parenthesis on its own line:
// postgresTableToMySQL extracts inline references into table-level foreign keys.
func mailboxPushExternalTable(driver string) string {
	statement := `CREATE TABLE mailbox_push_requests (
			id VARCHAR(64) PRIMARY KEY,
			mailbox_id VARCHAR(64) NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
			from_user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			to_user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			status VARCHAR(16) NOT NULL,
			expires_at VARCHAR(35) NOT NULL,
			version INTEGER NOT NULL DEFAULT 1,
			created_at VARCHAR(35) NOT NULL,
			updated_at VARCHAR(35) NOT NULL,
			resolved_at VARCHAR(35),
			resolved_by VARCHAR(64),
			pending_mailbox_id VARCHAR(64) GENERATED ALWAYS AS (CASE WHEN status='pending' THEN mailbox_id ELSE NULL END) STORED
		)`
	if driver == databaseDriverMySQL {
		// The generated key depends on a column with an ON DELETE CASCADE FK.
		statement = strings.Replace(statement, "END) STORED", "END) VIRTUAL", 1)
		return postgresTableToMySQL(statement)
	}
	return statement
}

func lockExternalSchema(ctx context.Context, conn *sql.Conn, driver string) (func(), error) {
	switch driver {
	case databaseDriverPostgres:
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(4810941213515201)`); err != nil {
			return nil, fmt.Errorf("external schema: acquire PostgreSQL lock: %w", err)
		}
		return func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock(4810941213515201)`)
		}, nil
	case databaseDriverMySQL:
		var acquired sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK('lanqin_email_external_schema_v1', 30)`).Scan(&acquired); err != nil {
			return nil, fmt.Errorf("external schema: acquire MySQL lock: %w", err)
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return nil, fmt.Errorf("external schema: timed out acquiring MySQL initialization lock")
		}
		return func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = conn.ExecContext(unlockCtx, `SELECT RELEASE_LOCK('lanqin_email_external_schema_v1')`)
		}, nil
	default:
		return nil, fmt.Errorf("external schema: unsupported database driver %q", driver)
	}
}

func listExternalSchemaTables(ctx context.Context, conn *sql.Conn, driver string) ([]string, error) {
	var query string
	switch driver {
	case databaseDriverPostgres:
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' ORDER BY table_name`
	case databaseDriverMySQL:
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' ORDER BY table_name`
	default:
		return nil, fmt.Errorf("external schema: unsupported database driver %q", driver)
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("external schema: inspect database: %w", err)
	}
	defer rows.Close()
	tables := make([]string, 0, len(externalSchemaTables))
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("external schema: inspect table name: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("external schema: inspect database: %w", err)
	}
	return tables, nil
}

func validateExternalSchema(ctx context.Context, conn *sql.Conn, actual []string, migrationName string) error {
	expected := append([]string(nil), externalSchemaTables...)
	sort.Strings(expected)
	actual = append([]string(nil), actual...)
	sort.Strings(actual)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf(
			"external schema: database is not empty and does not match LanQin schema v%d; use a new empty database or run an explicit import (found tables: %s)",
			externalSchemaVersion,
			strings.Join(actual, ", "),
		)
	}

	var count, version int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &version); err != nil {
		return fmt.Errorf("external schema: read migration version: %w", err)
	}
	if count != externalSchemaVersion || version != externalSchemaVersion {
		return fmt.Errorf("external schema: unsupported migration history (count=%d, version=%d); automatic external database upgrades are disabled", count, version)
	}
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read migration marker: %w", err)
	}
	if name != migrationName {
		return fmt.Errorf("external schema: database belongs to %q, expected %q", name, migrationName)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=2`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v2 migration marker: %w", err)
	}
	if name != "external_schema_v2_linuxdo_sso" {
		return fmt.Errorf("external schema: unexpected v2 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=3`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v3 migration marker: %w", err)
	}
	if name != "external_schema_v3_registration_invites" {
		return fmt.Errorf("external schema: unexpected v3 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=4`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v4 migration marker: %w", err)
	}
	if name != "external_schema_v4_telegram_notifications" {
		return fmt.Errorf("external schema: unexpected v4 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=5`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v5 migration marker: %w", err)
	}
	if name != "external_schema_v5_login_rate_limits" {
		return fmt.Errorf("external schema: unexpected v5 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=6`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v6 migration marker: %w", err)
	}
	if name != "external_schema_v6_queue_leases" {
		return fmt.Errorf("external schema: unexpected v6 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=7`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v7 migration marker: %w", err)
	}
	if name != "external_schema_v7_message_threads" {
		return fmt.Errorf("external schema: unexpected v7 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=8`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v8 migration marker: %w", err)
	}
	if name != "external_schema_v8_mailbox_quota" {
		return fmt.Errorf("external schema: unexpected v8 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=9`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v9 migration marker: %w", err)
	}
	if name != "external_schema_v9_invite_groups_mailbox_rate" {
		return fmt.Errorf("external schema: unexpected v9 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=10`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v10 migration marker: %w", err)
	}
	if name != "external_schema_v10_session_indexes_totp_replay" {
		return fmt.Errorf("external schema: unexpected v10 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=11`).Scan(&name); err != nil {
		return fmt.Errorf("external schema: read v11 migration marker: %w", err)
	}
	if name != "external_schema_v11_smtp_rate_recipients" {
		return fmt.Errorf("external schema: unexpected v11 migration %q", name)
	}
	if err := conn.QueryRowContext(ctx, "SELECT name FROM schema_migrations WHERE version=12").Scan(&name); err != nil || name != "external_schema_v12_domain_collection" {
		return fmt.Errorf("external schema: invalid v12 migration marker")
	}
	return nil
}

// The table sets each historical schema version shipped with. They are frozen on
// purpose: the upgrade checks must describe what an old database really contains,
// not be derived from externalSchemaTables, which keeps growing with every release.
var (
	externalSchemaTablesV1 = []string{
		"aliases", "api_tokens", "attachments", "blocked_senders", "contacts", "delivery_events",
		"domains", "external_imap_accounts", "external_imap_folder_states", "external_imap_messages",
		"external_imap_sync_runs", "folders", "imap_events", "login_challenges", "mail_labels",
		"mail_rules", "mail_signatures", "mail_templates", "mailbox_share_audit_events",
		"mailbox_share_folders", "mailbox_share_labels", "mailbox_shares", "mailboxes",
		"message_labels", "messages", "permission_groups", "pop3_events", "scheduled_sends",
		"schema_migrations", "send_as_grants", "send_audit_events", "send_idempotency_keys",
		"send_queue", "sent_message_dedupe_keys", "sessions", "smtp_send_events",
		"status_webhook_outbox", "system_settings", "user_notifications", "user_permission_groups",
		"users",
	}
	externalSchemaTablesV2 = appendTables(externalSchemaTablesV1, "oauth_identities", "oauth_login_states", "oauth_registration_challenges")
	externalSchemaTablesV3 = appendTables(externalSchemaTablesV2, "registration_invites")
	externalSchemaTablesV4 = appendTables(externalSchemaTablesV3, "telegram_notification_settings", "telegram_notification_outbox")
)

func appendTables(base []string, tables ...string) []string {
	return append(append([]string(nil), base...), tables...)
}

// checkExternalSchemaTables verifies that a database at schema version `version`
// has exactly the tables that version shipped with. On MySQL, DDL commits
// implicitly, so an interrupted upgrade may already have created some of the
// tables the next migration adds; those are tolerated so the upgrade can resume.
func checkExternalSchemaTables(driver string, version int, actual, want, adding []string) error {
	expected := map[string]bool{}
	for _, table := range want {
		expected[table] = true
	}
	resumable := map[string]bool{}
	if driver == databaseDriverMySQL {
		for _, table := range adding {
			resumable[table] = true
		}
	}
	found := map[string]bool{}
	mismatch := false
	for _, table := range actual {
		found[table] = true
		if !expected[table] && !resumable[table] {
			mismatch = true
		}
	}
	for _, table := range want {
		if !found[table] {
			mismatch = true
		}
	}
	if mismatch {
		got := append([]string(nil), actual...)
		sort.Strings(got)
		return fmt.Errorf("external schema: database does not match LanQin schema v%d (found tables: %s)", version, strings.Join(got, ", "))
	}
	return nil
}

// externalSchemaHistory reads the migration counter and requires it to be either
// past `version` (already applied) or exactly one step before it.
func externalSchemaHistory(ctx context.Context, conn *sql.Conn, version int) (applied bool, err error) {
	var count, current int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&count, &current); err != nil {
		return false, fmt.Errorf("external schema: read migration version: %w", err)
	}
	if count >= version && current >= version {
		return true, nil
	}
	if count != version-1 || current != version-1 {
		return false, fmt.Errorf("external schema: unsupported migration history before v%d (count=%d, version=%d)", version, count, current)
	}
	return false, nil
}

// applyExternalSchemaMigration runs one migration step and records its marker.
//
// PostgreSQL runs the step in a transaction. MySQL cannot: every CREATE/ALTER
// commits on its own, so a crash can leave part of a step applied without its
// marker. There, objects that already exist are skipped, which lets the next
// start finish the step instead of failing on it forever. Every step must
// therefore consist of CREATE TABLE, ADD COLUMN, CREATE INDEX or idempotent data
// statements.
func applyExternalSchemaMigration(ctx context.Context, conn *sql.Conn, driver string, version int, name string, statements []string) error {
	var executor externalSchemaExecutor = conn
	var tx *sql.Tx
	if driver == databaseDriverPostgres {
		var err error
		tx, err = conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("external schema: begin v%d PostgreSQL migration: %w", version, err)
		}
		executor = tx
		defer func() { _ = tx.Rollback() }()
	}
	for i, statement := range statements {
		if driver == databaseDriverMySQL && strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE ") {
			statement = postgresTableToMySQL(statement)
		}
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			if driver == databaseDriverMySQL && isMySQLObjectExists(err) {
				continue
			}
			return fmt.Errorf("external schema: apply v%d statement %d: %w", version, i+1, err)
		}
	}
	if _, err := executor.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,CURRENT_TIMESTAMP)`, version, name); err != nil {
		return fmt.Errorf("external schema: record v%d: %w", version, err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("external schema: commit v%d PostgreSQL migration: %w", version, err)
		}
	}
	return nil
}

// isMySQLObjectExists reports the errors a re-run CREATE TABLE, ADD COLUMN or
// CREATE INDEX returns once the object exists: ER_TABLE_EXISTS_ERROR,
// ER_DUP_FIELDNAME and ER_DUP_KEYNAME.
func isMySQLObjectExists(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	switch mysqlErr.Number {
	case 1050, 1060, 1061:
		return true
	}
	return false
}

func migrateExternalSchemaV2(ctx context.Context, conn *sql.Conn, driver string, actual []string, migrationName string) error {
	applied, err := externalSchemaHistory(ctx, conn, 2)
	if err != nil || applied {
		return err
	}
	if err := checkExternalSchemaTables(driver, 1, actual, externalSchemaTablesV1, externalSchemaTablesV2[len(externalSchemaTablesV1):]); err != nil {
		return err
	}
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version=1`).Scan(&name); err != nil || name != migrationName {
		return fmt.Errorf("external schema: invalid v1 migration marker")
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 2, "external_schema_v2_linuxdo_sso", externalSchemaV2Statements)
}

func migrateExternalSchemaV3(ctx context.Context, conn *sql.Conn, driver string, actual []string) error {
	applied, err := externalSchemaHistory(ctx, conn, 3)
	if err != nil || applied {
		return err
	}
	if err := checkExternalSchemaTables(driver, 2, actual, externalSchemaTablesV2, externalSchemaTablesV3[len(externalSchemaTablesV2):]); err != nil {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 3, "external_schema_v3_registration_invites", externalSchemaV3Statements)
}

func migrateExternalSchemaV4(ctx context.Context, conn *sql.Conn, driver string, actual []string) error {
	applied, err := externalSchemaHistory(ctx, conn, 4)
	if err != nil || applied {
		return err
	}
	if err := checkExternalSchemaTables(driver, 3, actual, externalSchemaTablesV3, externalSchemaTablesV4[len(externalSchemaTablesV3):]); err != nil {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 4, "external_schema_v4_telegram_notifications", externalSchemaV4Statements)
}

func migrateExternalSchemaV5(ctx context.Context, conn *sql.Conn, driver string, actual []string) error {
	applied, err := externalSchemaHistory(ctx, conn, 5)
	if err != nil || applied {
		return err
	}
	if err := checkExternalSchemaTables(driver, 4, actual, externalSchemaTablesV4, []string{"login_rate_limits"}); err != nil {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 5, "external_schema_v5_login_rate_limits", externalSchemaV5Statements)
}

// The statements of migrations V2 to V5, as those versions created them. Later
// columns are added by later migrations (lease columns by V6, invite groups by
// V9), so these must not be replaced with the current CREATE TABLE statements.
var (
	externalSchemaV2Statements = []string{
		`CREATE TABLE oauth_identities (
			provider VARCHAR(32) NOT NULL,
			subject VARCHAR(255) NOT NULL,
			user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			username VARCHAR(255) NOT NULL DEFAULT '',
			created_at VARCHAR(35) NOT NULL,
			updated_at VARCHAR(35) NOT NULL,
			PRIMARY KEY(provider,subject),
			UNIQUE(user_id,provider)
		)`,
		`CREATE INDEX idx_oauth_identities_user ON oauth_identities(user_id,provider)`,
		`CREATE TABLE oauth_login_states (
			token_hash VARCHAR(255) PRIMARY KEY,
			purpose VARCHAR(16) NOT NULL CHECK(purpose IN ('login','link')),
			user_id VARCHAR(64) REFERENCES users(id) ON DELETE CASCADE,
			expires_at VARCHAR(35) NOT NULL,
			created_at VARCHAR(35) NOT NULL
		)`,
		`CREATE INDEX idx_oauth_login_states_expires ON oauth_login_states(expires_at)`,
		`CREATE TABLE oauth_registration_challenges (
			token_hash VARCHAR(255) PRIMARY KEY,
			provider VARCHAR(32) NOT NULL,
			subject VARCHAR(255) NOT NULL,
			username VARCHAR(255) NOT NULL DEFAULT '',
			display_name VARCHAR(255) NOT NULL DEFAULT '',
			expires_at VARCHAR(35) NOT NULL,
			created_at VARCHAR(35) NOT NULL
		)`,
		`CREATE INDEX idx_oauth_registration_challenges_expires ON oauth_registration_challenges(expires_at)`,
	}
	// V3 originally declared the used_count <= max_uses check on the column,
	// which MySQL rejects; the table-level form is equivalent and works on both.
	externalSchemaV3Statements = []string{
		`CREATE TABLE registration_invites (
			id VARCHAR(64) PRIMARY KEY,
			code VARCHAR(64) NOT NULL UNIQUE,
			max_uses INTEGER NOT NULL CHECK(max_uses > 0),
			used_count INTEGER NOT NULL DEFAULT 0 CHECK(used_count >= 0),
			created_by VARCHAR(64) REFERENCES users(id) ON DELETE SET NULL,
			created_at VARCHAR(35) NOT NULL,
			updated_at VARCHAR(35) NOT NULL,
			CHECK(used_count <= max_uses)
		)`,
		`CREATE INDEX idx_registration_invites_created ON registration_invites(created_at)`,
	}
	externalSchemaV4Statements = []string{
		`CREATE TABLE telegram_notification_settings (
			user_id VARCHAR(64) PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			bot_token_ciphertext TEXT NOT NULL,
			bot_username VARCHAR(255) NOT NULL DEFAULT '',
			chat_id VARCHAR(255) NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			last_test_at VARCHAR(35),
			last_delivered_at VARCHAR(35),
			last_error TEXT NOT NULL DEFAULT '',
			created_at VARCHAR(35) NOT NULL,
			updated_at VARCHAR(35) NOT NULL
		)`,
		`CREATE TABLE telegram_notification_outbox (
			id VARCHAR(64) PRIMARY KEY,
			event_key VARCHAR(255) NOT NULL UNIQUE,
			user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id VARCHAR(64) NOT NULL,
			message_id VARCHAR(64) NOT NULL,
			rule_id VARCHAR(64) NOT NULL,
			payload_json TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_attempt_at VARCHAR(35) NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at VARCHAR(35) NOT NULL,
			updated_at VARCHAR(35) NOT NULL,
			delivered_at VARCHAR(35)
		)`,
		`CREATE INDEX idx_telegram_notification_outbox_due ON telegram_notification_outbox(delivered_at,next_attempt_at,created_at)`,
		`CREATE INDEX idx_telegram_notification_outbox_user ON telegram_notification_outbox(user_id,created_at)`,
	}
	externalSchemaV5Statements = []string{
		`CREATE TABLE login_rate_limits (
			scope VARCHAR(16) NOT NULL CHECK(scope IN ('account','ip')),
			subject_hash VARCHAR(64) NOT NULL,
			failure_count INTEGER NOT NULL CHECK(failure_count > 0),
			window_started_at VARCHAR(35) NOT NULL,
			blocked_until VARCHAR(35) NOT NULL DEFAULT '',
			updated_at VARCHAR(35) NOT NULL,
			PRIMARY KEY(scope,subject_hash)
		)`,
		`CREATE INDEX idx_login_rate_limits_updated ON login_rate_limits(updated_at)`,
	}
)

func migrateExternalSchemaV6(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 6)
	if err != nil || applied {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 6, "external_schema_v6_queue_leases", externalSchemaV6Statements)
}

var externalSchemaV6Statements = []string{
	`ALTER TABLE send_queue ADD COLUMN lease_owner VARCHAR(128) NOT NULL DEFAULT ''`,
	`ALTER TABLE send_queue ADD COLUMN lease_token VARCHAR(128) NOT NULL DEFAULT ''`,
	`ALTER TABLE send_queue ADD COLUMN lease_until VARCHAR(35)`,
	`ALTER TABLE status_webhook_outbox ADD COLUMN lease_owner VARCHAR(128) NOT NULL DEFAULT ''`,
	`ALTER TABLE status_webhook_outbox ADD COLUMN lease_token VARCHAR(128) NOT NULL DEFAULT ''`,
	`ALTER TABLE status_webhook_outbox ADD COLUMN lease_until VARCHAR(35)`,
	`ALTER TABLE telegram_notification_outbox ADD COLUMN lease_owner VARCHAR(128) NOT NULL DEFAULT ''`,
	`ALTER TABLE telegram_notification_outbox ADD COLUMN lease_token VARCHAR(128) NOT NULL DEFAULT ''`,
	`ALTER TABLE telegram_notification_outbox ADD COLUMN lease_until VARCHAR(35)`,
}

func migrateExternalSchemaV7(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 7)
	if err != nil || applied {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 7, "external_schema_v7_message_threads", []string{
		`ALTER TABLE messages ADD COLUMN thread_id VARCHAR(512) NOT NULL DEFAULT ''`,
	})
}

// migrateExternalSchemaV8 adds the per-user mailbox counters.
//
// Existing rows are backfilled from the current mailbox count. Mailboxes deleted
// before this migration ran leave no trace, so the backfilled total is a lower
// bound for pre-existing accounts. That is an accepted one-time imprecision.
func migrateExternalSchemaV8(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 8)
	if err != nil || applied {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 8, "external_schema_v8_mailbox_quota", []string{
		`ALTER TABLE users ADD COLUMN mailboxes_created_total INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN mailbox_quota_bonus INTEGER NOT NULL DEFAULT 0`,
		`UPDATE users SET mailboxes_created_total =
			(SELECT COUNT(*) FROM mailboxes WHERE mailboxes.user_id = users.id)`,
	})
}

// migrateExternalSchemaV9 adds the invite-to-permission-group binding and the
// mailbox creation event log used by the per-day creation limit.
//
// mailbox_creation_events needs no backfill: an empty log means nobody has used
// today's allowance yet, which is the correct post-upgrade state.
func migrateExternalSchemaV9(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 9)
	if err != nil || applied {
		return err
	}
	// The fresh MySQL schema declares TEXT columns as LONGTEXT; match it.
	textType := "TEXT"
	if driver == databaseDriverMySQL {
		textType = "LONGTEXT"
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 9, "external_schema_v9_invite_groups_mailbox_rate", []string{
		// MySQL requires expression defaults for TEXT; parentheses also work in PostgreSQL.
		`ALTER TABLE registration_invites ADD COLUMN permission_group_ids_json ` + textType + ` NOT NULL DEFAULT ('[]')`,
		`CREATE TABLE mailbox_creation_events (
			id VARCHAR(64) PRIMARY KEY,
			user_id VARCHAR(64) NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			mailbox_id VARCHAR(64) NOT NULL,
			created_at VARCHAR(35) NOT NULL
		)`,
		`CREATE INDEX idx_mailbox_creation_events_user ON mailbox_creation_events(user_id,created_at)`,
	})
}

// migrateExternalSchemaV10 indexes sessions and adds the TOTP replay guard.
//
// Only token_hash was indexed, so revoking a user's sessions and pruning expired
// rows both had to scan the whole table — and the new cleanup worker does the latter
// on a timer. two_factor_last_counter records the last accepted TOTP step so a code
// cannot be presented twice inside its acceptance window.
func migrateExternalSchemaV10(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 10)
	if err != nil || applied {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 10, "external_schema_v10_session_indexes_totp_replay", []string{
		`CREATE INDEX idx_sessions_user ON sessions(user_id)`,
		`CREATE INDEX idx_sessions_expires ON sessions(expires_at)`,
		`ALTER TABLE users ADD COLUMN two_factor_last_counter BIGINT NOT NULL DEFAULT 0`,
	})
}

// migrateExternalSchemaV11 charges the SMTP daily allowance per recipient.
//
// Existing rows default to 1, which is what they always meant: the allowance used to
// be charged once per message no matter how many addresses it carried.
func migrateExternalSchemaV11(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 11)
	if err != nil || applied {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 11, "external_schema_v11_smtp_rate_recipients", []string{
		`ALTER TABLE smtp_send_events ADD COLUMN recipients INTEGER NOT NULL DEFAULT 1`,
	})
}
