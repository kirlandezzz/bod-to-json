package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sample = "../../testdata/SyncSupplierInvoice.xml"

func noEnv(string) string { return "" }

func TestPrintsJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{sample}, nil, &out, &errOut, noEnv); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got struct {
		ID    string `json:"id"`
		Total struct {
			Value, Currency string
		} `json:"total"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if got.ID != "ID-101" || got.Total.Value != "110" || got.Total.Currency != "USD" {
		t.Errorf("got %+v", got)
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  int
	}{
		{"not a BOD on stdin", nil, "<Other/>", 1},
		{"missing file", []string{"no-such-file.xml"}, "", 1},
		{"two files", []string{"a.xml", "b.xml"}, "", 64},
		{"unknown flag", []string{"-nope"}, "", 64},
		{"post without settings", []string{"-post", sample}, "", 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := run(tt.args, strings.NewReader(tt.stdin), &out, &errOut, noEnv); got != tt.want {
				t.Errorf("exit %d, want %d (stderr: %s)", got, tt.want, errOut.String())
			}
			if errOut.Len() == 0 {
				t.Error("nothing written to stderr")
			}
		})
	}
}

func TestPost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 60})
	})
	var key string
	mux.HandleFunc("POST /invoices", func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	env := map[string]string{"BOD_API_URL": srv.URL, "BOD_CLIENT_ID": "id", "BOD_CLIENT_SECRET": "secret"}
	var out, errOut bytes.Buffer
	code := run([]string{"-post", sample}, nil, &out, &errOut, func(k string) string { return env[k] })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if key == "" || !strings.Contains(out.String(), key) {
		t.Errorf("output %q does not report the key %q the server received", out.String(), key)
	}
}
