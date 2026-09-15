package collector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/machani/meraki-exporter/internal/config"
	"github.com/machani/meraki-exporter/internal/meraki"
)

// readMetric extracts label values and the gauge value from a const metric.
func readMetric(t *testing.T, m prometheus.Metric) (labels map[string]string, value float64) {
	t.Helper()
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		t.Fatalf("writing metric: %v", err)
	}
	labels = map[string]string{}
	for _, lp := range d.Label {
		labels[lp.GetName()] = lp.GetValue()
	}
	return labels, d.GetGauge().GetValue()
}

func TestStateSetKnownState(t *testing.T) {
	ms := stateSet(descDeviceStatus, deviceStates, "online", "Q234", "ap1", "N_1", "HQ")
	if len(ms) != len(deviceStates) {
		t.Fatalf("got %d series, want %d (one per known state)", len(ms), len(deviceStates))
	}
	ones := 0
	for _, m := range ms {
		labels, v := readMetric(t, m)
		switch {
		case labels["status"] == "online":
			if v != 1 {
				t.Errorf("status=online: got %v, want 1", v)
			}
			ones++
		case v != 0:
			t.Errorf("status=%s: got %v, want 0", labels["status"], v)
		}
		if labels["serial"] != "Q234" || labels["network_name"] != "HQ" {
			t.Errorf("unexpected labels: %v", labels)
		}
	}
	if ones != 1 {
		t.Errorf("series with value 1: got %d, want exactly 1", ones)
	}
}

func TestStateSetUnknownState(t *testing.T) {
	ms := stateSet(descUplinkStatus, uplinkStates, "hibernating", "Q234", "N_1", "HQ", "wan1", "standalone")
	if len(ms) != len(uplinkStates)+1 {
		t.Fatalf("got %d series, want %d (known states + unknown)", len(ms), len(uplinkStates)+1)
	}
	labels, v := readMetric(t, ms[len(ms)-1])
	if labels["status"] != "hibernating" || v != 1 {
		t.Errorf("unknown state must be emitted with value 1: labels=%v v=%v", labels, v)
	}
	for _, m := range ms[:len(ms)-1] {
		if _, v := readMetric(t, m); v != 0 {
			t.Errorf("known state series must be 0 when actual state is unknown, got %v", v)
		}
	}
}

func TestStateSetEmptyStatus(t *testing.T) {
	ms := stateSet(descDeviceStatus, deviceStates, "", "Q234", "ap1", "N_1", "HQ")
	if len(ms) != len(deviceStates) {
		t.Fatalf("got %d series, want %d (no extra series for empty status)", len(ms), len(deviceStates))
	}
	for _, m := range ms {
		if _, v := readMetric(t, m); v != 0 {
			t.Errorf("all series must be 0 for empty status, got %v", v)
		}
	}
}

// haFixtureJSON mirrors a typical org: a real HA pair (MX1/MX2), a
// standalone appliance that Meraki still labels "primary" (MX3), and an
// appliance whose response omits the highAvailability block entirely (MX4).
const haFixtureJSON = `[
	{"serial":"MX1","networkId":"N_1","highAvailability":{"enabled":true,"role":"primary"},
	 "uplinks":[{"interface":"wan1","status":"active","ip":"10.0.0.1"},
	            {"interface":"wan2","status":"ready","ip":"10.0.1.1"}]},
	{"serial":"MX2","networkId":"N_1","highAvailability":{"enabled":true,"role":"spare"},
	 "uplinks":[{"interface":"wan1","status":"active","ip":"10.0.0.2"},
	            {"interface":"wan2","status":"ready","ip":"10.0.1.2"}]},
	{"serial":"MX3","networkId":"N_2","highAvailability":{"enabled":false,"role":"primary"},
	 "uplinks":[{"interface":"wan1","status":"active","ip":"10.0.2.1"}]},
	{"serial":"MX4","networkId":"N_2",
	 "uplinks":[{"interface":"wan1","status":"active","ip":"10.0.2.2"}]}
]`

// haSnapshot indexes one collectUplinks run by serial, so the assertions read
// as statements about appliances rather than about a slice of metrics.
type haSnapshot struct {
	enabled map[string]float64            // serial -> value
	role    map[string]map[string]float64 // serial -> role -> value
	uplink  map[string]map[string]float64 // serial -> "interface/status" -> value
	// uplinkRole is the role label carried on the uplink series themselves,
	// which is a different thing from the ha_role metric: it exists for every
	// device with an uplink, including ones that report no HA block.
	uplinkRole map[string]string // serial -> role label
}

