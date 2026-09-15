// Per-domain collector implementations.
package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/machani/meraki-exporter/internal/meraki"
)

const (
	// apiUsageWindow is the lookback for the org-wide API request overview.
	apiUsageWindow      = 24 * time.Hour
	orgClientsMinWindow = 24 * time.Hour
	// vpnUsageMinWindow floors the timespan sent to the VPN stats endpoint,
	// which does not answer shorter windows coherently: it serves a fixed
	// bucket rather than the rolling window asked for, producing a traffic
	// graph of spikes separated by flat zero. 1h is the shortest window known
	// to be coherent; shorter values are not simply a smaller bucket, so do
	// not lower this without evidence for the specific value.
	vpnUsageMinWindow = time.Hour
	// msPerSecond converts the API's millisecond latencies to seconds.
	msPerSecond = 1000.0
	// bytesPerKilobyte converts the VPN stats endpoint's kilobyte figures to
	// Prometheus base units. Note the appliance uplink usage endpoint reports
	// BYTES already and needs no conversion — the two disagree, so check each
	// endpoint rather than carrying an assumption between them.
	bytesPerKilobyte = 1000.0
)

// orgClientsWindow returns the timespan to use for the org-wide clients
// overview: the configured value, floored at orgClientsMinWindow so the org
// total is never silently zero. A larger configured window is honoured.
func orgClientsWindow(configured time.Duration) time.Duration {
	if configured < orgClientsMinWindow {
		return orgClientsMinWindow
	}
	return configured
}

// vpnUsageWindow returns the timespan to request from the VPN stats endpoint:
// the poll interval, floored at vpnUsageMinWindow. A longer interval is
// honoured unchanged.
//
// Consequence to be aware of rather than surprised by: at a poll interval
// shorter than the floor, consecutive samples OVERLAP - each covers the
// preceding hour. They therefore cannot be summed into a period total. Read
// the rate instead, which is what the dashboard panels do by dividing by
// meraki_vpn_usage_window_seconds.
func vpnUsageWindow(interval time.Duration) time.Duration {
	if interval < vpnUsageMinWindow {
		return vpnUsageMinWindow
	}
	return interval
}

// boolMetric emits a 0/1 gauge from a bool.
func boolMetric(desc *prometheus.Desc, v bool, labels ...string) prometheus.Metric {
	f := 0.0
	if v {
		f = 1
	}
	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, f, labels...)
}

// stateSet emits one 0/1 series per known state so a missing series never
// masks an outage; an unknown current state is emitted as an extra series to
// stay future-proof. The state is appended as the final label value.
func stateSet(desc *prometheus.Desc, states []string, current string, labels ...string) []prometheus.Metric {
	out := make([]prometheus.Metric, 0, len(states)+1)
	seen := false
	for _, s := range states {
		v := 0.0
		if current == s {
			v = 1
			seen = true
		}
		lv := append(append(make([]string, 0, len(labels)+1), labels...), s)
		out = append(out, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, lv...))
	}
	if !seen && current != "" {
		lv := append(append(make([]string, 0, len(labels)+1), labels...), current)
		out = append(out, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1, lv...))
	}
	return out
}

// haRoleValue reports the warm-spare role to expose for an appliance. Meraki
// sends a highAvailability block for every MX and labels standalone units
// "primary" with enabled=false, so a large share of appliances can claim a
// role they do not have — enough to make a naive warm-spare alert fire
// org-wide. Reporting "standalone" instead makes the metric self-describing:
// a role query no longer has to be paired with meraki_appliance_ha_enabled
// to be correct.
func haRoleValue(enabled bool, role string) string {
	if !enabled {
		return "standalone"
	}
	return role
}

// usageKey identifies one site uplink. The usage endpoint reports a serial per
// row, so an HA pair emits the same network+interface twice; keying on the
// pair and summing keeps the output one series per site uplink. Emitting both
// rows would make the registry reject the whole scrape as a duplicate.
type usageKey struct {
	networkID string
	iface     string
}

