package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/HallelujahHomeChurch/notification-api/internal/config"
	"github.com/HallelujahHomeChurch/notification-api/internal/database"
	"github.com/HallelujahHomeChurch/notification-api/internal/diagnostics"
	"github.com/HallelujahHomeChurch/notification-api/internal/dsr"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	from, to, err := parseArgs(args)
	if err != nil {
		return err
	}
	email, err := readEmail(os.Stdin)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate("migrate"); err != nil {
		return fmt.Errorf("validate database config: %w", err)
	}
	if len(cfg.HashKeys) == 0 {
		return errors.New("notification hash keys are required")
	}
	db, err := database.Open(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	records, err := diagnostics.Lookup(ctx, db, cfg.HashKeys, email, from, to)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(records)
}

func parseArgs(args []string) (time.Time, time.Time, error) {
	flags := flag.NewFlagSet("diagnose-notification", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fromText := flags.String("from", "", "UTC start (RFC3339)")
	toText := flags.String("to", "", "UTC end (RFC3339)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return time.Time{}, time.Time{}, errors.New("usage: diagnose-notification -from RFC3339 -to RFC3339 < recipient-input")
	}
	from, fromErr := time.Parse(time.RFC3339, *fromText)
	to, toErr := time.Parse(time.RFC3339, *toText)
	if fromErr != nil || toErr != nil || !from.Before(to) || to.Sub(from) > 31*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("from/to must define a range up to 31 days")
	}
	return from, to, nil
}

func readEmail(input io.Reader) (string, error) {
	value, err := io.ReadAll(io.LimitReader(input, 321))
	if err != nil {
		return "", err
	}
	email := strings.ToLower(strings.TrimSpace(string(value)))
	if len(value) > 320 || !dsr.ValidEmail(email) {
		return "", errors.New("stdin must contain one recipient email")
	}
	return email, nil
}
