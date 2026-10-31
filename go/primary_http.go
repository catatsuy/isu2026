package isureal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

const primaryRPCPath = "/api/internal/primary"
const primarySignatureHeader = "X-Isureal-Primary-Signature"
const primaryRegionHeader = "X-Isureal-Primary-Region"
const primaryTimeHeader = "X-Isureal-Primary-Time"
const primaryNonceHeader = "X-Isureal-Primary-Nonce"
const primaryResponseHeader = "X-Isureal-Primary-Response"
const maxPrimaryBody = 64 << 20

type primaryAPIClient struct {
	base   *url.URL
	key    []byte
	region string
	client *http.Client
}

func primaryAPIKey(token []byte) []byte {
	mac := hmac.New(sha256.New, bytes.TrimSpace(token))
	mac.Write([]byte("isureal-primary-api-v1"))
	return mac.Sum(nil)
}
func newPrimaryAPIClient(address, region string, token []byte) (*primaryAPIClient, error) {
	if address == "" {
		return nil, nil
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	if region == "asia" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid Primary API configuration")
	}
	// Production uses the server destination on its existing routed VPC path.
	if host := u.Hostname(); host != "192.168.1.10" && host != "127.0.0.1" && host != "localhost" {
		return nil, errors.New("Primary API must target isu01")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 64
	transport.IdleConnTimeout = 90 * time.Second
	return &primaryAPIClient{base: u, key: primaryAPIKey(token), region: region, client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func primaryMAC(key []byte, parts ...string) string {
	mac := hmac.New(sha256.New, key)
	for _, part := range parts {
		mac.Write([]byte(part))
		mac.Write([]byte{'\n'})
	}
	return hex.EncodeToString(mac.Sum(nil))
}
func primaryBodyHash(body []byte) string {
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}
func primaryRequestMAC(key []byte, req *http.Request, body []byte) string {
	return primaryMAC(key, "request-v1", req.Method, req.URL.RequestURI(), req.Header.Get(primaryRegionHeader), req.Header.Get(primaryTimeHeader), req.Header.Get(primaryNonceHeader), req.Header.Get("X-Client-IP"), req.Header.Get("Authorization"), req.Header.Get("Content-Type"), primaryBodyHash(body))
}
func (p *primaryAPIClient) request(ctx context.Context, method, uri string, headers http.Header, body []byte) (*http.Response, error) {
	relative, err := url.ParseRequestURI(uri)
	if err != nil || !strings.HasPrefix(relative.Path, "/api/") {
		return nil, errors.New("invalid Primary API path")
	}
	target := *p.base
	target.Path = relative.Path
	target.RawPath = relative.RawPath
	target.RawQuery = relative.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	req.Header.Del("Content-Length")
	req.Header.Del("Idempotency-Key")
	req.Header.Del("X-Idempotency-Key")
	if method != http.MethodGet && method != http.MethodHead {
		req.GetBody = nil
	}
	stripHopHeaders(req.Header)
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-isureal-primary-") {
			req.Header.Del(name)
		}
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	req.Header.Set(primaryRegionHeader, p.region)
	req.Header.Set(primaryTimeHeader, strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set(primaryNonceHeader, hex.EncodeToString(nonce))
	req.Header.Set(primarySignatureHeader, primaryRequestMAC(p.key, req, body))
	// No write retries: a transport timeout after forwarding is an unknown result.
	return p.client.Do(req)
}
func stripHopHeaders(headers http.Header) {
	for _, name := range strings.Split(headers.Get("Connection"), ",") {
		headers.Del(strings.TrimSpace(name))
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		headers.Del(name)
	}
}
func (p *primaryAPIClient) forward(c *echo.Context) error {
	c.Set("primary_forwarded", true)
	req := c.Request()
	var body []byte
	var err error
	if req.Body != nil {
		body, err = io.ReadAll(io.LimitReader(req.Body, maxPrimaryBody+1))
	}
	if err != nil {
		return err
	}
	if len(body) > maxPrimaryBody {
		return NewAPIError(413, "request_too_large")
	}
	timeout := 30 * time.Second
	if c.Path() == "/api/initialize" {
		timeout = 58 * time.Second
	}
	ctx := req.Context()
	var cancel context.CancelFunc
	if c.Path() != "/api/notifications" {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	headers := req.Header.Clone()
	headers.Set("X-Client-IP", c.RealIP())
	response, err := p.request(ctx, req.Method, req.URL.RequestURI(), headers, body)
	if err != nil {
		return NewAPIError(502, "primary_api_unavailable")
	}
	defer response.Body.Close()
	stripHopHeaders(response.Header)
	for name, values := range response.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-isureal-primary-") {
			continue
		}
		for _, value := range values {
			c.Response().Header().Add(name, value)
		}
	}
	c.Response().WriteHeader(response.StatusCode)
	if c.Path() == "/api/notifications" {
		controller := http.NewResponseController(c.Response())
		_ = controller.Flush()
		buf := make([]byte, 8192)
		for {
			n, readErr := response.Body.Read(buf)
			if n > 0 {
				if _, err = c.Response().Write(buf[:n]); err != nil {
					return nil
				}
				_ = controller.Flush()
			}
			if readErr != nil {
				if readErr == io.EOF {
					return nil
				}
				return readErr
			}
		}
	}
	_, err = io.Copy(c.Response(), response.Body)
	return err
}

type primaryNonceEntry struct {
	value   string
	expires int64
}
type primaryNonceCache struct {
	mu    sync.Mutex
	seen  map[string]bool
	queue []primaryNonceEntry
	head  int
}

func (s *primaryNonceCache) accept(nonce string, now int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	for s.head < len(s.queue) && s.queue[s.head].expires < now {
		delete(s.seen, s.queue[s.head].value)
		s.head++
	}
	if s.head > 1024 && s.head*2 > len(s.queue) {
		s.queue = append([]primaryNonceEntry(nil), s.queue[s.head:]...)
		s.head = 0
	}
	if s.seen[nonce] || len(s.seen) > 250000 {
		return false
	}
	s.seen[nonce] = true
	s.queue = append(s.queue, primaryNonceEntry{nonce, now + 61})
	return true
}

func AsiaRoutingMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	cache := &primaryNonceCache{}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			signature := req.Header.Get(primarySignatureHeader)
			internal := c.Path() == primaryRPCPath
			if signature != "" || internal || req.Header.Get(primaryRegionHeader) != "" {
				if h.Config.Region != "asia" || h.ImageFiles == nil || len(h.ImageFiles.token) < 32 {
					return NewAPIError(403, "primary_api_forbidden")
				}
				region := req.Header.Get(primaryRegionHeader)
				stamp, err := strconv.ParseInt(req.Header.Get(primaryTimeHeader), 10, 64)
				now := time.Now().Unix()
				nonce := req.Header.Get(primaryNonceHeader)
				if err != nil || stamp < now-30 || stamp > now+30 || (region != "europe" && region != "america") || len(nonce) != 32 {
					return NewAPIError(403, "primary_api_forbidden")
				}
				body, err := io.ReadAll(io.LimitReader(req.Body, maxPrimaryBody+1))
				if err != nil || len(body) > maxPrimaryBody {
					return NewAPIError(413, "request_too_large")
				}
				key := primaryAPIKey(h.ImageFiles.token)
				expected := primaryRequestMAC(key, req, body)
				if !hmac.Equal([]byte(signature), []byte(expected)) || !cache.accept(nonce, now) {
					return NewAPIError(403, "primary_api_forbidden")
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				c.Set("primary_forward_region", region)
				c.Set("primary_request_nonce", nonce)
				return next(c)
			}
			if h.PrimaryAPI != nil && strings.HasPrefix(req.URL.Path, "/api/") {
				return h.PrimaryAPI.forward(c)
			}
			return next(c)
		}
	}
}