// uplinkUsageMetrics fetches per-site uplink volumes.
//
// The window is the poll interval: the endpoint returns a sum over whatever
// timespan is asked for, so matching it to the interval means consecutive
// samples neither overlap nor leave gaps. The window is published as its own
// metric rather than assumed by dashboards, so changing SCRAPE_INTERVAL cannot
// silently rescale existing panels.
//
// Failure is deliberately non-fatal: losing traffic volumes must not take
// uplink status — and with it the warm-spare alerts — down as well.
func (e *Exporter) uplinkUsageMetrics(ctx context.Context, nets map[string]string) []prometheus.Metric {
	usage, err := e.client.GetUplinksUsageByNetwork(ctx, e.orgID, e.interval)
	if err != nil {
		e.logger.Warn("failed to fetch uplink usage", "err", err)
		return nil
	}

	sent := map[usageKey]float64{}
	received := map[usageKey]float64{}
	for _, n := range usage {
		for _, u := range n.ByUplink {
			k := usageKey{networkID: n.NetworkID, iface: u.Interface}
			sent[k] += u.Sent
			received[k] += u.Received
		}
	}

	out := make([]prometheus.Metric, 0, 2*len(sent)+1)
	for k, v := range sent {
		out = append(out, prometheus.MustNewConstMetric(descUplinkSentBytes, prometheus.GaugeValue,
			v, k.networkID, nets[k.networkID], k.iface))
	}
	for k, v := range received {
		out = append(out, prometheus.MustNewConstMetric(descUplinkReceivedBytes, prometheus.GaugeValue,
			v, k.networkID, nets[k.networkID], k.iface))
	}
	out = append(out, prometheus.MustNewConstMetric(descUplinkUsageWindow, prometheus.GaugeValue,
		e.interval.Seconds()))
	return out
}

// roleUnknown labels uplink series for devices the appliance uplink-status
// response says nothing about. The loss/latency endpoint covers more device
// types than the appliance endpoint, so a serial can appear there with no HA
// information at all — that is genuinely unknown, not standalone, and saying
// so is better than guessing.
const roleUnknown = "unknown"

// uplinkRoles indexes the warm-spare role by serial. Loss and latency come
// from a different endpoint that carries no HA data, so the role has to be
// carried across from the uplink-status response to label them consistently.
func uplinkRoles(statuses []meraki.ApplianceUplinkStatus) map[string]string {
	roles := make(map[string]string, len(statuses))
	for _, dev := range statuses {
		role := roleUnknown
		if ha := dev.HighAvailability; ha != nil {
			role = haRoleValue(ha.Enabled, ha.Role)
		}
		roles[dev.Serial] = role
	}
	return roles
}

// roleFor looks up a serial's role, falling back to roleUnknown.
func roleFor(roles map[string]string, serial string) string {
	if role, ok := roles[serial]; ok {
		return role
	}
	return roleUnknown
}

// parseCoTermExpiration parses co-term license expiration dates as returned
// by the licenses/overview endpoint, e.g. "Mar 16, 2027 UTC".
func parseCoTermExpiration(s string) (time.Time, error) {
	return time.Parse("Jan 2, 2006 MST", s)
}

