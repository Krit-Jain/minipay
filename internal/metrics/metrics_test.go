package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExpositionShape(t *testing.T) {
	reg := NewRegistry()
	reg.ObserveHTTP("POST", "POST /v1/orders", 201, 25*time.Millisecond)
	reg.ObserveHTTP("POST", "POST /v1/orders", 201, 75*time.Millisecond)
	reg.ObserveHTTP("GET", "GET /v1/orders/{id}", 404, 2*time.Millisecond)
	reg.IncOrdersCreated()
	reg.IncPaymentsCreated("AUTHORIZED")
	reg.AddWebhookSettled(3)

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()

	for _, want := range []string{
		`minipay_http_requests_total{method="POST",route="POST /v1/orders",status="201"} 2`,
		`minipay_http_requests_total{method="GET",route="GET /v1/orders/{id}",status="404"} 1`,
		`minipay_orders_created_total 1`,
		`minipay_payments_created_total{status="AUTHORIZED"} 1`,
		`minipay_webhook_deliveries_settled_total 3`,
		`minipay_http_request_duration_seconds_count{method="POST",route="POST /v1/orders",status="201"} 2`,
		`le="+Inf"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q\nfull body:\n%s", want, body)
		}
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Errorf("content type = %q", ct)
	}
}

func TestHistogramBucketsCumulative(t *testing.T) {
	reg := NewRegistry()
	reg.ObserveHTTP("GET", "x", 200, 30*time.Millisecond)  // <= 0.05
	reg.ObserveHTTP("GET", "x", 200, 300*time.Millisecond) // <= 0.5

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()

	// Cumulative: le=0.05 sees 1, le=0.5 sees both.
	if !strings.Contains(body, `le=0.05} 1`) {
		t.Errorf("le=0.05 wrong:\n%s", body)
	}

	if !strings.Contains(body, `le=0.5} 2`) {
		t.Errorf("le=0.5 wrong:\n%s", body)
	}
}
