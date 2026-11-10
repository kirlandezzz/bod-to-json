package bod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client posts invoices to an API protected by an OAuth 2.0 client
// credentials token. It is safe for use by several goroutines.
type Client struct {
	BaseURL      string
	TokenURL     string // defaults to BaseURL + "/token"
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	MaxAttempts  int           // total tries per invoice, including the first
	BaseDelay    time.Duration // wait before the first retry; doubles each time
	MaxDelay     time.Duration // upper limit for one wait; 0 means no limit

	mu      sync.Mutex
	tok     string
	expires time.Time
}

// errRetryable marks failures worth another attempt: network errors, 429 and 5xx.
var errRetryable = errors.New("retryable")

// errUnauthorized means the API refused the token (401).
var errUnauthorized = errors.New("unauthorized")

// retryAfterError carries the wait the server asked for in Retry-After.
type retryAfterError struct {
	status int
	wait   time.Duration
}

func (e *retryAfterError) Error() string { return fmt.Sprintf("status %d", e.status) }
func (e *retryAfterError) Unwrap() error { return errRetryable }

// token returns the cached bearer token, asking the token endpoint for a new
// one when there is none or it is about to expire.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && time.Now().Before(c.expires) {
		return c.tok, nil
	}

	tokenURL := c.TokenURL
	if tokenURL == "" {
		tokenURL = c.BaseURL + "/token"
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.ClientID, c.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request: status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"` // seconds
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", errors.New("token response: no access_token")
	}
	c.tok = body.AccessToken
	// Renew 30 seconds early. Without expires_in the token is not cached.
	c.expires = time.Now().Add(time.Duration(body.ExpiresIn)*time.Second - 30*time.Second)
	return c.tok, nil
}

// dropToken forgets a token the API has refused.
func (c *Client) dropToken(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == tok {
		c.tok = ""
	}
}

// PostInvoice validates the invoice and sends it, retrying temporary failures
// with exponential backoff until MaxAttempts is reached or ctx ends. Every
// attempt carries the same Idempotency-Key, so a retry after a lost response
// cannot create a second invoice.
func (c *Client) PostInvoice(ctx context.Context, inv Invoice) error {
	if err := inv.Validate(); err != nil {
		return fmt.Errorf("invalid invoice: %w", err)
	}
	payload, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	key := inv.IdempotencyKey()

	delay := c.BaseDelay
	refreshed := false
	for attempt := 1; ; attempt++ {
		tok, err := c.token(ctx)
		if err != nil {
			return err
		}
		err = c.send(ctx, tok, key, payload)

		// A 401 usually means the token expired early: get a new one, once.
		if errors.Is(err, errUnauthorized) && !refreshed {
			refreshed = true
			c.dropToken(tok)
			attempt--
			continue
		}
		if err == nil || !errors.Is(err, errRetryable) || attempt >= c.MaxAttempts {
			return err
		}

		wait := c.wait(delay, err)
		delay *= 2
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return fmt.Errorf("gave up after %d attempts: %w", attempt, ctx.Err())
		}
	}
}

// wait picks the pause before the next attempt: what the server asked for in
// Retry-After, or else the backoff delay plus up to 50% random jitter so that
// many clients do not retry at the same instant. MaxDelay caps both.
func (c *Client) wait(delay time.Duration, err error) time.Duration {
	wait := delay
	if delay > 0 {
		wait += rand.N(delay/2 + 1)
	}
	var ra *retryAfterError
	if errors.As(err, &ra) && ra.wait > 0 {
		wait = ra.wait
	}
	if c.MaxDelay > 0 && wait > c.MaxDelay {
		wait = c.MaxDelay
	}
	return wait
}

func (c *Client) send(ctx context.Context, tok, key string, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/invoices", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errRetryable, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: status 401", errUnauthorized)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		// Retry-After in seconds; the HTTP-date form is ignored.
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &retryAfterError{status: resp.StatusCode, wait: time.Duration(secs) * time.Second}
	default:
		return fmt.Errorf("rejected: status %d", resp.StatusCode)
	}
}
