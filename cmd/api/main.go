package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"minipay/internal/httpapi"
	"minipay/internal/payment"
	"minipay/internal/store"
)

// resolvePort maps $PORT to a listen port: empty means 8080, anything else
// must be a valid TCP port number.
func resolvePort(raw string) (string, error) {
	if raw == "" {
		return "8080", nil
	}

	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("PORT must be 1-65535, got %q", raw)
	}

	return strconv.Itoa(port), nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// Fail fast on bad config: a clear fatal at boot beats a bind error
	// three log lines later, or a server on the wrong port.
	port, err := resolvePort(os.Getenv("PORT"))
	if err != nil {
		slog.Error("invalid PORT", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()

	db, err := store.NewPostgres(ctx)
	if err != nil {
		slog.Error("database connection failed", "err", err)
		os.Exit(1)
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

	api := http.NewServeMux()

	api.HandleFunc("POST /v1/orders", orderHandler.CreateOrder)
	api.HandleFunc("GET /v1/orders", orderHandler.ListOrders)
	api.HandleFunc("GET /v1/orders/{id}", orderHandler.GetOrder)
	api.HandleFunc("POST /v1/orders/{id}/payments", paymentHandler.CreatePayment)
	api.HandleFunc("GET /v1/payments", paymentHandler.ListPayments)
	api.HandleFunc("GET /v1/payments/{id}", paymentHandler.GetPayment)
	api.HandleFunc("GET /v1/payments/{id}/refunds", refundHandler.ListRefunds)
	api.HandleFunc("POST /v1/payments/{id}/capture", paymentHandler.CapturePayment)
	api.HandleFunc("POST /v1/payments/{id}/void", paymentHandler.VoidPayment)
	api.HandleFunc("POST /v1/payments/{id}/refunds", refundHandler.CreateRefund)

	// Budgets: generous per-IP protection for everything (including the
	// auth lookups themselves), tighter per-merchant quotas past auth. The
	// merchant budget lives in Postgres so all replicas share it.
	ipLimiter := httpapi.NewRateLimiter(600, 100)
	merchantLimiter := store.NewPostgresLimiter(db, 120, 30)

	authed := httpapi.RequireAuth(
		orderRepo,
		httpapi.Limit(
			merchantLimiter,
			func(r *http.Request) string {
				return httpapi.MerchantFromContext(r.Context())
			},
		)(api),
	)

	root := http.NewServeMux()

	// Probes stay public: orchestrators cannot present API keys.
	root.HandleFunc("GET /healthz", httpapi.Healthz)
	root.HandleFunc("GET /readyz", httpapi.Readyz(db))
	root.Handle(
		"/",
		httpapi.RequestIDMiddleware(
			httpapi.LoggingMiddleware(
				httpapi.Limit(ipLimiter, httpapi.ClientIP)(authed),
			),
		),
	)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	sigCtx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	go func() {
		slog.Info("minipay listening", "addr", server.Addr)

		if err := server.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-sigCtx.Done()
	slog.Info("shutdown signal received, draining in-flight requests")

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}

	slog.Info("server stopped")
}
