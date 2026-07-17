package meraki

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(baseURL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestProxyTransport(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("no proxy leaves default transport", func(t *testing.T) {
		c, err := New("https://api.meraki.com/api/v1", "k", time.Second, "ua", "", logger, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if c.httpClient.Transport != nil {
			t.Error("expected nil Transport (stdlib default with env proxy support)")
		}
	})

	t.Run("proxy URL is wired into the transport", func(t *testing.T) {
		c, err := New("https://api.meraki.com/api/v1", "k", time.Second, "ua", "http://proxy.local:3128", logger, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		tr, ok := c.httpClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("expected *http.Transport, got %T", c.httpClient.Transport)
		}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.meraki.com/api/v1/organizations", http.NoBody)
		u, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("Proxy func: %v", err)
		}
		if u == nil || u.String() != "http://proxy.local:3128" {
			t.Errorf("proxy for API request: got %v, want http://proxy.local:3128", u)
		}
	})

	t.Run("invalid proxy URL is rejected", func(t *testing.T) {
		if _, err := New("https://api.meraki.com/api/v1", "k", time.Second, "ua", "http://%zz-bad", logger, nil); err == nil {
			t.Error("expected error for unparseable proxy URL")
		}
	})
}

// TestProxyEndToEnd proves requests actually flow through the proxy: the
// target host is unresolvable, so the request can only succeed if the
// client sends it to the proxy (which, for plain HTTP, receives the full
// target URL and answers on the origin's behalf).
func TestProxyEndToEnd(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "api.invalid" {
			t.Errorf("proxy received request for host %q, want api.invalid", r.URL.Host)
		}
		proxied.Add(1)
		_, _ = fmt.Fprint(w, `[{"id":"o1","name":"org"}]`)
	}))
	defer proxy.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New("http://api.invalid/api/v1", "k", 5*time.Second, "ua", proxy.URL, logger, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	orgs, err := c.GetOrganizations(context.Background())
	if err != nil {
		t.Fatalf("GetOrganizations via proxy: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != "o1" {
		t.Errorf("got %+v, want one org o1", orgs)
	}
	if proxied.Load() == 0 {
		t.Error("proxy never saw the request")
	}
}

func TestLinkNextRe(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string // "" = no match
	}{
		{
			"unquoted (Meraki style)",
			`<https://api.meraki.com/api/v1/organizations/1/devices?perPage=1000&startingAfter=Q234>; rel=next`,
			"https://api.meraki.com/api/v1/organizations/1/devices?perPage=1000&startingAfter=Q234",
		},
		{"quoted (RFC 8288)", `<https://example.com/page2>; rel="next"`, "https://example.com/page2"},
		{
			"multiple rels, unquoted",
			`<https://e.com/first>; rel=first, <https://e.com/p2>; rel=next, <https://e.com/last>; rel=last`,
			"https://e.com/p2",
		},
		{"multiple rels, quoted", `<https://e.com/first>; rel="first", <https://e.com/p2>; rel="next"`, "https://e.com/p2"},
		{"no next", `<https://e.com/first>; rel=first, <https://e.com/last>; rel=last`, ""},
		{"empty", "", ""},
		{"rel prefix is not next", `<https://e.com/x>; rel=nextpage`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if m := linkNextRe.FindStringSubmatch(tc.header); m != nil {
				got = m[1]
			}
			if got != tc.want {
				t.Errorf("header %q: got %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	def := time.Second
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty uses default", "", def},
		{"delta seconds", "7", 7 * time.Second},
		{"zero seconds uses default", "0", def},
		{"negative uses default", "-3", def},
		{"garbage uses default", "soon", def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in, def); got != tc.want {
				t.Errorf("parseRetryAfter(%q): got %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	t.Run("HTTP-date in the future", func(t *testing.T) {
		in := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
		got := parseRetryAfter(in, def)
		if got < 25*time.Second || got > 30*time.Second {
			t.Errorf("parseRetryAfter(%q): got %v, want ~30s", in, got)
		}
	})
	t.Run("HTTP-date in the past uses default", func(t *testing.T) {
		in := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
		if got := parseRetryAfter(in, def); got != def {
			t.Errorf("parseRetryAfter(%q): got %v, want %v", in, got, def)
		}
	})
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://api.meraki.com/api/v1/networks/N_1/clients?timespan=3600&secret=x")
	want := "https://api.meraki.com/api/v1/networks/N_1/clients"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetPaginated(t *testing.T) {
	var firstQuery atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/organizations/o1/networks", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header: got %q", got)
		}
		firstQuery.Store(r.URL.Query().Get("perPage"))
		// Quoted rel deliberately: proves both Link variants paginate.
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/page2>; rel="next", <http://%s/organizations/o1/networks>; rel=first`, r.Host, r.Host))
		_, _ = fmt.Fprint(w, `[{"id":"N1","name":"one"}]`)
	})
	mux.HandleFunc("/page2", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[{"id":"N2","name":"two"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv.URL)
	nets, err := c.GetNetworks(context.Background(), "o1")
	if err != nil {
		t.Fatalf("GetNetworks: %v", err)
	}
	if len(nets) != 2 || nets[0].ID != "N1" || nets[1].ID != "N2" {
		t.Errorf("got %+v, want N1 and N2", nets)
	}
	if got := firstQuery.Load(); got != "1000" {
		t.Errorf("perPage query param: got %v, want 1000", got)
	}
}

func TestRetryOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = fmt.Fprint(w, `[{"id":"o1","name":"org"}]`)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	orgs, err := c.GetOrganizations(context.Background())
	if err != nil {
		t.Fatalf("GetOrganizations after 429: %v", err)
	}
	if len(orgs) != 1 || orgs[0].ID != "o1" {
		t.Errorf("got %+v, want one org o1", orgs)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("API calls: got %d, want 2 (one 429, one success)", n)
	}
}

func TestNonRetryableStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "org not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	_, err := c.GetOrganizations(context.Background())
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status 404: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("API calls: got %d, want 1 (404 must not be retried)", n)
	}
}

