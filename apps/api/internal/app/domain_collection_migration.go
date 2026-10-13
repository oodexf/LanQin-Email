package app

import (
	"context"
	"database/sql"
)

func migrateExternalSchemaV12(ctx context.Context, conn *sql.Conn, driver string) error {
	applied, err := externalSchemaHistory(ctx, conn, 12)
	if err != nil || applied {
		return err
	}
	return applyExternalSchemaMigration(ctx, conn, driver, 12, "external_schema_v12_domain_collection", domainCollectionSchema())
}