func putNested(m map[string]map[string]float64, outer, inner string, v float64) {
	if m[outer] == nil {
		m[outer] = map[string]float64{}
	}
	m[outer][inner] = v
}

// collectHASnapshot runs the uplinks collector against haFixtureJSON.
func collectHASnapshot(t *testing.T) *haSnapshot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "uplinksLossAndLatency") || strings.Contains(r.URL.Path, "usage/byNetwork") {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		_, _ = fmt.Fprint(w, haFixtureJSON)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{client: client, logger: logger, orgID: "O_1"}

	metrics, err := e.collectUplinks(context.Background(), map[string]string{"N_1": "HQ", "N_2": "Branch"})
	if err != nil {
		t.Fatalf("collectUplinks: %v", err)
	}

	snap := &haSnapshot{
		enabled:    map[string]float64{},
		role:       map[string]map[string]float64{},
		uplink:     map[string]map[string]float64{},
		uplinkRole: map[string]string{},
	}
	for _, m := range metrics {
		labels, v := readMetric(t, m)
		switch m.Desc() {
		case descApplianceHAEnabled:
			snap.enabled[labels["serial"]] = v
		case descApplianceHARole:
			putNested(snap.role, labels["serial"], labels["role"], v)
		case descUplinkStatus:
			putNested(snap.uplink, labels["serial"], labels["interface"]+"/"+labels["status"], v)
			snap.uplinkRole[labels["serial"]] = labels["role"]
		}
	}
	return snap
}

// assertHAEnabledGuard covers the trap that fired the old alert org-wide: a
// standalone appliance is only distinguishable from a real HA member by
// ha_enabled, and an appliance that reports no HA block at all must emit no
// HA series (absent is not the same as disabled).
func assertHAEnabledGuard(t *testing.T, snap *haSnapshot) {
	t.Helper()
	want := map[string]float64{"MX1": 1, "MX2": 1, "MX3": 0}
	for serial, w := range want {
		got, ok := snap.enabled[serial]
		if !ok || got != w {
			t.Errorf("meraki_appliance_ha_enabled{serial=%q}: got %v (present=%v), want %v", serial, got, ok, w)
		}
	}
	if _, ok := snap.enabled["MX4"]; ok {
		t.Error("MX4 omits the highAvailability block; it must emit no ha_enabled series")
	}
	if _, ok := snap.role["MX4"]; ok {
		t.Error("MX4 omits the highAvailability block; it must emit no ha_role series")
	}
}

// assertRoleStateSet checks ha_role behaves as a state set and that the role
// values are self-describing: a genuine pair reports primary/spare, and an
// appliance with HA switched off reports "standalone" rather than the
// "primary" Meraki sends for it. That mapping is the whole point of the value
// fix — it is what lets a role query be correct without an ha_enabled guard.
func assertRoleStateSet(t *testing.T, snap *haSnapshot) {
	t.Helper()
	for _, serial := range []string{"MX1", "MX2", "MX3"} {
		roles := snap.role[serial]
		if len(roles) != len(haRoles) {
			t.Errorf("ha_role{serial=%q}: got %d series, want %d (one per known role)", serial, len(roles), len(haRoles))
		}
		ones := 0
		for _, v := range roles {
			if v == 1 {
				ones++
			}
		}
		if ones != 1 {
			t.Errorf("ha_role{serial=%q}: %d series at 1, want exactly 1", serial, ones)
		}
	}
	// The genuine pair keeps the roles Meraki reports.
	if snap.role["MX1"]["primary"] != 1 || snap.role["MX2"]["spare"] != 1 {
		t.Errorf("HA pair roles: MX1 primary=%v, MX2 spare=%v, want 1 and 1",
			snap.role["MX1"]["primary"], snap.role["MX2"]["spare"])
	}
	// The standalone appliance must NOT inherit Meraki's "primary". That is
	// the trap which fired the first warm-spare alert across the whole org.
	if snap.role["MX3"]["standalone"] != 1 {
		t.Errorf("MX3 has HA disabled and must report role=standalone (got %v)", snap.role["MX3"]["standalone"])
	}
	if snap.role["MX3"]["primary"] != 0 {
		t.Errorf("MX3 has HA disabled and must not report role=primary (got %v)", snap.role["MX3"]["primary"])
	}
}

