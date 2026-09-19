package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"minipay/internal/httpapi"
	"minipay/internal/payment"
	"minipay/internal/store"
)

func main() {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()

	db, err := store.NewPostgres(ctx)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	orderRepo := &store.OrderRepository{
		DB: db,
	}

	orderService := &payment.OrderService{
		Repo: orderRepo,
	}

	orderHandler := &httpapi.OrderHandler{
		Service: orderService,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders", orderHandler.CreateOrder)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("MiniPay listening on :8080")

	log.Fatal(server.ListenAndServe())
}
