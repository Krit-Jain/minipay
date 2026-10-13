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
	"minipay/internal/metrics"
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

// resolveDuration parses an optional $VAR Go duration ("30m", "24h"): empty
// means def, anything else must parse and be positive. Used for the janitor
// knobs so operators tune retention without code changes.
func resolveDuration(raw string, def string) (time.Duration, error) {
	if raw == "" {
		raw = def
	}

	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("must be a positive Go duration, got %q", raw)
	}

	return d, nil
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

	holdTTL, err := resolveDuration(os.Getenv("HOLD_TTL"), "30m")
	if err != nil {
		slog.Error("invalid HOLD_TTL", "err", err)
		os.Exit(1)
	}

	keyTTL, err := resolveDuration(os.Getenv("KEY_TTL"), "24h")
	if err != nil {
		slog.Error("invalid KEY_TTL", "err", err)
		os.Exit(1)
	}

	janitorInterval, err := resolveDuration(os.Getenv("JANITOR_INTERVAL"), "1m")
	if err != nil {
		slog.Error("invalid JANITOR_INTERVAL", "err", err)
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

	reg := metrics.NewRegistry()

	orderHandler := &httpapi.OrderHandler{
		Service: orderService,
		Metrics: reg,
	}

	refundHandler := &httpapi.RefundHandler{
		Service: orderService,
		Metrics: reg,
	}

	paymentHandler := &httpapi.PaymentHandler{
		Service: orderService,
		Metrics: reg,
	}

	webhookHandler := &httpapi.WebhookHandler{
		Service: orderService,
	}

	disputeHandler := &httpapi.DisputeHandler{
		Service: orderService,
		Metrics: reg,
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
	api.HandleFunc("POST /v1/webhook_endpoints", webhookHandler.CreateEndpoint)
	api.HandleFunc("GET /v1/webhook_endpoints", webhookHandler.ListEndpoints)
	api.HandleFunc("DELETE /v1/webhook_endpoints/{id}", webhookHandler.DeleteEndpoint)
	api.HandleFunc("GET /v1/events", webhookHandler.ListEvents)
	api.HandleFunc("POST /v1/payments/{id}/disputes", disputeHandler.CreateDispute)
	api.HandleFunc("POST /v1/disputes/{id}/resolve", disputeHandler.ResolveDispute)
	api.HandleFunc("GET /v1/disputes", disputeHandler.ListDisputes)
	api.HandleFunc("GET /v1/disputes/{id}", disputeHandler.GetDispute)
	api.HandleFunc("GET /v1/balance", disputeHandler.GetBalance)

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
		)(reg.Middleware(api)),
	)

	root := http.NewServeMux()

	// Probes stay public: orchestrators cannot present API keys.
	root.HandleFunc("GET /healthz", httpapi.Healthz)
	root.HandleFunc("GET /readyz", httpapi.Readyz(db))
	root.Handle("GET /metrics", reg.Handler())
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

	// The outbox relay runs inside the API process: every 2s it claims due
	// webhook deliveries (SKIP LOCKED, so replicas share work) and POSTs
	// them. It stops before the server starts draining, so in-flight HTTP
	// keeps its full shutdown budget.
	relayCtx, relayStop := context.WithCancel(context.Background())
	go runRelay(relayCtx, orderRepo, reg)

	// The janitor owns time-based maintenance both workers share one
	// shutdown: holds expire and keys are collected while anyone is alive,
	// exactly once per tick across replicas (advisory-lock election).
	janitorCtx, janitorStop := context.WithCancel(context.Background())
	go runJanitor(janitorCtx, orderRepo, janitorInterval, holdTTL, keyTTL)

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
	relayStop()
	janitorStop()

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

func runRelay(ctx context.Context, repo *store.OrderRepository, reg *metrics.Registry) {
	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			settled, err := repo.ProcessDueDeliveries(ctx, client)
			if err != nil {
				slog.Error("webhook relay failed", "err", err)
			} else if settled > 0 {
				reg.AddWebhookSettled(settled)
				slog.Info("webhook relay settled deliveries", "count", settled)
			}
		}
	}
}

func runJanitor(
	ctx context.Context,
	repo *store.OrderRepository,
	interval time.Duration,
	holdTTL time.Duration,
	keyTTL time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired, purged, elected, err := repo.RunJanitor(ctx, holdTTL, keyTTL)
			switch {
			case err != nil:
				slog.Error("janitor failed", "err", err)
			case !elected:
				// Another replica is on duty this tick.
			case expired+purged > 0:
				slog.Info("janitor swept",
					"holds_expired", expired,
					"keys_purged", purged,
				)
			}
		}
	}
}