// assertUplinkRoleLabel pins the role carried on the uplink series themselves
// which is what the dashboard legends
// read. It comes from the HA block on the same response, and a device that
// reports no HA block at all is labelled "unknown" rather than guessed at —
// the loss/latency endpoint covers device types the appliance endpoint does
// not, so a missing role is a real state, not an error.
func assertUplinkRoleLabel(t *testing.T, snap *haSnapshot) {
	t.Helper()
	want := map[string]string{
		"MX1": "primary",
		"MX2": "spare",
		"MX3": "standalone",
		"MX4": roleUnknown,
	}
	for serial, w := range want {
		if got := snap.uplinkRole[serial]; got != w {
			t.Errorf("uplink role label for %s: got %q, want %q", serial, got, w)
		}
	}
}

// TestCollectUplinksHAMetrics pins the warm-spare data shape that the
// MerakiWarmSpareFailover and MerakiWarmSpareUnavailable rules in
// prometheus/alerts.yml are built on, and the role values those rules read.
// Both invariants below have already produced production false positives
// (fixed 2026-07-29).
func TestCollectUplinksHAMetrics(t *testing.T) {
	snap := collectHASnapshot(t)
	assertHAEnabledGuard(t, snap)
	assertRoleStateSet(t, snap)
	assertUplinkRoleLabel(t, snap)

	// A healthy standby spare holds an active uplink of its own. Any rule that
	// treats that as failover fires on every healthy pair.
	if snap.uplink["MX2"]["wan1/active"] != 1 {
		t.Errorf("healthy spare MX2 wan1 active: got %v, want 1", snap.uplink["MX2"]["wan1/active"])
	}
	if snap.uplink["MX2"]["wan2/ready"] != 1 {
		t.Errorf("healthy spare MX2 wan2 ready: got %v, want 1", snap.uplink["MX2"]["wan2/ready"])
	}
}

// TestUplinkUsageMetrics covers what this endpoint's shape forces on the
// collector: an HA pair reports the same site uplink once per appliance, and
// those rows must be summed. Emitting both would put two conflicting series
// under one identity, and the registry rejects the entire scrape on a
// duplicate — taking every other uplink metric down with it.
//
// The figures need no unit conversion: this endpoint reports bytes already,
// unlike several of its siblings that report kilobytes.
func TestUplinkUsageMetrics(t *testing.T) {
	const window = 5 * time.Minute
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("timespan"); got != "300" {
			t.Errorf("timespan: got %q, want 300 (the poll interval, so samples do not overlap)", got)
		}
		_, _ = fmt.Fprint(w, `[
			{"networkId":"N_1","name":"HQ","byUplink":[
				{"serial":"MX1","interface":"wan1","sent":100,"received":200},
				{"serial":"MX2","interface":"wan1","sent":50,"received":25},
				{"serial":"MX1","interface":"wan2","sent":10,"received":20}]},
			{"networkId":"N_2","name":"Branch","byUplink":[
				{"serial":"MX3","interface":"wan1","sent":1,"received":2}]}
		]`)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{client: client, logger: logger, orgID: "O_1", interval: window}

	metrics := e.uplinkUsageMetrics(context.Background(), map[string]string{"N_1": "HQ", "N_2": "Branch"})

	sent := map[string]float64{}
	received := map[string]float64{}
	var windowSeconds float64
	for _, m := range metrics {
		labels, v := readMetric(t, m)
		key := labels["network_name"] + "/" + labels["interface"]
		switch m.Desc() {
		case descUplinkSentBytes:
			sent[key] = v
		case descUplinkReceivedBytes:
			received[key] = v
		case descUplinkUsageWindow:
			windowSeconds = v
		}
	}

	// Three distinct site uplinks from four rows: the HA pair's two wan1 rows
	// collapse into one. Anything else means the dedupe is broken.
	if len(metrics) != 2*3+1 {
		t.Errorf("got %d metrics, want 7 (3 site uplinks x sent/received + window)", len(metrics))
	}
	wantSent := map[string]float64{"HQ/wan1": 150, "HQ/wan2": 10, "Branch/wan1": 1}
	for k, want := range wantSent {
		if sent[k] != want {
			t.Errorf("sent bytes %s: got %v, want %v", k, sent[k], want)
		}
	}
	if received["HQ/wan1"] != 225 {
		t.Errorf("received bytes HQ/wan1: got %v, want 225 (200+25 summed across the HA pair)", received["HQ/wan1"])
	}
	if windowSeconds != window.Seconds() {
		t.Errorf("window: got %v, want %v", windowSeconds, window.Seconds())
	}
}

