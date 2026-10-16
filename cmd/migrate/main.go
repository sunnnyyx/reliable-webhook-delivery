package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"relayforge/internal/config"
	"relayforge/internal/migrate"
	"relayforge/internal/receiver/schema"
	"strings"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) != 2 || (os.Args[1] != "up" && os.Args[1] != "down" && os.Args[1] != "receiver-up" && os.Args[1] != "receiver-down") {
		log.Error("usage: migrate up|down|receiver-up|receiver-down")
		os.Exit(1)
	}
	if strings.HasPrefix(os.Args[1], "receiver-") {
		url := os.Getenv("RECEIVER_DATABASE_URL")
		if url == "" {
			log.Error("RECEIVER_DATABASE_URL required")
			os.Exit(1)
		}
		if os.Args[1] == "receiver-down" && os.Getenv("PROFILE") != "development" {
			log.Error("receiver-down restricted to explicit development profile")
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db, e := sql.Open("pgx", url)
		if e == nil {
			defer db.Close()
			db.SetMaxOpenConns(1)
			var pErr error
			p, pErr := schema.Provider(db)
			e = pErr
			if e == nil {
				if os.Args[1] == "receiver-up" {
					_, e = p.Up(ctx)
				} else {
					_, e = p.Down(ctx)
				}
			}
		}
		if e != nil {
			log.Error("receiver migration failed")
			os.Exit(1)
		}
		log.Info("receiver migration complete")
		return
	}
	c, e := config.Load(os.Getenv)
	if e != nil {
		log.Error("invalid configuration", "error", e.Error())
		os.Exit(1)
	}
	if os.Args[1] == "down" && c.Profile != "development" {
		log.Error("down is restricted to development")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e = migrate.Run(ctx, c.DatabaseURL, os.Args[1]); e != nil {
		log.Error("migration failed; verify database availability and schema state")
		os.Exit(1)
	}
	log.Info("migration complete", "direction", os.Args[1])
}
