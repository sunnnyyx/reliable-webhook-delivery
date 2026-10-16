package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"relayforge/internal/config"
	"relayforge/internal/repository/postgres"
	"relayforge/internal/security"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e.Error())
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: admin tenant-create|key-create|key-revoke|tenant-disable [flags]")
	}
	action := os.Args[1]
	flags := flag.NewFlagSet("admin", flag.ContinueOnError)
	name := flags.String("name", "", "tenant name")
	tenant := flags.String("tenant", "", "tenant UUID")
	key := flags.String("key", "", "API key UUID (not token)")
	if e := flags.Parse(os.Args[2:]); e != nil {
		return fmt.Errorf("invalid command flags")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	switch action {
	case "tenant-create":
		if *name == "" || *tenant != "" || *key != "" {
			return fmt.Errorf("tenant-create requires --name only")
		}
	case "key-create", "tenant-disable":
		if !security.ValidID(*tenant) || *name != "" || *key != "" {
			return fmt.Errorf("command requires --tenant UUID only")
		}
	case "key-revoke":
		if !security.ValidID(*tenant) || !security.ValidID(*key) || *name != "" {
			return fmt.Errorf("key-revoke requires --tenant UUID --key UUID")
		}
	default:
		return fmt.Errorf("unknown admin command")
	}
	c, e := config.Load(os.Getenv)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, e := postgres.Open(ctx, c.DatabaseURL, c.DBMaxConns)
	if e != nil {
		return e
	}
	defer db.Close()
	s := &postgres.Store{DB: db}
	var output any
	switch action {
	case "tenant-create":
		output, e = s.Provision(ctx, *name)
	case "key-create":
		output, e = s.CreateKey(ctx, *tenant)
	case "key-revoke":
		e = s.RevokeKey(ctx, *tenant, *key)
		output = map[string]string{"status": "revoked"}
	case "tenant-disable":
		e = s.DisableTenant(ctx, *tenant)
		output = map[string]string{"status": "disabled"}
	}
	if e != nil {
		return fmt.Errorf("admin operation failed; check inputs and database state")
	}
	return json.NewEncoder(os.Stdout).Encode(output)
}