func forwardedRegion(c *echo.Context, config *Config) string {
	if region, ok := c.Get("primary_forward_region").(string); ok {
		return region
	}
	return config.Region
}

func (p *primaryAPIClient) call(ctx context.Context, auth, ip string, input primaryRPCRequest, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", auth)
	headers.Set("X-Client-IP", ip)
	response, err := p.request(ctx, http.MethodPost, primaryRPCPath, headers, body)
	if err != nil {
		return NewAPIError(502, "primary_api_unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return NewAPIError(502, "primary_api_unavailable")
	}
	nonce := response.Request.Header.Get(primaryNonceHeader)
	expected := primaryMAC(p.key, "response-v1", nonce, strconv.Itoa(response.StatusCode), primaryBodyHash(data))
	if !hmac.Equal([]byte(response.Header.Get(primaryResponseHeader)), []byte(expected)) {
		return NewAPIError(502, "primary_api_invalid_response")
	}
	var result primaryRPCResult
	if err = json.Unmarshal(data, &result); err != nil {
		return NewAPIError(502, "primary_api_invalid_response")
	}
	if result.NoRows {
		return sql.ErrNoRows
	}
	if response.StatusCode != 200 {
		return NewAPIError(response.StatusCode, result.Error)
	}
	if output != nil {
		if err = json.Unmarshal(result.Data, output); err != nil {
			return fmt.Errorf("decode Primary API result: %w", err)
		}
	}
	return nil
}