// TestGetSwitchPortStatusesItemsWrapper proves GetSwitchPortStatuses handles
// the {items, meta} response wrapper this endpoint uses (unlike the other
// list endpoints, which return a bare array) while still paginating via the
// same Link header as everything else.
func TestGetSwitchPortStatusesItemsWrapper(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/organizations/o1/switch/ports/statuses/bySwitch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/page2>; rel="next"`, r.Host))
		_, _ = fmt.Fprint(w, `{"items":[{"serial":"S1","network":{"id":"N_1"},"ports":[
			{"portId":"1","enabled":true,"status":"Connected","poe":{"isAllocated":true}}
		]}],"meta":{"counts":{"items":{"total":2,"remaining":1}}}}`)
	})
	mux.HandleFunc("/page2", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"items":[{"serial":"S2","network":{"id":"N_1"},"ports":[
			{"portId":"1","enabled":false,"status":"Disabled","poe":{"isAllocated":false}}
		]}],"meta":{"counts":{"items":{"total":2,"remaining":0}}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv.URL)
	switches, err := c.GetSwitchPortStatuses(context.Background(), "o1")
	if err != nil {
		t.Fatalf("GetSwitchPortStatuses: %v", err)
	}
	if len(switches) != 2 || switches[0].Serial != "S1" || switches[1].Serial != "S2" {
		t.Fatalf("got %+v, want switches S1 and S2 across both pages", switches)
	}
	if len(switches[0].Ports) != 1 || switches[0].Ports[0].Status != "Connected" || !switches[0].Ports[0].Poe.IsAllocated {
		t.Errorf("switch S1 port: got %+v", switches[0].Ports)
	}
}

// TestGetApplianceUplinkStatusesHA verifies the optional highAvailability
// block decodes when present and stays nil when the API omits it (non-HA
// appliances), so callers can distinguish "HA disabled" from "not reported".
func TestGetApplianceUplinkStatusesHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[
			{"serial":"MX1","networkId":"N_1","highAvailability":{"enabled":true,"role":"primary"},
			 "uplinks":[{"interface":"wan1","status":"active","ip":"1.2.3.4"}]},
			{"serial":"MX2","networkId":"N_1","highAvailability":{"enabled":true,"role":"spare"},
			 "uplinks":[{"interface":"wan1","status":"ready","ip":"1.2.3.5"}]},
			{"serial":"MX3","networkId":"N_2",
			 "uplinks":[{"interface":"wan1","status":"active","ip":"1.2.3.6"}]}
		]`)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	statuses, err := c.GetApplianceUplinkStatuses(context.Background(), "o1")
	if err != nil {
		t.Fatalf("GetApplianceUplinkStatuses: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("got %d appliances, want 3", len(statuses))
	}
	if ha := statuses[0].HighAvailability; ha == nil || !ha.Enabled || ha.Role != "primary" {
		t.Errorf("MX1 HA: got %+v, want enabled primary", ha)
	}
	if ha := statuses[1].HighAvailability; ha == nil || ha.Role != "spare" {
		t.Errorf("MX2 HA: got %+v, want spare", ha)
	}
	if statuses[2].HighAvailability != nil {
		t.Errorf("MX3 HA: got %+v, want nil (block omitted by API)", statuses[2].HighAvailability)
	}
}

