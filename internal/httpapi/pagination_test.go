package httpapi

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func TestParsePagination(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/orders", nil)

	limit, cursor, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}

	if limit != DefaultPageLimit || cursor != "" {
		t.Fatalf("defaults: got (%d, %q), want (20, empty)", limit, cursor)
	}

	req = httptest.NewRequest("GET", "/v1/orders?limit=5&starting_after=abc", nil)

	limit, cursor, err = ParsePagination(req)
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}

	if limit != 5 || cursor != "abc" {
		t.Fatalf("explicit: got (%d, %q)", limit, cursor)
	}

	for _, bad := range []string{
		"/v1/orders?limit=0",
		"/v1/orders?limit=-3",
		"/v1/orders?limit=101",
		"/v1/orders?limit=huge",
	} {
		req = httptest.NewRequest("GET", bad, nil)

		if _, _, err := ParsePagination(req); !errors.Is(err, ErrInvalidPagination) {
			t.Fatalf("%s: got %v, want ErrInvalidPagination", bad, err)
		}
	}
}
