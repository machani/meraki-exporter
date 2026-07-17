// Package meraki is a minimal Cisco Meraki Dashboard API v1 client:
// authentication, rate limiting, retries with backoff (honoring
// Retry-After on 429), and Link-header pagination.
package meraki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxRetries = 4
	// minRequestGap keeps us at ~5 req/s, half of Meraki's 10 req/s
	// per-org budget, leaving headroom for other API consumers.
	minRequestGap = 200 * time.Millisecond
	// perPageDefault is the page size for org-wide list endpoints that accept
	// large pages (devices, networks, availabilities, uplink statuses, ...).
	perPageDefault = 1000
	// Some endpoints reject perPageDefault — Meraki caps their page size much
	// lower and returns HTTP 400. These are each endpoint's documented maximum.
	perPageVPNStatuses = 300
	perPageAssurance   = 300
	perPageSwitchPorts = 20
	perPageSensors     = 100
	// errorBodyLimit caps how much of an error response body is echoed
	// into error messages.
	errorBodyLimit = 512
)

// Client is a minimal Meraki Dashboard API v1 client.
type Client struct {
	baseURL    string
	apiKey     string
	userAgent  string
	httpClient *http.Client
	logger     *slog.Logger
	// throttle serializes requests and enforces minRequestGap.
	throttle chan struct{}
	lastReq  time.Time
	// requestsTotal counts API requests by HTTP status code.
	requestsTotal *prometheus.CounterVec
}

// New builds a client. reg may be nil to skip instrumentation.
// proxyURL optionally routes all API requests through an HTTP, HTTPS, or
// SOCKS5 proxy; when empty, the default transport applies the standard
// HTTP_PROXY / HTTPS_PROXY / NO_PROXY environment settings instead.
func New(baseURL, apiKey string, timeout time.Duration, userAgent, proxyURL string,
	logger *slog.Logger, reg prometheus.Registerer) (*Client, error) {
	httpClient := &http.Client{Timeout: timeout}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		// Clone the default transport so HTTP/2, dial timeouts, and
		// connection pooling keep their stdlib defaults.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyURL(u)
		httpClient.Transport = transport
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		userAgent:  userAgent,
		httpClient: httpClient,
		logger:     logger,
		throttle:   make(chan struct{}, 1),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "meraki",
			Subsystem: "exporter",
			Name:      "api_requests_total",
			Help:      "Meraki API requests made by this exporter, by HTTP status code.",
		}, []string{"code"}),
	}
	c.throttle <- struct{}{}
	if reg != nil {
		reg.MustRegister(c.requestsTotal)
	}
	return c, nil
}