func (e *Exporter) collectDevices(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	devices, err := e.client.GetDevices(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	// Indexed rather than ranged by value: Device grew past gocritic's copy
	// threshold when the coordinate fields were added.
	for i := range devices {
		d := &devices[i]
		// Coordinates ride along on the devices response at no extra API cost.
		// A device with no position keeps its series with empty lat/lng rather
		// than being dropped: it must still appear in the inventory table, and
		// an empty coordinate is skipped by a map panel while 0,0 would not be.
		out = append(out, prometheus.MustNewConstMetric(descDeviceInfo, prometheus.GaugeValue, 1,
			d.Serial, d.Name, d.Model, d.MAC, d.Firmware, d.ProductType, d.NetworkID, nets[d.NetworkID],
			d.Lat.String(), d.Lng.String(), d.Address))
	}

	avail, err := e.client.GetDeviceAvailabilities(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	for _, a := range avail {
		out = append(out, stateSet(descDeviceStatus, deviceStates, a.Status,
			a.Serial, a.Name, a.Network.ID, nets[a.Network.ID])...)
	}
	return out, nil
}

func (e *Exporter) collectUplinks(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	statuses, err := e.client.GetApplianceUplinkStatuses(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	roles := uplinkRoles(statuses)
	for _, dev := range statuses {
		role := roleFor(roles, dev.Serial)
		for _, u := range dev.Uplinks {
			out = append(out, stateSet(descUplinkStatus, uplinkStates, u.Status,
				dev.Serial, dev.NetworkID, nets[dev.NetworkID], u.Interface, role)...)
		}
		// Warm-spare HA state rides along in the same API response — no
		// extra call. A spare keeps its own uplinks active while in standby,
		// so "spare has an active uplink" is NOT failover.
		if ha := dev.HighAvailability; ha != nil {
			out = append(out, boolMetric(descApplianceHAEnabled, ha.Enabled,
				dev.Serial, dev.NetworkID, nets[dev.NetworkID]))
			out = append(out, stateSet(descApplianceHARole, haRoles, haRoleValue(ha.Enabled, ha.Role),
				dev.Serial, dev.NetworkID, nets[dev.NetworkID])...)
		}
	}

	out = append(out, e.uplinkUsageMetrics(ctx, nets)...)

	lossLatency, err := e.client.GetUplinksLossAndLatency(ctx, e.orgID)
	if err != nil {
		// Loss/latency is a nice-to-have; keep uplink statuses if it fails.
		e.logger.Warn("failed to fetch uplink loss/latency", "err", err)
		return out, nil
	}
	for _, ll := range lossLatency {
		role := roleFor(roles, ll.Serial)
		var lossSum, latSum float64
		var lossN, latN int
		for _, p := range ll.TimeSeries {
			if p.LossPercent != nil {
				lossSum += *p.LossPercent
				lossN++
			}
			if p.LatencyMs != nil {
				latSum += *p.LatencyMs
				latN++
			}
		}
		if lossN > 0 {
			out = append(out, prometheus.MustNewConstMetric(descUplinkLossPercent, prometheus.GaugeValue,
				lossSum/float64(lossN), ll.Serial, ll.NetworkID, nets[ll.NetworkID], ll.Uplink, ll.IP, role))
		}
		if latN > 0 {
			out = append(out, prometheus.MustNewConstMetric(descUplinkLatencySeconds, prometheus.GaugeValue,
				latSum/float64(latN)/msPerSecond, ll.Serial, ll.NetworkID, nets[ll.NetworkID], ll.Uplink, ll.IP, role))
		}
	}
	return out, nil
}

func (e *Exporter) collectClients(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	// This collector uses the networks map as its work list, not just for
	// labels: without it the per-network loop below has nothing to iterate and
	// would emit only the org total, while still reporting success. Fail
	// instead, so meraki_exporter_collector_success drops to 0 and
	// MerakiCollectorFailing fires rather than the data quietly going missing.
	// Checked before the first API call — a doomed collector should not spend
	// API budget.
	if e.netsErr != nil {
		return nil, fmt.Errorf("network list unavailable, per-network client counts cannot be collected: %w", e.netsErr)
	}

	var out []prometheus.Metric

	ov, err := e.client.GetOrgClientsOverview(ctx, e.orgID, orgClientsWindow(e.cfg.ClientsTimespan))
	if err != nil {
		return nil, err
	}
	out = append(out, prometheus.MustNewConstMetric(descOrgClients, prometheus.GaugeValue, ov.Counts.Total))

	for id, name := range nets {
		nov, err := e.client.GetNetworkClientsOverview(ctx, id, e.cfg.ClientsTimespan)
		if err != nil {
			e.logger.Warn("failed to fetch network clients overview", "network_id", id, "err", err)
			continue
		}
		out = append(out,
			prometheus.MustNewConstMetric(descNetworkClients, prometheus.GaugeValue, nov.Counts.Total, id, name),
			prometheus.MustNewConstMetric(descNetworkClientsHeavy, prometheus.GaugeValue, nov.Counts.WithHeavyUsage, id, name),
		)
	}
	return out, nil
}

func (e *Exporter) collectWireless(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	utils, err := e.client.GetWirelessChannelUtilization(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	for _, u := range utils {
		for _, b := range u.ByBand {
			netName := nets[u.Network.ID]
			out = append(out,
				prometheus.MustNewConstMetric(descChannelUtilization, prometheus.GaugeValue, b.WiFi.Percentage,
					u.Serial, u.Network.ID, netName, b.Band, "wifi"),
				prometheus.MustNewConstMetric(descChannelUtilization, prometheus.GaugeValue, b.NonWiFi.Percentage,
					u.Serial, u.Network.ID, netName, b.Band, "non_wifi"),
				prometheus.MustNewConstMetric(descChannelUtilization, prometheus.GaugeValue, b.Total.Percentage,
					u.Serial, u.Network.ID, netName, b.Band, "total"),
			)
		}
	}
	return out, nil
}

func (e *Exporter) collectLicenses(ctx context.Context, _ map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	lo, err := e.client.GetLicenseOverview(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	out = append(out, boolMetric(descLicenseOK, lo.Status == "OK", lo.Status))

	// Co-term expiration dates look like "Mar 16, 2027 UTC".
	if lo.ExpirationDate != "" && lo.ExpirationDate != "N/A" {
		if t, err := parseCoTermExpiration(lo.ExpirationDate); err == nil {
			out = append(out, prometheus.MustNewConstMetric(descLicenseExpiration, prometheus.GaugeValue, float64(t.Unix())))
		} else {
			e.logger.Warn("could not parse license expiration date", "value", lo.ExpirationDate)
		}
	}
	for devType, count := range lo.LicensedDeviceCounts {
		out = append(out, prometheus.MustNewConstMetric(descLicensedDevices, prometheus.GaugeValue, count, devType))
	}
	return out, nil
}

func (e *Exporter) collectAPIUsage(ctx context.Context, _ map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	ov, err := e.client.GetAPIRequestsOverview(ctx, e.orgID, apiUsageWindow)
	if err != nil {
		return nil, err
	}
	for code, count := range ov.ResponseCodeCounts {
		if count == 0 {
			continue // the API returns every possible code; skip zeros to limit series
		}
		out = append(out, prometheus.MustNewConstMetric(descOrgAPIResponses, prometheus.GaugeValue, count, code))
	}
	return out, nil
}

func (e *Exporter) collectVPN(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	statuses, err := e.client.GetApplianceVPNStatuses(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	for _, s := range statuses {
		netName := nets[s.NetworkID]
		for _, p := range s.MerakiVPNPeers {
			out = append(out, stateSet(descVPNPeerStatus, vpnPeerStates, p.Reachability,
				s.NetworkID, netName, "meraki", p.NetworkName)...)
		}
		for _, p := range s.ThirdPartyVPNPeers {
			out = append(out, stateSet(descVPNPeerStatus, vpnPeerStates, p.Reachability,
				s.NetworkID, netName, "third_party", p.Name)...)
		}
	}

	out = append(out, e.vpnUsageMetrics(ctx, nets)...)
	return out, nil
}

// vpnUsageMetrics fetches per-tunnel traffic volumes.
//
// Same window reasoning as uplinkUsageMetrics: the endpoint returns a sum over
// whatever timespan is asked for, so matching it to the poll interval means
// consecutive samples neither overlap nor leave gaps, and the window is
// published as its own metric rather than assumed by dashboards.
//
// Failure is non-fatal, and that matters more here than for uplinks: peer
// reachability drives MerakiVpnPeerUnreachable, and traffic volumes are a
// nice-to-have. A malformed usage figure must not take an outage signal down
// with it — which is a live risk, since these values arrive as JSON strings
// and would fail to unmarshal if Meraki ever sent them as numbers.
func (e *Exporter) vpnUsageMetrics(ctx context.Context, nets map[string]string) []prometheus.Metric {
	window := vpnUsageWindow(e.interval)
	stats, err := e.client.GetVPNStats(ctx, e.orgID, window)
	if err != nil {
		e.logger.Warn("failed to fetch VPN stats; peer traffic volumes unavailable", "err", err)
		return nil
	}

	out := make([]prometheus.Metric, 0, 2*len(stats)+1)
	for _, s := range stats {
		netName := nets[s.NetworkID]
		if netName == "" {
			netName = s.NetworkName // this endpoint carries the name itself
		}
		for _, p := range s.MerakiVPNPeers {
			if p.UsageSummary == nil {
				continue // peer present but no traffic summary for this window
			}
			out = append(out,
				prometheus.MustNewConstMetric(descVPNPeerSentBytes, prometheus.GaugeValue,
					p.UsageSummary.SentInKilobytes*bytesPerKilobyte,
					s.NetworkID, netName, "meraki", p.NetworkName),
				prometheus.MustNewConstMetric(descVPNPeerReceivedBytes, prometheus.GaugeValue,
					p.UsageSummary.ReceivedInKilobytes*bytesPerKilobyte,
					s.NetworkID, netName, "meraki", p.NetworkName))
		}
	}
	// The published window MUST be the floored one, not the poll interval:
	// dashboards divide by it, and reporting 300 while asking for 3600 would
	// overstate throughput twelvefold.
	out = append(out, prometheus.MustNewConstMetric(descVPNUsageWindow, prometheus.GaugeValue, window.Seconds()))
	return out
}

func (e *Exporter) collectSwitchPorts(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	switches, err := e.client.GetSwitchPortStatuses(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	for _, sw := range switches {
		netName := nets[sw.Network.ID]
		for _, p := range sw.Ports {
			out = append(out, stateSet(descSwitchPortStatus, switchPortStates, p.Status,
				sw.Serial, sw.Network.ID, netName, p.PortID)...)
			out = append(out,
				boolMetric(descSwitchPortEnabled, p.Enabled, sw.Serial, sw.Network.ID, netName, p.PortID),
				boolMetric(descSwitchPortPoEAllocated, p.Poe.IsAllocated, sw.Serial, sw.Network.ID, netName, p.PortID))
		}
	}
	return out, nil
}

func (e *Exporter) collectSensors(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
	var out []prometheus.Metric

	devices, err := e.client.GetSensorReadingsLatest(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	for _, d := range devices {
		netName := nets[d.Network.ID]
		labels := []string{d.Serial, d.Network.ID, netName}
		for _, r := range d.Readings {
			switch {
			case r.Temperature != nil:
				out = append(out, prometheus.MustNewConstMetric(descSensorTemperature, prometheus.GaugeValue, r.Temperature.Celsius, labels...))
			case r.Humidity != nil:
				out = append(out, prometheus.MustNewConstMetric(descSensorHumidity, prometheus.GaugeValue, r.Humidity.RelativePercentage, labels...))
			case r.Door != nil:
				out = append(out, boolMetric(descSensorDoorOpen, r.Door.Open, labels...))
			case r.Water != nil:
				out = append(out, boolMetric(descSensorWaterDetected, r.Water.Present, labels...))
			case r.Co2 != nil:
				out = append(out, prometheus.MustNewConstMetric(descSensorCO2, prometheus.GaugeValue, r.Co2.Concentration, labels...))
			case r.Tvoc != nil:
				out = append(out, prometheus.MustNewConstMetric(descSensorTVOC, prometheus.GaugeValue, r.Tvoc.Concentration, labels...))
			case r.Noise != nil:
				out = append(out, prometheus.MustNewConstMetric(descSensorNoise, prometheus.GaugeValue, r.Noise.Ambient.Level, labels...))
			case r.Battery != nil:
				out = append(out, prometheus.MustNewConstMetric(descSensorBattery, prometheus.GaugeValue, r.Battery.Percentage, labels...))
			}
		}
	}
	return out, nil
}

// alertCategoryKey identifies an Assurance alert aggregation bucket.
type alertCategoryKey struct {
	severity     string
	categoryType string
}

// countAlertsBySeverityCategory aggregates open alerts into per-(severity,
// category) counts. Pulled out as a pure function so it is unit-testable
// without an HTTP fixture.
func countAlertsBySeverityCategory(alerts []meraki.AssuranceAlert) map[alertCategoryKey]float64 {
	counts := map[alertCategoryKey]float64{}
	for _, a := range alerts {
		counts[alertCategoryKey{a.Severity, a.CategoryType}]++
	}
	return counts
}

func (e *Exporter) collectAlerts(ctx context.Context, _ map[string]string) ([]prometheus.Metric, error) {
	alerts, err := e.client.GetOrgAssuranceAlerts(ctx, e.orgID)
	if err != nil {
		return nil, err
	}
	counts := countAlertsBySeverityCategory(alerts)
	out := make([]prometheus.Metric, 0, len(counts))
	for k, v := range counts {
		out = append(out, prometheus.MustNewConstMetric(descOrgOpenAlerts, prometheus.GaugeValue, v, k.severity, k.categoryType))
	}
	return out, nil
}
