package bod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sampleInvoice(t *testing.T) Invoice {
	t.Helper()
	f, err := os.Open("testdata/SyncSupplierInvoice.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	inv, err := ParseInvoice(f)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestParseInvoice(t *testing.T) {
	inv := sampleInvoice(t)

	checks := []struct{ name, got, want string }{
		{"action", inv.Action.Code, "Add"},
		{"id", inv.ID, "ID-101"},
		{"supplier", inv.Supplier, "Supplier-101"},
		{"date", inv.Date, "2019-01-13T06:56:52Z"},
		{"total", inv.Total.Value, "110"},
		{"currency", inv.Total.Currency, "USD"},
		{"net", inv.Net.Value, "100"},
		{"sender", inv.Sender, "infor.WLM.1"},
		{"tenant", inv.Tenant, "Infor"},
		{"accounting entity", inv.AccountingEntity, "ae1"},
		{"purchase order", inv.PurchaseOrder, "ID-101"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if len(inv.Lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(inv.Lines))
	}
	if l := inv.Lines[0]; l.Number != "Line001" || l.Amount.Value != "100" || l.Item != "ItemID1" {
		t.Errorf("line = %+v, want Line001 / ItemID1 / 100", l)
	}
	if err := inv.Validate(); err != nil {
		t.Errorf("the Infor sample should be valid: %v", err)
	}
}

func TestParseInvoiceRejectsOtherDocuments(t *testing.T) {
	for _, doc := range []string{
		`<SyncPurchaseOrder xmlns="http://schema.infor.com/InforOAGIS/2"/>`,
		`<SyncSupplierInvoice/>`, // right name, missing namespace
		`not xml`,
	} {
		if _, err := ParseInvoice(strings.NewReader(doc)); err == nil {
			t.Errorf("ParseInvoice(%q) returned no error", doc)
		}
	}
}

func TestValidate(t *testing.T) {
	valid := Invoice{
		Action: Action{Code: "Add"}, ID: "ID-101", Supplier: "Supplier-101", Date: "2019-01-13T06:56:52Z",
		Net:   Amount{Value: "100.50", Currency: "USD"},
		Total: Amount{Value: "110.50", Currency: "USD"},
		Lines: []Line{
			{Number: "1", Amount: Amount{Value: "0.10", Currency: "USD"}},
			{Number: "2", Amount: Amount{Value: "100.40", Currency: "USD"}},
		},
	}

	tests := []struct {
		name    string
		change  func(*Invoice)
		wantErr string // empty means valid
	}{
		{"valid", func(*Invoice) {}, ""},
		{"no id", func(i *Invoice) { i.ID = " " }, "invoice ID is empty"},
		{"no supplier", func(i *Invoice) { i.Supplier = "" }, "supplier is empty"},
		{"no currency", func(i *Invoice) { i.Total.Currency = "" }, "currency is empty"},
		{"total not a number", func(i *Invoice) { i.Total.Value = "abc" }, "not a number"},
		{"total zero", func(i *Invoice) { i.Total.Value = "0" }, "not positive"},
		{"total negative", func(i *Invoice) { i.Total.Value = "-5" }, "not positive"},
		{"unknown action", func(i *Invoice) { i.Action.Code = "Upsert" }, "action code"},
		{"date without time", func(i *Invoice) { i.Date = "2019-01-13" }, "document date"},
		{"lines do not add up", func(i *Invoice) { i.Net.Value = "100.51" }, "lines add up to 100.50"},
		{"line in another currency", func(i *Invoice) {
			i.Lines = []Line{{Number: "1", Amount: Amount{Value: "100.50", Currency: "EUR"}}}
		}, "differs from invoice currency"},
		{"line amount not a number", func(i *Invoice) {
			i.Lines = []Line{{Number: "1", Amount: Amount{Value: "x", Currency: "USD"}}}
		}, "line 1: amount"},
		{"no lines is allowed", func(i *Invoice) { i.Lines = nil }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := valid
			tt.change(&inv)
			err := inv.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestIdempotencyKey(t *testing.T) {
	a := sampleInvoice(t)
	b := sampleInvoice(t)
	if a.IdempotencyKey() != b.IdempotencyKey() {
		t.Error("same invoice produced different keys")
	}
	if len(a.IdempotencyKey()) != 64 {
		t.Errorf("key length = %d, want 64 hex characters", len(a.IdempotencyKey()))
	}

	b.Supplier = " supplier-101 " // same supplier, different formatting
	if a.IdempotencyKey() != b.IdempotencyKey() {
		t.Error("case and spaces changed the key")
	}
	b = sampleInvoice(t)
	b.AccountingEntity = "ae2" // same number, another company
	if a.IdempotencyKey() == b.IdempotencyKey() {
		t.Error("another accounting entity produced the same key")
	}
	b = sampleInvoice(t)
	b.ID = "ID-102"
	if a.IdempotencyKey() == b.IdempotencyKey() {
		t.Error("different invoices produced the same key")
	}
}

// fakeERP is a token endpoint plus an invoice endpoint that answers with the
// given status codes in order, repeating the last one.
type fakeERP struct {
	*httptest.Server
	statuses []int
	posts    atomic.Int32
	tokens   atomic.Int32 // token requests seen
	expires  int          // expires_in sent with each token, in seconds
	header   http.Header  // extra headers on every invoice response
	keys     chan string
	bodies   chan []byte
}

func newFakeERP(t *testing.T, statuses ...int) *fakeERP {
	t.Helper()
	erp := &fakeERP{statuses: statuses, keys: make(chan string, 10), bodies: make(chan []byte, 10)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "client" || secret != "secret" || r.FormValue("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		erp.tokens.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "token_type": "Bearer", "expires_in": erp.expires})
	})
	mux.HandleFunc("POST /invoices", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := int(erp.posts.Add(1))
		var body json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		erp.keys <- r.Header.Get("Idempotency-Key")
		erp.bodies <- body
		for k, v := range erp.header {
			w.Header()[k] = v
		}
		w.WriteHeader(erp.statuses[min(n, len(erp.statuses))-1])
	})
	erp.Server = httptest.NewServer(mux)
	t.Cleanup(erp.Close)
	return erp
}

func (erp *fakeERP) client() *Client {
	return &Client{
		BaseURL: erp.URL, ClientID: "client", ClientSecret: "secret",
		HTTP: erp.Server.Client(), MaxAttempts: 4, BaseDelay: time.Millisecond,
	}
}

func TestPostInvoiceRetriesUntilSuccess(t *testing.T) {
	erp := newFakeERP(t, 503, 503, 201)
	inv := sampleInvoice(t)

	if err := erp.client().PostInvoice(context.Background(), inv); err != nil {
		t.Fatalf("PostInvoice: %v", err)
	}
	if got := erp.posts.Load(); got != 3 {
		t.Errorf("server saw %d posts, want 3", got)
	}
	for i := 0; i < 3; i++ {
		if key := <-erp.keys; key != inv.IdempotencyKey() {
			t.Errorf("attempt %d sent key %q, want %q", i+1, key, inv.IdempotencyKey())
		}
	}
	var sent struct {
		ID    string `json:"id"`
		Total Amount `json:"total"`
	}
	if err := json.Unmarshal(<-erp.bodies, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ID != "ID-101" || sent.Total.Value != "110" || sent.Total.Currency != "USD" {
		t.Errorf("server received %+v", sent)
	}
}

func TestPostInvoiceDoesNotRetryRejection(t *testing.T) {
	erp := newFakeERP(t, 400)
	err := erp.client().PostInvoice(context.Background(), sampleInvoice(t))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %v, want a 400 rejection", err)
	}
	if got := erp.posts.Load(); got != 1 {
		t.Errorf("server saw %d posts, want 1", got)
	}
}

func TestPostInvoiceGivesUp(t *testing.T) {
	erp := newFakeERP(t, 503)
	err := erp.client().PostInvoice(context.Background(), sampleInvoice(t))
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if got := erp.posts.Load(); got != 4 {
		t.Errorf("server saw %d posts, want MaxAttempts = 4", got)
	}
}

func TestPostInvoiceStopsWhenContextEnds(t *testing.T) {
	erp := newFakeERP(t, 503)
	c := erp.client()
	c.BaseDelay = time.Hour // the wait must be cut short by the context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.PostInvoice(ctx, sampleInvoice(t))
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("error = %v, want a deadline error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("PostInvoice ignored the context timeout")
	}
}

func TestPostInvoiceRejectsBadCredentials(t *testing.T) {
	erp := newFakeERP(t, 201)
	c := erp.client()
	c.ClientSecret = "wrong"
	if err := c.PostInvoice(context.Background(), sampleInvoice(t)); err == nil {
		t.Fatal("expected a token error")
	}
	if got := erp.posts.Load(); got != 0 {
		t.Errorf("server saw %d posts, want 0", got)
	}
}

func TestPostInvoiceRefusesInvalidInvoice(t *testing.T) {
	erp := newFakeERP(t, 201)
	inv := sampleInvoice(t)
	inv.Supplier = ""
	if err := erp.client().PostInvoice(context.Background(), inv); err == nil {
		t.Fatal("expected a validation error")
	}
	if got := erp.posts.Load(); got != 0 {
		t.Errorf("server saw %d posts, want 0", got)
	}
}

func TestTokenIsCachedUntilItExpires(t *testing.T) {
	erp := newFakeERP(t, 201)
	erp.expires = 3600
	c := erp.client()
	for i := 0; i < 3; i++ {
		if err := c.PostInvoice(context.Background(), sampleInvoice(t)); err != nil {
			t.Fatal(err)
		}
	}
	if got := erp.tokens.Load(); got != 1 {
		t.Errorf("token endpoint called %d times for 3 invoices, want 1", got)
	}

	erp.expires = 0 // a token without a lifetime must not be reused
	c = erp.client()
	erp.tokens.Store(0)
	for i := 0; i < 2; i++ {
		if err := c.PostInvoice(context.Background(), sampleInvoice(t)); err != nil {
			t.Fatal(err)
		}
	}
	if got := erp.tokens.Load(); got != 2 {
		t.Errorf("token endpoint called %d times, want 2", got)
	}
}

func TestPostInvoiceRefreshesTokenOnceAfter401(t *testing.T) {
	erp := newFakeERP(t, 401, 201)
	erp.expires = 3600
	if err := erp.client().PostInvoice(context.Background(), sampleInvoice(t)); err != nil {
		t.Fatalf("PostInvoice: %v", err)
	}
	if got := erp.tokens.Load(); got != 2 {
		t.Errorf("token endpoint called %d times, want 2 (first token, then a fresh one)", got)
	}

	erp = newFakeERP(t, 401) // always refused: one refresh, then stop
	err := erp.client().PostInvoice(context.Background(), sampleInvoice(t))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want a 401", err)
	}
	if got := erp.posts.Load(); got != 2 {
		t.Errorf("server saw %d posts, want 2", got)
	}
}

func TestWait(t *testing.T) {
	c := &Client{MaxDelay: 3 * time.Second}
	plain := errRetryable

	for i := 0; i < 100; i++ { // jitter is random, so sample it
		if w := c.wait(time.Second, plain); w < time.Second || w > 1500*time.Millisecond {
			t.Fatalf("wait = %v, want 1s plus at most 50%% jitter", w)
		}
	}
	if w := c.wait(time.Second, &retryAfterError{status: 429, wait: 2 * time.Second}); w != 2*time.Second {
		t.Errorf("wait = %v, want the 2s the server asked for", w)
	}
	if w := c.wait(time.Second, &retryAfterError{status: 503, wait: time.Minute}); w != 3*time.Second {
		t.Errorf("wait = %v, want MaxDelay 3s", w)
	}
	if w := c.wait(time.Minute, plain); w != 3*time.Second {
		t.Errorf("wait = %v, want MaxDelay 3s", w)
	}
}

func TestPostInvoiceHonoursRetryAfter(t *testing.T) {
	erp := newFakeERP(t, 429, 201)
	erp.header = http.Header{"Retry-After": {"1"}}
	c := erp.client() // BaseDelay is 1ms, so a 1s pause can only come from the header

	start := time.Now()
	if err := c.PostInvoice(context.Background(), sampleInvoice(t)); err != nil {
		t.Fatalf("PostInvoice: %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Errorf("retried after %v, want at least the 1s from Retry-After", waited)
	}
}
