// Package payerhttp is the ports.PayerGateway implementation that talks to
// a PAS-style HTTP endpoint (the payer simulator, or a real intermediary
// exposing Claim/$submit and ClaimResponse reads).
package payerhttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gateway posts bundles to {BaseURL}/Claim/$submit and reads
// {BaseURL}/ClaimResponse/{ref}. It retries transient failures with a
// bounded exponential backoff; PAS submissions are idempotent on the
// payer side by Claim id, so a retry can never double-submit.
type Gateway struct {
	BaseURL string
	Client  *http.Client
	// Retries is the number of additional attempts on 5xx / transport errors.
	Retries int
	// Backoff is the initial backoff; it doubles per attempt (capped at 8x).
	Backoff time.Duration
	// Headers are added to every request (e.g. Authorization).
	Headers map[string]string
}

// New builds a gateway with sane defaults.
func New(baseURL string, timeout time.Duration) (*Gateway, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("payerhttp: invalid base url %q", baseURL)
	}
	return &Gateway{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: timeout},
		Retries: 2,
		Backoff: 200 * time.Millisecond,
	}, nil
}

// ErrPayerRejected marks a 4xx from the payer (do not retry).
var ErrPayerRejected = errors.New("payer rejected request")

// ErrPayerUnavailable marks 5xx / transport failures after retries.
var ErrPayerUnavailable = errors.New("payer unavailable")

// Submit implements ports.PayerGateway.
func (g *Gateway) Submit(ctx context.Context, bundle []byte) ([]byte, error) {
	return g.do(ctx, http.MethodPost, g.BaseURL+"/Claim/$submit", bundle)
}

// Inquire implements ports.PayerGateway.
func (g *Gateway) Inquire(ctx context.Context, ref string) ([]byte, error) {
	return g.do(ctx, http.MethodGet, g.BaseURL+"/ClaimResponse/"+url.PathEscape(ref), nil)
}

// Ping implements ports.PayerGateway.
func (g *Gateway) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPayerUnavailable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: status %d", ErrPayerUnavailable, resp.StatusCode)
	}
	return nil
}

func (g *Gateway) do(ctx context.Context, method, target string, body []byte) ([]byte, error) {
	backoff := g.Backoff
	var lastErr error
	for attempt := 0; attempt <= g.Retries; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
			if backoff < 8*g.Backoff {
				backoff *= 2
			}
		}
		out, retry, err := g.once(ctx, method, target, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, lastErr
}

// once performs one HTTP exchange; retry says whether the failure is transient.
func (g *Gateway) once(ctx context.Context, method, target string, body []byte) ([]byte, bool, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, false, fmt.Errorf("payerhttp: build request: %w", err)
	}
	req.Header.Set("Accept", "application/fhir+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	for k, v := range g.Headers {
		req.Header.Set(k, v)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("%w: %v", ErrPayerUnavailable, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, true, fmt.Errorf("%w: read body: %v", ErrPayerUnavailable, err)
	}
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, true, fmt.Errorf("%w: status %d: %s", ErrPayerUnavailable, resp.StatusCode, trim(out))
	case resp.StatusCode >= 400:
		return nil, false, fmt.Errorf("%w: status %d: %s", ErrPayerRejected, resp.StatusCode, trim(out))
	}
	return out, false, nil
}

func trim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
