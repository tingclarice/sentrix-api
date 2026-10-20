package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tingclarice/sentrix-api/internal/config"
	"github.com/tingclarice/sentrix-api/internal/httpapi"
	"github.com/tingclarice/sentrix-api/internal/ingest"
	"github.com/tingclarice/sentrix-api/internal/push"
	"github.com/tingclarice/sentrix-api/internal/store"
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

	pg := store.NewPG(db)
	ing := ingest.New(cfg, db)
	if cfg.PushEnabled() {
		ing.SetNotifier(push.New(pg, cfg.VAPIDPublicKey, cfg.VAPIDPrivateKey, cfg.VAPIDSubject))
	} else {
		log.Println("Web Push is disabled: set VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY and VAPID_SUBJECT to enable it")
	}
	client, err := ing.Connect()
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(250)

	auth, err := httpapi.NewVerifier(ctx, cfg.SupabaseJWTSecret, cfg.SupabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	if !auth.Configured() {
		log.Println("warning: neither SUPABASE_JWT_SECRET nor SUPABASE_URL is set; authenticated routes will return 503")
	}

	srv := &http.Server{
		Addr: ":" + cfg.HTTPPort,
		Handler: httpapi.New(httpapi.Deps{
			DB:                 db,
			Store:              pg,
			Auth:               auth,
			AllowedOrigins:     cfg.AllowedOrigins,
			CalibrationMinutes: cfg.CalibrationMinutes,
			OnDeviceChanged:    ing.InvalidateDevice,
		}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("HTTP API listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
