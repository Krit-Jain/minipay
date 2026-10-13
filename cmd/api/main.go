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

	refundHandler := &httpapi.RefundHandler{
		Service: orderService,
	}

	paymentHandler := &httpapi.PaymentHandler{
		Service: orderService,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/orders", orderHandler.CreateOrder)
	mux.HandleFunc("GET /v1/orders/{id}", orderHandler.GetOrder)
	mux.HandleFunc("POST /v1/orders/{id}/payments", paymentHandler.CreatePayment)
	mux.HandleFunc("GET /v1/payments/{id}", paymentHandler.GetPayment)
	mux.HandleFunc("POST /v1/payments/{id}/refunds", refundHandler.CreateRefund)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           httpapi.RequireAuth(orderRepo, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("MiniPay listening on :8080")

	log.Fatal(server.ListenAndServe())
}