// TestPerPageWithinEndpointCaps guards against the regression where every
// paginated endpoint sent perPage=1000: Meraki rejects that with HTTP 400 on
// the vpn (max 300), alerts (max 300), switch-ports (max 20), and sensor
// (max 100) endpoints. Each must send a page size within its documented cap.
func TestPerPageWithinEndpointCaps(t *testing.T) {
	seen := map[string]string{}
	mux := http.NewServeMux()
	record := func(path, body string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			seen[path] = r.URL.Query().Get("perPage")
			_, _ = fmt.Fprint(w, body)
		})
	}
	record("/organizations/o1/appliance/vpn/statuses", `[]`)
	record("/organizations/o1/assurance/alerts", `[]`)
	record("/organizations/o1/switch/ports/statuses/bySwitch", `{"items":[],"meta":{"counts":{"items":{"total":0,"remaining":0}}}}`)
	record("/organizations/o1/sensor/readings/latest", `[]`)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient(t, srv.URL)
	ctx := context.Background()
	if _, err := c.GetApplianceVPNStatuses(ctx, "o1"); err != nil {
		t.Fatalf("GetApplianceVPNStatuses: %v", err)
	}
	if _, err := c.GetOrgAssuranceAlerts(ctx, "o1"); err != nil {
		t.Fatalf("GetOrgAssuranceAlerts: %v", err)
	}
	if _, err := c.GetSwitchPortStatuses(ctx, "o1"); err != nil {
		t.Fatalf("GetSwitchPortStatuses: %v", err)
	}
	if _, err := c.GetSensorReadingsLatest(ctx, "o1"); err != nil {
		t.Fatalf("GetSensorReadingsLatest: %v", err)
	}

	// path -> [min, max] Meraki accepts.
	caps := map[string][2]int{
		"/organizations/o1/appliance/vpn/statuses":         {3, 300},
		"/organizations/o1/assurance/alerts":               {4, 300},
		"/organizations/o1/switch/ports/statuses/bySwitch": {3, 20},
		"/organizations/o1/sensor/readings/latest":         {3, 100},
	}
	for path, lohi := range caps {
		n, err := strconv.Atoi(seen[path])
		if err != nil {
			t.Errorf("%s: perPage %q not an integer", path, seen[path])
			continue
		}
		if n < lohi[0] || n > lohi[1] {
			t.Errorf("%s: perPage=%d outside Meraki's accepted range [%d, %d]", path, n, lohi[0], lohi[1])
		}
	}
}