// linkNextRe matches the rel=next link in a Link header. Meraki currently
// sends the rel unquoted, but RFC 8288 allows rel="next" — accept both so an
// API-side change cannot silently truncate pagination.
var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel=(?:"next"|next\b)`)

// do performs one HTTP GET with rate limiting and retries, returning the
// response body and the "next" pagination URL (empty if none).
func (c *Client) do(ctx context.Context, rawURL string) (body []byte, next string, err error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		b, n, retryAfter, attemptErr := c.doOnce(ctx, rawURL)
		if attemptErr == nil {
			return b, n, nil
		}
		lastErr = attemptErr
		if retryAfter < 0 { // non-retryable
			return nil, "", attemptErr
		}
		wait := retryAfter
		if wait == 0 {
			wait = time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, 8s
		}
		c.logger.Debug("retrying Meraki API request", "url", rawURL, "attempt", attempt+1, "wait", wait, "err", attemptErr)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return nil, "", fmt.Errorf("giving up after %d retries: %w", maxRetries, lastErr)
}

// doOnce performs a single attempt. retryAfter semantics:
// -1 = do not retry; 0 = retry with default backoff; >0 = retry after this duration.
func (c *Client) doOnce(ctx context.Context, rawURL string) (body []byte, next string, retryAfter time.Duration, err error) {
	// Rate limit: serialize and space out requests.
	select {
	case tok := <-c.throttle:
		if gap := time.Since(c.lastReq); gap < minRequestGap {
			time.Sleep(minRequestGap - gap)
		}
		c.lastReq = time.Now()
		c.throttle <- tok
	case <-ctx.Done():
		return nil, "", -1, ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, "", -1, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", 0, err // network error: retryable with backoff
	}
	defer func() { _ = resp.Body.Close() }()
	c.requestsTotal.WithLabelValues(strconv.Itoa(resp.StatusCode)).Inc()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", 0, err
		}
		if m := linkNextRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			next = m[1]
		}
		return b, next, -1, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := parseRetryAfter(resp.Header.Get("Retry-After"), time.Second)
		return nil, "", ra, fmt.Errorf("rate limited (429) on %s", redactURL(rawURL))
	case resp.StatusCode >= http.StatusInternalServerError:
		return nil, "", 0, fmt.Errorf("server error %d on %s", resp.StatusCode, redactURL(rawURL))
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return nil, "", -1, fmt.Errorf("unexpected status %d on %s: %s", resp.StatusCode, redactURL(rawURL), strings.TrimSpace(string(b)))
	}
}

// parseRetryAfter interprets a Retry-After header value, which per RFC 7231
// may be delta-seconds or an HTTP-date. Returns def when absent or invalid.
func parseRetryAfter(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return def
}

func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery = ""
		return u.String()
	}
	return raw
}

// get fetches a single (non-paginated) endpoint into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	body, _, err := c.do(ctx, u)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// getPaginated fetches all pages of a list endpoint, calling decode for
// each page's raw JSON body. query may be nil. pageSize sets the perPage
// parameter — it must respect the endpoint's cap or Meraki returns HTTP 400
// (endpoints disagree: most accept perPageDefault, a few cap far lower).
func (c *Client) getPaginated(ctx context.Context, path string, query url.Values, pageSize int, decode func([]byte) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("perPage", strconv.Itoa(pageSize))
	next := c.baseURL + path + "?" + query.Encode()
	for next != "" {
		body, n, err := c.do(ctx, next)
		if err != nil {
			return err
		}
		if err := decode(body); err != nil {
			return err
		}
		next = n
	}
	return nil
}

// decodePages returns a decode func that appends each page into dst
// (a pointer to a slice).
func decodePages[T any](dst *[]T) func([]byte) error {
	return func(b []byte) error {
		var page []T
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		*dst = append(*dst, page...)
		return nil
	}
}

// ---- Endpoint methods ----

func (c *Client) GetOrganizations(ctx context.Context) ([]Organization, error) {
	var orgs []Organization
	err := c.get(ctx, "/organizations", nil, &orgs)
	return orgs, err
}

func (c *Client) GetNetworks(ctx context.Context, orgID string) ([]Network, error) {
	var nets []Network
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/networks", nil, perPageDefault, decodePages(&nets))
	return nets, err
}

func (c *Client) GetDevices(ctx context.Context, orgID string) ([]Device, error) {
	var devs []Device
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/devices", nil, perPageDefault, decodePages(&devs))
	return devs, err
}

func (c *Client) GetDeviceAvailabilities(ctx context.Context, orgID string) ([]DeviceAvailability, error) {
	var avail []DeviceAvailability
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/devices/availabilities", nil, perPageDefault, decodePages(&avail))
	return avail, err
}

func (c *Client) GetApplianceUplinkStatuses(ctx context.Context, orgID string) ([]ApplianceUplinkStatus, error) {
	var st []ApplianceUplinkStatus
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/appliance/uplink/statuses", nil, perPageDefault, decodePages(&st))
	return st, err
}

func (c *Client) GetApplianceVPNStatuses(ctx context.Context, orgID string) ([]ApplianceVPNStatus, error) {
	var st []ApplianceVPNStatus
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/appliance/vpn/statuses", nil, perPageVPNStatuses, decodePages(&st))
	return st, err
}

// GetSwitchPortStatuses paginates switch port statuses. Unlike the other
// list endpoints, each page body is wrapped as {items, meta} rather than a
// bare array, so it cannot use decodePages.
func (c *Client) GetSwitchPortStatuses(ctx context.Context, orgID string) ([]SwitchPortsBySwitch, error) {
	var out []SwitchPortsBySwitch
	decode := func(b []byte) error {
		var page struct {
			Items []SwitchPortsBySwitch `json:"items"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		out = append(out, page.Items...)
		return nil
	}
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/switch/ports/statuses/bySwitch", nil, perPageSwitchPorts, decode)
	return out, err
}

