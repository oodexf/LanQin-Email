package app

import (
	"strings"
	"testing"
)

func TestInsertIgnoreSQL(t *testing.T) {
	base := `INSERT INTO message_labels(message_id,label_id) VALUES(?,?)`
	tests := []struct {
		driver string
		want   string
	}{
		{driver: databaseDriverSQLite, want: `INSERT OR IGNORE INTO message_labels(message_id,label_id) VALUES(?,?)`},
		{driver: databaseDriverMySQL, want: `INSERT IGNORE INTO message_labels(message_id,label_id) VALUES(?,?)`},
		{driver: databaseDriverPostgres, want: `INSERT INTO message_labels(message_id,label_id) VALUES(?,?) ON CONFLICT (message_id,label_id) DO NOTHING`},
	}
	for _, tt := range tests {
		t.Run(tt.driver, func(t *testing.T) {
			if got := insertIgnoreSQL(tt.driver, base, `(message_id,label_id)`); got != tt.want {
				t.Fatalf("insertIgnoreSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpsertSQL(t *testing.T) {
	base := `INSERT INTO system_settings(key,value) VALUES(?,?)`
	common := `value=excluded.value`
	mysql := `value=VALUES(value)`

	for _, driver := range []string{databaseDriverSQLite, databaseDriverPostgres} {
		got := upsertSQL(driver, base, `(key)`, common, mysql)
		want := base + ` ON CONFLICT (key) DO UPDATE SET ` + common
		if got != want {
			t.Fatalf("upsertSQL(%q) = %q, want %q", driver, got, want)
		}
	}
	if got, want := upsertSQL(databaseDriverMySQL, base, `(key)`, common, mysql), base+` ON DUPLICATE KEY UPDATE `+mysql; got != want {
		t.Fatalf("upsertSQL(mysql) = %q, want %q", got, want)
	}
}

func TestDialectScalarFunctions(t *testing.T) {
	if got := scalarMaxSQL(databaseDriverSQLite, "uid_next", "?"); got != "MAX(uid_next,?)" {
		t.Fatalf("SQLite scalar max = %q", got)
	}
	for _, driver := range []string{databaseDriverMySQL, databaseDriverPostgres} {
		if got := scalarMaxSQL(driver, "uid_next", "?"); got != "GREATEST(uid_next,?)" {
			t.Fatalf("%s scalar max = %q", driver, got)
		}
	}
	if got := permissionGroupRenameSQL(databaseDriverMySQL); !strings.Contains(got, "CONCAT(") || strings.Contains(got, "||") {
		t.Fatalf("MySQL rename query is not using CONCAT: %q", got)
	}
	if got := permissionGroupSystemColumnSQL(databaseDriverMySQL); got != "`system`" {
		t.Fatalf("MySQL system column = %q", got)
	}
	for _, driver := range []string{databaseDriverSQLite, databaseDriverPostgres} {
		if got := permissionGroupSystemColumnSQL(driver); got != "system" {
			t.Fatalf("%s system column = %q", driver, got)
		}
	}
	if got := keyColumnSQL(databaseDriverMySQL); got != "`key`" {
		t.Fatalf("MySQL key column = %q", got)
	}
	for _, driver := range []string{databaseDriverSQLite, databaseDriverPostgres} {
		if got := keyColumnSQL(driver); got != "key" {
			t.Fatalf("%s key column = %q", driver, got)
		}
	}
	if got := apiTokenLastUsedUpdateSQL(databaseDriverPostgres); strings.Contains(got, "julianday") || !strings.Contains(got, "last_used_at < ?") {
		t.Fatalf("Postgres token update uses incompatible date comparison: %q", got)
	}
	if got := apiTokenLastUsedUpdateSQL(databaseDriverSQLite); !strings.Contains(got, "julianday") {
		t.Fatalf("SQLite token update changed its date comparison: %q", got)
	}
}

func TestSQLDialectAliases(t *testing.T) {
	tests := map[string]string{
		"":           databaseDriverSQLite,
		"sqlite3":    databaseDriverSQLite,
		"mysql":      databaseDriverMySQL,
		"postgresql": databaseDriverPostgres,
		"PG":         databaseDriverPostgres,
	}
	for input, want := range tests {
		if got := sqlDialect(input); got != want {
			t.Fatalf("sqlDialect(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGroupConcatSQL(t *testing.T) {
	tests := []struct {
		driver string
		want   string
	}{
		{driver: databaseDriverSQLite, want: `GROUP_CONCAT(mb.address)`},
		{driver: databaseDriverMySQL, want: `GROUP_CONCAT(mb.address)`},
		{driver: databaseDriverPostgres, want: `string_agg(mb.address, ',')`},
	}
	for _, tt := range tests {
		t.Run(tt.driver, func(t *testing.T) {
			if got := groupConcatSQL(tt.driver, "mb.address"); got != tt.want {
				t.Fatalf("groupConcatSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConcatSQL(t *testing.T) {
	parts := []string{"from_addr", "' '", "COALESCE(from_name,'')"}
	for _, driver := range []string{databaseDriverSQLite, databaseDriverPostgres} {
		if got, want := concatSQL(driver, parts...), `from_addr || ' ' || COALESCE(from_name,'')`; got != want {
			t.Fatalf("concatSQL(%q) = %q, want %q", driver, got, want)
		}
	}
	if got, want := concatSQL(databaseDriverMySQL, parts...), `CONCAT(from_addr,' ',COALESCE(from_name,''))`; got != want {
		t.Fatalf("concatSQL(mysql) = %q, want %q", got, want)
	}
}

func TestEscapeLike(t *testing.T) {
	if got, want := escapeLike(`a_b%c!d\e`), `a!_b!%c!!d\e`; got != want {
		t.Fatalf("escapeLike() = %q, want %q", got, want)
	}
}
