package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewHTTPClient returns a client with conservative timeouts for provider calls.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// ClassifyTransportError decides whether a failed HTTP call could have
// reached the provider. Dial/DNS/TLS-handshake failures happen before the
// request is written and are safe to retry; anything else is uncertain.
func ClassifyTransportError(err error) error {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || (errors.As(err, &opErr) && opErr.Op == "dial") {
		return &ProviderError{Class: ErrNotSent, Message: err.Error()}
	}
	var uerr *url.Error
	if errors.As(err, &uerr) && strings.Contains(uerr.Err.Error(), "tls: ") {
		return &ProviderError{Class: ErrNotSent, Message: err.Error()}
	}
	return &ProviderError{Class: ErrUncertain, Message: err.Error()}
}

// DoJSON sends a JSON request and decodes a JSON response (any status).
// The response body is size-limited.
func DoJSON(ctx context.Context, c *http.Client, method, u string, headers map[string]string, body any, out any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, ClassifyTransportError(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, &ProviderError{Class: ErrUncertain, Message: "read response: " + err.Error(), HTTPStatus: resp.StatusCode}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, &ProviderError{Class: ErrUncertain, Message: fmt.Sprintf("decode response: %v", err), HTTPStatus: resp.StatusCode}
		}
	}
	return resp.StatusCode, raw, nil
}

// FlexString unmarshals a JSON string or number into a string. Providers are
// inconsistent about numeric fields (e.g. ResultCode, expires_in).
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = FlexString(n.String())
	return nil
}