// TestUplinkUsageMetricsSurvivesFailure pins that a failing usage endpoint
// costs only the volume metrics. Uplink status rides in the same collector and
// the warm-spare alerts depend on it, so this must never propagate an error.
//
// The fixture returns 400, which is both the realistic failure (an org whose
// endpoint rejects the timespan, the way the clients endpoint rejects windows
// under 24h) and non-retryable, so the test does not sit through the client's
// 1/2/4/8s backoff ladder.
func TestUplinkUsageMetricsSurvivesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{client: client, logger: logger, orgID: "O_1", interval: 5 * time.Minute}

	if got := e.uplinkUsageMetrics(context.Background(), map[string]string{}); got != nil {
		t.Errorf("got %d metrics on API failure, want none", len(got))
	}
}

func TestParseCoTermExpiration(t *testing.T) {
	got, err := parseCoTermExpiration("Mar 16, 2027 UTC")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := time.Date(2027, time.March, 16, 0, 0, 0, 0, time.UTC)
	if got.Unix() != want.Unix() {
		t.Errorf("got %v, want %v", got, want)
	}

	for _, bad := range []string{"N/A", "", "2027-03-16", "someday"} {
		if _, err := parseCoTermExpiration(bad); err == nil {
			t.Errorf("parseCoTermExpiration(%q): expected error", bad)
		}
	}
}

func TestCountAlertsBySeverityCategory(t *testing.T) {
	alerts := []meraki.AssuranceAlert{
		{Severity: "critical", CategoryType: "connectivity"},
		{Severity: "critical", CategoryType: "connectivity"},
		{Severity: "critical", CategoryType: "device_health"},
		{Severity: "warning", CategoryType: "connectivity"},
	}
	counts := countAlertsBySeverityCategory(alerts)

	cases := []struct {
		key  alertCategoryKey
		want float64
	}{
		{alertCategoryKey{"critical", "connectivity"}, 2},
		{alertCategoryKey{"critical", "device_health"}, 1},
		{alertCategoryKey{"warning", "connectivity"}, 1},
	}
	for _, tc := range cases {
		if got := counts[tc.key]; got != tc.want {
			t.Errorf("counts[%+v]: got %v, want %v", tc.key, got, tc.want)
		}
	}
	if len(counts) != len(cases) {
		t.Errorf("counts: got %d keys, want %d", len(counts), len(cases))
	}

	if got := countAlertsBySeverityCategory(nil); len(got) != 0 {
		t.Errorf("countAlertsBySeverityCategory(nil): got %v, want empty map", got)
	}
}

func TestOrgClientsWindow(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"default 1h is floored to 24h", time.Hour, 24 * time.Hour},
		{"exactly 24h is unchanged", 24 * time.Hour, 24 * time.Hour},
		{"larger window is honoured", 7 * 24 * time.Hour, 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orgClientsWindow(tc.configured); got != tc.want {
				t.Errorf("orgClientsWindow(%v): got %v, want %v", tc.configured, got, tc.want)
			}
		})
	}
}

// TestPollResolvesOrgOnce covers a configured org id the API key cannot see.
// resolveOrg can do nothing useful about that, so it must not re-list
// organizations on every poll — which it did until 2026-08-13, burning an API
// call and logging the same warning every cycle for the life of the process.
func TestPollResolvesOrgOnce(t *testing.T) {
	var orgCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/organizations" {
			orgCalls.Add(1)
			// Deliberately not the configured org.
			_, _ = fmt.Fprint(w, `[{"id":"O_SOMEONE_ELSE","name":"Other Org"}]`)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{
		cfg:      &config.Config{},
		client:   client,
		logger:   logger,
		group:    "fast",
		names:    []string{"devices"},
		interval: time.Minute,
		orgID:    "O_MINE",
	}

	e.poll(context.Background())
	e.poll(context.Background())
	e.poll(context.Background())

	if got := orgCalls.Load(); got != 1 {
		t.Errorf("GetOrganizations called %d times across 3 polls, want 1", got)
	}
	if !e.Ready() {
		t.Error("exporter should still be primed: an unresolvable org must not block serving metrics")
	}
}

