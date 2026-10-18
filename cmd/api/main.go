package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
	"github.com/tingclarice/sentrix-api/internal/ingest"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	// Supabase's transaction pooler (PgBouncer) doesn't support prepared statements.
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	db, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	// The pool connects lazily; fail fast on a bad DATABASE_URL.
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("database: %v", err)
	}

	client, err := ingest.New(cfg, db).Connect()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(250)

	<-ctx.Done()
	log.Println("shutting down")
}
