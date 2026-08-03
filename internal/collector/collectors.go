// Per-domain collector implementations.
package collector

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/machani/meraki-exporter/internal/meraki"
)

const (
	// apiUsageWindow is the lookback for the org-wide API request overview.
	apiUsageWindow      = 24 * time.Hour
	orgClientsMinWindow = 24 * time.Hour
	// msPerSecond converts the API's millisecond latencies to seconds.
	msPerSecond = 1000.0
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
	for _, d := range devices {
		out = append(out, prometheus.MustNewConstMetric(descDeviceInfo, prometheus.GaugeValue, 1,
			d.Serial, d.Name, d.Model, d.MAC, d.Firmware, d.ProductType, d.NetworkID, nets[d.NetworkID]))
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
	for _, dev := range statuses {
		for _, u := range dev.Uplinks {
			out = append(out, stateSet(descUplinkStatus, uplinkStates, u.Status,
				dev.Serial, dev.NetworkID, nets[dev.NetworkID], u.Interface)...)
		}
		// Warm-spare HA state rides along in the same API response — no
		// extra call. NOTE: ha.Enabled is false for standalone appliances,
		// which still report Role "primary" — always pair role queries with
		// meraki_appliance_ha_enabled. A spare keeps its own uplinks active
		// while in standby, so "spare has an active uplink" is NOT failover.
		if ha := dev.HighAvailability; ha != nil {
			out = append(out, boolMetric(descApplianceHAEnabled, ha.Enabled,
				dev.Serial, dev.NetworkID, nets[dev.NetworkID]))
			out = append(out, stateSet(descApplianceHARole, haRoles, ha.Role,
				dev.Serial, dev.NetworkID, nets[dev.NetworkID])...)
		}
	}

	lossLatency, err := e.client.GetUplinksLossAndLatency(ctx, e.orgID)
	if err != nil {
		// Loss/latency is a nice-to-have; keep uplink statuses if it fails.
		e.logger.Warn("failed to fetch uplink loss/latency", "err", err)
		return out, nil
	}
	for _, ll := range lossLatency {
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
				lossSum/float64(lossN), ll.Serial, ll.NetworkID, nets[ll.NetworkID], ll.Uplink, ll.IP))
		}
		if latN > 0 {
			out = append(out, prometheus.MustNewConstMetric(descUplinkLatencySeconds, prometheus.GaugeValue,
				latSum/float64(latN)/msPerSecond, ll.Serial, ll.NetworkID, nets[ll.NetworkID], ll.Uplink, ll.IP))
		}
	}
	return out, nil
}

func (e *Exporter) collectClients(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error) {
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
	ok := 0.0
	if lo.Status == "OK" {
		ok = 1
	}
	out = append(out, prometheus.MustNewConstMetric(descLicenseOK, prometheus.GaugeValue, ok, lo.Status))

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
	return out, nil
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
			enabled := 0.0
			if p.Enabled {
				enabled = 1
			}
			out = append(out, prometheus.MustNewConstMetric(descSwitchPortEnabled, prometheus.GaugeValue, enabled,
				sw.Serial, sw.Network.ID, netName, p.PortID))
			poe := 0.0
			if p.Poe.IsAllocated {
				poe = 1
			}
			out = append(out, prometheus.MustNewConstMetric(descSwitchPortPoEAllocated, prometheus.GaugeValue, poe,
				sw.Serial, sw.Network.ID, netName, p.PortID))
		}
	}
	return out, nil
}

func boolMetric(desc *prometheus.Desc, v bool, labels ...string) prometheus.Metric {
	f := 0.0
	if v {
		f = 1
	}
	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, f, labels...)
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