// TestPollRetriesOrgResolutionAfterFailure is the other half of the contract:
// a hard failure (API down, bad key) must keep being retried, or an exporter
// started before the API is reachable would never recover.
func TestPollRetriesOrgResolutionAfterFailure(t *testing.T) {
	var orgCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/organizations" {
			orgCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{cfg: &config.Config{}, client: client, logger: logger, group: "fast", names: []string{"devices"}, interval: time.Minute}

	e.poll(context.Background())
	e.poll(context.Background())

	if got := orgCalls.Load(); got != 2 {
		t.Errorf("GetOrganizations called %d times across 2 failing polls, want 2 (must keep retrying)", got)
	}
}

// TestPollFailsClientsWhenNetworksUnavailable pins that losing the network
// list makes the clients collector FAIL rather than quietly report only the
// org total. It used to succeed: poll treated a GetNetworks error as cosmetic
// ("labels will be empty"), but collectClients iterates that map to decide
// which networks to query, so an empty map meant no per-network series at all
// while meraki_exporter_collector_success still read 1. Silent data loss that
// no alert could catch.
func TestPollFailsClientsWhenNetworksUnavailable(t *testing.T) {
	var clientCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/networks"):
			// 400 rather than 500: a real rejection, and non-retryable, so the
			// test does not sit through the client's backoff ladder.
			w.WriteHeader(http.StatusBadRequest)
		case strings.Contains(r.URL.Path, "clients/overview"):
			clientCalls.Add(1)
			_, _ = fmt.Fprint(w, `{"counts":{"total":42}}`)
		default:
			_, _ = fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{
		cfg:         &config.Config{ClientsTimespan: time.Hour},
		client:      client,
		logger:      logger,
		group:       "slow",
		names:       []string{"clients"},
		interval:    time.Minute,
		orgID:       "O_1",
		orgResolved: true,
	}

	e.poll(context.Background())

	var success float64 = -1
	for _, m := range e.cache {
		labels, v := readMetric(t, m)
		if m.Desc() == descCollectorSuccess && labels["collector"] == "clients" {
			success = v
		}
	}
	if success != 0 {
		t.Errorf("meraki_exporter_collector_success{collector=\"clients\"}: got %v, want 0", success)
	}
	if got := clientCalls.Load(); got != 0 {
		t.Errorf("clients overview called %d times, want 0: a collector that cannot succeed should not spend API budget", got)
	}
}

// TestVPNUsageMetrics covers the collector half of the VPN volume work: the
// kilobyte-to-byte conversion, skipping peers the API reports without a usage
// summary, and publishing the window so dashboards need not assume it.
func TestVPNUsageMetrics(t *testing.T) {
	// Deliberately shorter than vpnUsageMinWindow: the collector must ask for
	// the floored window, not this one.
	const pollInterval = 5 * time.Minute
	var gotTimespan string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimespan = r.URL.Query().Get("timespan")
		_, _ = fmt.Fprint(w, `[
			{"networkId":"N_1","networkName":"HQ","merakiVpnPeers":[
				{"networkId":"N_2","networkName":"DC01","usageSummary":{"sentInKilobytes":"29","receivedInKilobytes":"11"}},
				{"networkId":"N_3","networkName":"NoData"}
			]}
		]`)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{cfg: &config.Config{}, client: client, logger: logger, orgID: "O_1", interval: pollInterval}

	metrics := e.vpnUsageMetrics(context.Background(), map[string]string{"N_1": "HQ"})

	sent := map[string]float64{}
	received := map[string]float64{}
	var windowSeconds float64
	for _, m := range metrics {
		labels, v := readMetric(t, m)
		switch m.Desc() {
		case descVPNPeerSentBytes:
			sent[labels["peer_name"]] = v
		case descVPNPeerReceivedBytes:
			received[labels["peer_name"]] = v
		case descVPNUsageWindow:
			windowSeconds = v
		}
	}

	// One peer with data (sent + received) plus the window metric. The peer
	// without a usageSummary must be skipped, not reported as 0 bytes — a fake
	// zero is indistinguishable from a genuinely idle tunnel.
	if len(metrics) != 3 {
		t.Errorf("got %d metrics, want 3 (one peer x sent/received + window)", len(metrics))
	}
	if sent["DC01"] != 29000 {
		t.Errorf("sent bytes DC01: got %v, want 29000 (29 KB)", sent["DC01"])
	}
	if received["DC01"] != 11000 {
		t.Errorf("received bytes DC01: got %v, want 11000 (11 KB)", received["DC01"])
	}
	if _, ok := sent["NoData"]; ok {
		t.Error("peer with no usageSummary must emit no series")
	}
	// The endpoint does not answer a sub-hour window honestly, so the collector
	// must ask for the floor - and must PUBLISH the same figure. Requesting
	// 3600 while reporting 300 would overstate throughput twelvefold on every
	// panel that divides by this metric.
	if gotTimespan != "3600" {
		t.Errorf("requested timespan: got %q, want 3600 (floored, not the %v poll interval)", gotTimespan, pollInterval)
	}
	if windowSeconds != vpnUsageMinWindow.Seconds() {
		t.Errorf("published window: got %v, want %v - it must match the timespan actually requested",
			windowSeconds, vpnUsageMinWindow.Seconds())
	}
}