func (c *Client) GetSensorReadingsLatest(ctx context.Context, orgID string) ([]SensorReadingsLatest, error) {
	var out []SensorReadingsLatest
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/sensor/readings/latest", nil, perPageSensors, decodePages(&out))
	return out, err
}

// GetOrgAssuranceAlerts fetches currently open (active, not dismissed, not
// resolved) organization-wide Assurance alerts. The filter is passed
// explicitly rather than relying on the endpoint's documented defaults, so a
// future default change on Meraki's side can't silently start returning
// resolved/dismissed alerts here.
func (c *Client) GetOrgAssuranceAlerts(ctx context.Context, orgID string) ([]AssuranceAlert, error) {
	var out []AssuranceAlert
	q := url.Values{"active": {"true"}, "dismissed": {"false"}, "resolved": {"false"}}
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/assurance/alerts", q, perPageAssurance, decodePages(&out))
	return out, err
}

func (c *Client) GetUplinksLossAndLatency(ctx context.Context, orgID string) ([]UplinkLossLatency, error) {
	var ll []UplinkLossLatency
	q := url.Values{"timespan": {"300"}} // last 5 minutes
	err := c.get(ctx, "/organizations/"+orgID+"/devices/uplinksLossAndLatency", q, &ll)
	return ll, err
}

func (c *Client) GetOrgClientsOverview(ctx context.Context, orgID string, timespan time.Duration) (*OrgClientsOverview, error) {
	var ov OrgClientsOverview
	q := url.Values{"timespan": {strconv.Itoa(int(timespan.Seconds()))}}
	err := c.get(ctx, "/organizations/"+orgID+"/clients/overview", q, &ov)
	return &ov, err
}

func (c *Client) GetNetworkClientsOverview(ctx context.Context, networkID string, timespan time.Duration) (*NetworkClientsOverview, error) {
	var ov NetworkClientsOverview
	q := url.Values{"timespan": {strconv.Itoa(int(timespan.Seconds()))}}
	err := c.get(ctx, "/networks/"+networkID+"/clients/overview", q, &ov)
	return &ov, err
}

func (c *Client) GetWirelessChannelUtilization(ctx context.Context, orgID string) ([]ChannelUtilization, error) {
	var cu []ChannelUtilization
	err := c.getPaginated(ctx, "/organizations/"+orgID+"/wireless/devices/channelUtilization/byDevice", nil, perPageDefault, decodePages(&cu))
	return cu, err
}

func (c *Client) GetLicenseOverview(ctx context.Context, orgID string) (*LicenseOverview, error) {
	var lo LicenseOverview
	err := c.get(ctx, "/organizations/"+orgID+"/licenses/overview", nil, &lo)
	return &lo, err
}

func (c *Client) GetAPIRequestsOverview(ctx context.Context, orgID string, timespan time.Duration) (*APIRequestsOverview, error) {
	var ov APIRequestsOverview
	q := url.Values{"timespan": {strconv.Itoa(int(timespan.Seconds()))}}
	err := c.get(ctx, "/organizations/"+orgID+"/apiRequests/overview", q, &ov)
	return &ov, err
}
