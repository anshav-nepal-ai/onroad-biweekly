package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
)

var db *sql.DB
var dbCleanup func() error

func initDB() {
	instanceConn := os.Getenv("INSTANCE_CONNECTION_NAME")
	dbUser := os.Getenv("DB_USER")
	dbName := os.Getenv("DB_NAME")

	if instanceConn == "" {
		log.Println("INSTANCE_CONNECTION_NAME not set — triage persistence disabled (local dev mode)")
		return
	}

	cleanup, err := pgxv5.RegisterDriver("cloudsql-postgres",
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()),
	)
	if err != nil {
		log.Printf("Warning: failed to register Cloud SQL driver: %v — persistence disabled", err)
		return
	}
	dbCleanup = cleanup

	dsn := fmt.Sprintf("host=%s user=%s dbname=%s sslmode=disable", instanceConn, dbUser, dbName)
	db, err = sql.Open("cloudsql-postgres", dsn)
	if err != nil {
		log.Printf("Warning: DB open failed: %v", err)
		cleanup()
		dbCleanup = nil
		return
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Printf("Warning: DB ping failed: %v — persistence disabled", err)
		db = nil
		cleanup()
		dbCleanup = nil
		return
	}
	if err := migrateDB(); err != nil {
		log.Printf("Warning: DB migration failed: %v", err)
		db = nil
		cleanup()
		dbCleanup = nil
		return
	}
	log.Println("Database connected and schema ready")
}

func migrateDB() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS triage_cycles (
			id               SERIAL PRIMARY KEY,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			started_by_name  TEXT NOT NULL DEFAULT '',
			started_by_email TEXT NOT NULL DEFAULT '',
			triage_date      DATE NOT NULL DEFAULT CURRENT_DATE
		)`,
		`CREATE TABLE IF NOT EXISTS triage_annotations (
			cycle_id        INTEGER NOT NULL REFERENCES triage_cycles(id) ON DELETE CASCADE,
			vehicle_id      TEXT NOT NULL,
			cam_quality     TEXT NOT NULL DEFAULT '',
			cam_note        TEXT NOT NULL DEFAULT '',
			drive_quality   TEXT NOT NULL DEFAULT '',
			drive_note      TEXT NOT NULL DEFAULT '',
			triage_comments TEXT NOT NULL DEFAULT '',
			included        BOOLEAN NOT NULL DEFAULT TRUE,
			last_edited_by  TEXT NOT NULL DEFAULT '',
			last_edited_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (cycle_id, vehicle_id)
		)`,
		`CREATE TABLE IF NOT EXISTS action_items (
			cycle_id       INTEGER NOT NULL REFERENCES triage_cycles(id) ON DELETE CASCADE,
			section        TEXT NOT NULL,
			content        TEXT NOT NULL DEFAULT '',
			last_edited_by TEXT NOT NULL DEFAULT '',
			last_edited_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (cycle_id, section)
		)`,
		`CREATE TABLE IF NOT EXISTS edit_sessions (
			id             SERIAL PRIMARY KEY,
			cycle_id       INTEGER NOT NULL REFERENCES triage_cycles(id) ON DELETE CASCADE,
			editor_name    TEXT NOT NULL,
			editor_email   TEXT NOT NULL,
			started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_active_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			is_active      BOOLEAN NOT NULL DEFAULT TRUE
		)`,
		`CREATE TABLE IF NOT EXISTS triage_vehicles (
			cycle_id   INTEGER NOT NULL REFERENCES triage_cycles(id) ON DELETE CASCADE,
			vehicle_id TEXT NOT NULL,
			data       JSONB NOT NULL DEFAULT '{}',
			PRIMARY KEY (cycle_id, vehicle_id)
		)`,
		`ALTER TABLE triage_cycles ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%w", err)
		}
	}
	return nil
}