func TestVPNUsageWindow(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"a 5m poll is floored to an hour", 5 * time.Minute, time.Hour},
		{"exactly an hour is unchanged", time.Hour, time.Hour},
		{"a longer interval is honoured", 6 * time.Hour, 6 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vpnUsageWindow(tc.interval); got != tc.want {
				t.Errorf("vpnUsageWindow(%v): got %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}

// TestVPNUsageMetricsSurvivesFailure pins that a failing stats call costs only
// the volumes. Peer reachability rides in the same collector and drives
// MerakiVpnPeerUnreachable, so this must never propagate an error.
func TestVPNUsageMetricsSurvivesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{cfg: &config.Config{}, client: client, logger: logger, orgID: "O_1", interval: 5 * time.Minute}

	if got := e.vpnUsageMetrics(context.Background(), map[string]string{}); got != nil {
		t.Errorf("got %d metrics on API failure, want none", len(got))
	}
}

// TestCollectDevicesCoordinates pins behaviour a real deployment may never
// exercise: where every device happens to have coordinates, a regression that
// dropped or zeroed an unplaced device would pass unnoticed.
//
// Two properties matter. A device with no position must keep its
// meraki_device_info series with EMPTY lat/lng — dropping it would remove the
// device from the inventory table as well as the map. And empty must not
// become "0": a map panel skips an empty coordinate but happily plots 0,0 in
// the Atlantic.
func TestCollectDevicesCoordinates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/availabilities") {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		_, _ = fmt.Fprint(w, `[
			{"serial":"Q1","name":"hq-ap","model":"MR46","networkId":"N_1",
			 "lat":51.5074,"lng":-0.1278,"address":"1 Example St, London"},
			{"serial":"Q2","name":"unplaced","model":"MR46","networkId":"N_1"}
		]`)
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := meraki.New(srv.URL, "test-key", 5*time.Second, "test-agent", "", logger, nil)
	if err != nil {
		t.Fatalf("meraki.New: %v", err)
	}
	e := &Exporter{cfg: &config.Config{}, client: client, logger: logger, orgID: "O_1"}

	metrics, err := e.collectDevices(context.Background(), map[string]string{"N_1": "HQ"})
	if err != nil {
		t.Fatalf("collectDevices: %v", err)
	}

	info := map[string]map[string]string{}
	for _, m := range metrics {
		labels, _ := readMetric(t, m)
		if m.Desc() == descDeviceInfo {
			info[labels["serial"]] = labels
		}
	}
	if len(info) != 2 {
		t.Fatalf("got %d device_info series, want 2 — an unplaced device must not be dropped", len(info))
	}
	if got := info["Q1"]["lat"]; got != "51.5074" {
		t.Errorf("Q1 lat: got %q, want %q unchanged from the API", got, "51.5074")
	}
	if got := info["Q1"]["lng"]; got != "-0.1278" {
		t.Errorf("Q1 lng: got %q, want %q", got, "-0.1278")
	}
	if got := info["Q1"]["address"]; got != "1 Example St, London" {
		t.Errorf("Q1 address: got %q", got)
	}
	if got := info["Q2"]["lat"]; got != "" {
		t.Errorf("unplaced device lat: got %q, want empty — 0 would plot it at null island", got)
	}
	if got := info["Q2"]["lng"]; got != "" {
		t.Errorf("unplaced device lng: got %q, want empty", got)
	}
}
