package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"relayforge/internal/delivery"
	"relayforge/internal/netpolicy"
	"relayforge/internal/security"
	"relayforge/internal/signing"
	"strconv"
	"time"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}
type Sender struct {
	Client HTTPDoer
	Policy *netpolicy.Policy
	Vault  *security.Vault
	Now    func() time.Time
}

func NewClient(policy *netpolicy.Policy, timeout time.Duration, concurrency int) *http.Client {
	transport := &http.Transport{Proxy: nil, DialContext: policy.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, ExpectContinueTimeout: time.Second, MaxResponseHeaderBytes: 16 << 10, DisableKeepAlives: true, DisableCompression: true, MaxConnsPerHost: concurrency, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (s *Sender) Send(ctx context.Context, job delivery.Job) delivery.Result {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	permanent := func(code string) delivery.Result {
		return delivery.Result{Outcome: "permanent_failure", ErrorCode: code}
	}
	if s.Policy.ValidateURL(job.URL) != nil {
		return permanent("forbidden_destination")
	}
	keys := [][]byte{}
	for _, record := range job.Secrets {
		if record.Version != 1 {
			return permanent("secret_unavailable")
		}
		plain, e := s.Vault.Decrypt(job.TenantID, job.EndpointID, record.ID, record.Cipher, record.Nonce)
		if e != nil {
			return permanent("secret_unavailable")
		}
		keys = append(keys, plain)
	}
	timestamp := strconv.FormatInt(now().Unix(), 10)
	signature, e := signing.Sign(job.ID, timestamp, job.Body, keys)
	if e != nil {
		return permanent("secret_unavailable")
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, job.URL, bytes.NewReader(job.Body))
	if e != nil {
		return permanent("forbidden_destination")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "RelayForge/0.4")
	request.Header.Set("webhook-id", job.ID)
	request.Header.Set("webhook-timestamp", timestamp)
	request.Header.Set("webhook-signature", signature)
	start := time.Now()
	response, e := s.Client.Do(request)
	latency := time.Since(start).Milliseconds()
	if e != nil {
		r := delivery.Result{Outcome: "retryable_failure", ErrorCode: "connection_error", LatencyMS: latency}
		var verify *tls.CertificateVerificationError
		var authority x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		switch {
		case errors.Is(e, netpolicy.ErrForbidden):
			r.Outcome = "permanent_failure"
			r.ErrorCode = "forbidden_destination"
		case errors.As(e, &verify) || errors.As(e, &authority) || errors.As(e, &hostname) || errors.As(e, &invalid):
			r.Outcome = "permanent_failure"
			r.ErrorCode = "tls_validation_failed"
		case errors.Is(e, context.DeadlineExceeded):
			r.ErrorCode = "request_timeout"
		case errors.Is(e, context.Canceled):
			r.ErrorCode = "request_cancelled"
		case errors.Is(e, netpolicy.ErrDNS):
			r.ErrorCode = "dns_error"
		}
		return r
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	r := delivery.Classify(response.StatusCode)
	r.LatencyMS = time.Since(start).Milliseconds()
	if response.StatusCode == 429 || response.StatusCode == 503 {
		r.RetryAfter = delivery.RetryAfter(response.Header.Get("Retry-After"), now())
	}
	return r
}
