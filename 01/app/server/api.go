package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"koupa/app/hooks"
	"koupa/app/store"
	"koupa/internal/wire"
)

// maxBody caps request bodies; orders are tiny.
const maxBody = 64 << 10

// pendingLimit caps GET /api/orders?status=pending.
const pendingLimit = 500

type orderJSON struct {
	OrderID int64  `json:"order_id"`
	Number  string `json:"number"`
	Total   int64  `json:"total"`
}

type pendingItemJSON struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

// pendingJSON is an order plus what to make for it.
type pendingJSON struct {
	orderJSON
	ClientRef string            `json:"client_ref"`
	Items     []pendingItemJSON `json:"items"`
}

type menuJSON struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Price     int64  `json:"price"`
	Available bool   `json:"available"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeStoreError maps a store error (and the transaction outcome) to a
// status. Anything not known to be the client's fault is 503: retryable.
func writeStoreError(w http.ResponseWriter, err error, txr store.TxResult) {
	switch {
	case txr.Outcome == store.TxCommitUnknown:
		writeError(w, http.StatusServiceUnavailable, "commit outcome unknown")
	case errors.Is(err, store.ErrInvalidOrder):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "order not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrSoldOut):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case store.IsLockTimeout(err):
		writeError(w, http.StatusServiceUnavailable, "lock timeout")
	case store.IsDeadlock(err):
		writeError(w, http.StatusServiceUnavailable, "deadlock")
	case errors.Is(err, store.ErrKeyVanished):
		writeError(w, http.StatusServiceUnavailable, "idempotency key vanished, retry")
	default:
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func (s *Server) handleMenu(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := store.DBContext()
	defer cancel()
	items, err := store.ListMenu(ctx, s.deps.Pool)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := make([]menuJSON, 0, len(items))
	for _, m := range items {
		out = append(out, menuJSON{ID: m.ID, Name: m.Name, Price: m.Price, Available: m.Available})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientRef string `json:"client_ref"`
		Items     []struct {
			MenuItemID int64 `json:"menu_item_id"`
			Quantity   int   `json:"quantity"`
		} `json:"items"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	attempt := r.Header.Get(wire.HeaderAttempt)
	s.deps.Hooks.Pause(r.Context(), hooks.RequestMid, attempt)
	req := store.OrderRequest{ClientRef: body.ClientRef}
	for _, it := range body.Items {
		req.Items = append(req.Items, store.OrderItem{MenuItemID: it.MenuItemID, Quantity: it.Quantity})
	}
	// The transaction runs on its own context: a client that gives up
	// must not cancel it half way.
	ctx, cancel := store.DBContext()
	defer cancel()
	res, txr, err := store.PlaceOrder(ctx, s.deps.Pool, req, store.PlaceOptions{
		Idempotency:    s.cfg.IdempotencyKey,
		CheckSoldOut:   s.cfg.SoldOut(),
		LockTimeout:    s.cfg.LockTimeout,
		DuplicateOrder: s.deps.DuplicateOrder,
		DuplicateDone:  s.deps.DuplicateDone,
		RequestCtx:     r.Context(),
		Hooks:          s.deps.Hooks,
		AttemptID:      attempt,
	})
	recordTx(r, txr)
	if err != nil {
		writeStoreError(w, err, txr)
		return
	}
	recordFields(r, "order_id", res.Order.ID, "replayed", res.Replayed)
	if !res.Replayed {
		// Committed; the client has no response yet.
		s.deps.Hooks.Pause(r.Context(), hooks.OrderAfterCommit, attempt)
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, orderJSON{OrderID: res.Order.ID, Number: res.Order.Number, Total: res.Order.Total})
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	if st := r.URL.Query().Get("status"); st != "pending" {
		writeError(w, http.StatusBadRequest, "status must be pending")
		return
	}
	ctx, cancel := store.DBContext()
	defer cancel()
	pat := ""
	if run := r.URL.Query().Get("run"); run != "" {
		pat = runPattern(run)
	}
	orders, err := store.ListPending(ctx, s.deps.Pool, pat, pendingLimit)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	out := make([]pendingJSON, 0, len(orders))
	for _, o := range orders {
		p := pendingJSON{orderJSON: orderJSON{OrderID: o.ID, Number: o.Number, Total: o.Total}, ClientRef: o.ClientRef, Items: []pendingItemJSON{}}
		for _, it := range o.Items {
			p.Items = append(p.Items, pendingItemJSON{Name: it.Name, Quantity: it.Quantity})
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	attempt := r.Header.Get(wire.HeaderAttempt)
	s.deps.Hooks.Pause(r.Context(), hooks.RequestMid, attempt)
	ctx, cancel := store.DBContext()
	defer cancel()
	recordFields(r, "order_id", id)
	res, txr, err := store.MarkReady(ctx, s.deps.Pool, id, attempt, 0)
	recordTx(r, txr)
	if err != nil {
		writeStoreError(w, err, txr)
		return
	}
	// Whether this attempt assigned the number or got the earlier one (A10).
	recordFields(r, "ready_seq", res.ReadySeq, "newly_assigned", res.NewlyAssigned,
		"lock_wait_ms", float64(res.LockWait.Microseconds())/1000)
	writeJSON(w, http.StatusOK, map[string]any{
		"order_id": res.OrderID, "ready_seq": res.ReadySeq, "number": res.Number, "newly_assigned": res.NewlyAssigned,
	})
}

// handleAvailable is v2's sold-out switch (not routed in v1).
func (s *Server) handleAvailable(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Available *bool `json:"available"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Available == nil {
		writeError(w, http.StatusBadRequest, "available is required")
		return
	}
	ctx, cancel := store.DBContext()
	defer cancel()
	txr, err := store.SetAvailable(ctx, s.deps.Pool, []int64{id}, *body.Available)
	recordTx(r, txr)
	if err != nil {
		writeStoreError(w, err, txr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "available": *body.Available})
}

// Audit queries (plan A9 step 5, A10). A run's rows are those whose
// client_ref starts with "<run>-".
const (
	auditCursorSQL = `SELECT COALESCE(MAX(ready_seq), 0) FROM orders
		WHERE client_ref LIKE $1 ESCAPE '\' AND ready_seq IS NOT NULL`
	auditOrdersSQL = `SELECT client_ref, id FROM orders
		WHERE client_ref LIKE $1 ESCAPE '\' ORDER BY client_ref, id`
	auditReadySQL = `SELECT id, ready_seq, ready_attempt FROM orders
		WHERE client_ref LIKE $1 ESCAPE '\' AND ready_seq IS NOT NULL ORDER BY ready_seq`
)

// runPattern is the LIKE pattern for a run's client_refs, with LIKE
// metacharacters in the run id escaped.
func runPattern(run string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(run) + "-%"
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	run := q.Get("run")
	if run == "" {
		writeError(w, http.StatusBadRequest, "run is required")
		return
	}
	pat := runPattern(run)
	ctx, cancel := store.DBContext()
	defer cancel()
	switch q.Get("mode") {
	case "cursor":
		// One statement, so C is read from a single snapshot.
		var c int64
		if err := s.deps.Pool.QueryRow(ctx, auditCursorSQL, pat).Scan(&c); err != nil {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"cursor": c})
	case "orders":
		rows, err := s.deps.Pool.Query(ctx, auditOrdersSQL, pat)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		defer rows.Close()
		out := map[string][]int64{}
		for rows.Next() {
			var ref string
			var id int64
			if err := rows.Scan(&ref, &id); err != nil {
				writeError(w, http.StatusServiceUnavailable, "database unavailable")
				return
			}
			out[ref] = append(out[ref], id)
		}
		if rows.Err() != nil {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, out)
	case "ready":
		type readyRow struct {
			OrderID      int64   `json:"order_id"`
			ReadySeq     int64   `json:"ready_seq"`
			ReadyAttempt *string `json:"ready_attempt"`
		}
		rows, err := s.deps.Pool.Query(ctx, auditReadySQL, pat)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		defer rows.Close()
		out := []readyRow{}
		for rows.Next() {
			var rr readyRow
			if err := rows.Scan(&rr.OrderID, &rr.ReadySeq, &rr.ReadyAttempt); err != nil {
				writeError(w, http.StatusServiceUnavailable, "database unavailable")
				return
			}
			out = append(out, rr)
		}
		if rows.Err() != nil {
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusBadRequest, "mode must be cursor, orders or ready")
	}
}
