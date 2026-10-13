// Command apikey mints a secret API key for a merchant. The raw key is
// printed once and cannot be recovered afterwards — only its hash is
// stored. Usage:
//
//	DATABASE_URL=... go run ./cmd/apikey -merchant acme
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"minipay/internal/store"
)

func main() {
	merchantID := flag.String("merchant", "", "merchant id to mint a key for")
	name := flag.String("name", "", "merchant display name (defaults to the id)")
	flag.Parse()

	if *merchantID == "" {
		fmt.Fprintln(os.Stderr, "usage: apikey -merchant <id> [-name <display name>]")
		os.Exit(2)
	}

	if *name == "" {
		*name = *merchantID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := store.NewPostgres(ctx)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	repo := &store.OrderRepository{DB: db}

	if err := repo.EnsureMerchant(ctx, *merchantID, *name); err != nil {
		log.Fatalf("ensure merchant: %v", err)
	}

	raw, err := repo.IssueAPIKey(ctx, *merchantID)
	if err != nil {
		log.Fatalf("issue key: %v", err)
	}

	fmt.Printf("merchant: %s\n", *merchantID)
	fmt.Printf("api key (shown once, store it now): %s\n", raw)
}
