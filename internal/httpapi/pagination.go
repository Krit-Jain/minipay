package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
)

// Pagination bounds: 20 rows by default, 100 max. The cap exists so one
// careless ?limit=1000000 cannot force a full-table read into memory.
const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

var ErrInvalidPagination = errors.New("invalid pagination parameters")

// ParsePagination reads ?limit= and ?starting_after= ("give me rows older
// than this id", newest-first). It validates cheaply here; the store
// resolves the cursor id to a timestamp and rejects unknown ids.
func ParsePagination(r *http.Request) (limit int, cursor string, err error) {
	limit = DefaultPageLimit

	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > MaxPageLimit {
			return 0, "", ErrInvalidPagination
		}
	}

	return limit, r.URL.Query().Get("starting_after"), nil
}

// writeList renders the page shape every list endpoint shares:
//
//	{"data": [...], "has_more": false}
//
// data is never null: an empty page encodes as [], so clients can always
// range over it without a nil check. has_more tells the client whether to
// fetch again with ?starting_after=<last id>.
func writeList[T any](
	w http.ResponseWriter,
	r *http.Request,
	data []T,
	hasMore bool,
) {
	if data == nil {
		data = make([]T, 0)
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(map[string]any{
		"data":     data,
		"has_more": hasMore,
	}); err != nil {
		slog.Error("encode list response", "request_id", RequestIDFromContext(r.Context()), "err", err)
	}
}
