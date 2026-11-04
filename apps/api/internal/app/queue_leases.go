package app

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	outboxLeaseDuration    = time.Minute
	sendQueueLeaseDuration = 15 * time.Minute
)

func (a *App) migrateQueueLeases(ctx context.Context) error {
	for _, table := range []string{"send_queue", "status_webhook_outbox", "telegram_notification_outbox"} {
		columns, err := sqliteTableColumns(ctx, a.db, table)
		if err != nil {
			return err
		}
		for _, column := range []struct{ name, definition string }{
			{"lease_owner", "TEXT NOT NULL DEFAULT ''"},
			{"lease_token", "TEXT NOT NULL DEFAULT ''"},
			{"lease_until", "TEXT"},
		} {
			if columns[column.name] {
				continue
			}
			if _, err := a.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column.name, column.definition)); err != nil {
				return fmt.Errorf("add %s.%s: %w", table, column.name, err)
			}
		}
	}
	return nil
}

// migrateTelegramAIStatus adds the per-item AI summary state to SQLite databases
// created before AI summaries existed. Existing rows default to "" (no AI).
func (a *App) migrateTelegramAIStatus(ctx context.Context) error {
	columns, err := sqliteTableColumns(ctx, a.db, "telegram_notification_outbox")
	if err != nil {
		return err
	}
	if columns["ai_status"] {
		return nil
	}
	if _, err := a.db.ExecContext(ctx, `ALTER TABLE telegram_notification_outbox ADD COLUMN ai_status TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add telegram_notification_outbox.ai_status: %w", err)
	}
	return nil
}

func sqliteTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}
