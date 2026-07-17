// Package collector implements the exporter core: a background poller
// fetches Meraki data on a fixed interval and caches Prometheus
// const-metrics; scrapes of /metrics are served instantly from the cache.
// This decouples Prometheus scrape latency from slow, rate-limited
// Meraki API calls.
package collector

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/machani/meraki-exporter/internal/config"
	"github.com/machani/meraki-exporter/internal/meraki"
)

// ErrMultipleOrgs is returned when no org ID is configured and the API key
// can see more than one organization.
var ErrMultipleOrgs = errors.New("multiple organizations visible; MERAKI_ORG_ID required")

// ---- Metric descriptors ----

var (
	// Known state sets (emitted as 0/1 series per state).
	deviceStates = []string{"online", "alerting", "offline", "dormant"}
	uplinkStates = []string{"active", "ready", "connecting", "not connected", "failed"}
	// vpnPeerStates and switchPortStates are not exhaustively documented by
	// Meraki; stateSet's unknown-state fallback covers any value not listed
	// here without a code change.
	vpnPeerStates    = []string{"reachable", "unreachable"}
	switchPortStates = []string{"Connected", "Disconnected", "Disabled"}
	haRoles          = []string{"primary", "spare"}

	descDeviceInfo = prometheus.NewDesc("meraki_device_info",
		"Static device metadata; value is always 1.",
		[]string{"serial", "name", "model", "mac", "firmware", "product_type", "network_id", "network_name"}, nil)
	descDeviceStatus = prometheus.NewDesc("meraki_device_status",
		"Device status state set: 1 for the current status, 0 otherwise.",
		[]string{"serial", "name", "network_id", "network_name", "status"}, nil)

	descUplinkStatus = prometheus.NewDesc("meraki_uplink_status",
		"Appliance uplink status state set: 1 for the current status, 0 otherwise.",
		[]string{"serial", "network_id", "network_name", "interface", "status"}, nil)
	descUplinkLossPercent = prometheus.NewDesc("meraki_uplink_loss_percent",
		"Uplink packet loss percentage, averaged over the last 5 minutes.",
		[]string{"serial", "network_id", "network_name", "uplink", "ip"}, nil)
	descUplinkLatencySeconds = prometheus.NewDesc("meraki_uplink_latency_seconds",
		"Uplink latency in seconds, averaged over the last 5 minutes.",
		[]string{"serial", "network_id", "network_name", "uplink", "ip"}, nil)
	descApplianceHAEnabled = prometheus.NewDesc("meraki_appliance_ha_enabled",
		"1 if warm spare high availability is enabled for the appliance, 0 otherwise. Only emitted for appliances reporting HA state.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descApplianceHARole = prometheus.NewDesc("meraki_appliance_ha_role",
		"Appliance warm-spare role state set: 1 for the current role, 0 otherwise. A spare with an active uplink means failover is in effect.",
		[]string{"serial", "network_id", "network_name", "role"}, nil)

	descOrgClients = prometheus.NewDesc("meraki_org_clients",
		"Total clients seen org-wide during the configured timespan.",
		nil, nil)
	descNetworkClients = prometheus.NewDesc("meraki_network_clients",
		"Clients seen per network during the configured timespan.",
		[]string{"network_id", "network_name"}, nil)
	descNetworkClientsHeavy = prometheus.NewDesc("meraki_network_clients_heavy_usage",
		"Clients with heavy usage per network during the configured timespan.",
		[]string{"network_id", "network_name"}, nil)

	descChannelUtilization = prometheus.NewDesc("meraki_wireless_channel_utilization_percent",
		"Wireless channel utilization percentage per AP and band.",
		[]string{"serial", "network_id", "network_name", "band", "kind"}, nil)

	descLicenseOK = prometheus.NewDesc("meraki_license_status_ok",
		"1 if the organization license status is OK, 0 otherwise.",
		[]string{"status"}, nil)
	descLicenseExpiration = prometheus.NewDesc("meraki_license_expiration_timestamp_seconds",
		"Unix timestamp of the organization license expiration (co-term licensing).",
		nil, nil)
	descLicensedDevices = prometheus.NewDesc("meraki_licensed_devices",
		"Licensed device counts by device type.",
		[]string{"device_type"}, nil)

	descOrgAPIResponses = prometheus.NewDesc("meraki_org_api_response_codes",
		"Org-wide Meraki API responses by status code over the last 24 hours (all API consumers).",
		[]string{"code"}, nil)

	descVPNPeerStatus = prometheus.NewDesc("meraki_vpn_peer_status",
		"Site-to-site VPN peer reachability state set: 1 for the current state, 0 otherwise.",
		[]string{"network_id", "network_name", "peer_type", "peer_name", "status"}, nil)

	descSwitchPortStatus = prometheus.NewDesc("meraki_switch_port_status",
		"Switch port status state set: 1 for the current status, 0 otherwise.",
		[]string{"serial", "network_id", "network_name", "port_id", "status"}, nil)
	descSwitchPortEnabled = prometheus.NewDesc("meraki_switch_port_enabled",
		"1 if the switch port is administratively enabled, 0 otherwise.",
		[]string{"serial", "network_id", "network_name", "port_id"}, nil)
	descSwitchPortPoEAllocated = prometheus.NewDesc("meraki_switch_port_poe_allocated",
		"1 if PoE power is allocated to the switch port, 0 otherwise.",
		[]string{"serial", "network_id", "network_name", "port_id"}, nil)

	descSensorTemperature = prometheus.NewDesc("meraki_sensor_temperature_celsius",
		"Latest sensor temperature reading in Celsius.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorHumidity = prometheus.NewDesc("meraki_sensor_humidity_percent",
		"Latest sensor relative humidity reading.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorDoorOpen = prometheus.NewDesc("meraki_sensor_door_open",
		"1 if the sensor's monitored door is open, 0 otherwise.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorWaterDetected = prometheus.NewDesc("meraki_sensor_water_detected",
		"1 if the sensor detects water, 0 otherwise.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorCO2 = prometheus.NewDesc("meraki_sensor_co2_ppm",
		"Latest sensor CO2 reading in parts per million.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorTVOC = prometheus.NewDesc("meraki_sensor_tvoc_micrograms_per_cubic_meter",
		"Latest sensor total volatile organic compound reading.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorNoise = prometheus.NewDesc("meraki_sensor_noise_db",
		"Latest sensor ambient noise reading in decibels.",
		[]string{"serial", "network_id", "network_name"}, nil)
	descSensorBattery = prometheus.NewDesc("meraki_sensor_battery_percent",
		"Latest sensor remaining battery percentage.",
		[]string{"serial", "network_id", "network_name"}, nil)

	descOrgOpenAlerts = prometheus.NewDesc("meraki_org_open_alerts",
		"Count of currently open (active, undismissed, unresolved) Meraki Assurance alerts by severity and category.",
		[]string{"severity", "category_type"}, nil)

	descCollectorSuccess = prometheus.NewDesc("meraki_exporter_collector_success",
		"1 if the collector's last run succeeded, 0 otherwise.",
		[]string{"collector"}, nil)
	descCollectorDuration = prometheus.NewDesc("meraki_exporter_collector_duration_seconds",
		"Duration of the collector's last run.",
		[]string{"collector"}, nil)
	descLastPoll = prometheus.NewDesc("meraki_exporter_last_poll_timestamp_seconds",
		"Unix timestamp of the last completed poll cycle, per poll group.",
		[]string{"group"}, nil)
	descOrgInfo = prometheus.NewDesc("meraki_org_info",
		"Organization being monitored; value is always 1.",
		[]string{"org_id", "org_name"}, nil)
)

// Exporter polls the Meraki API in the background and serves cached metrics.
// Each Exporter instance owns one poll group (e.g. "fast" or "slow") with its
// own collector subset and interval.
type Exporter struct {
	cfg      *config.Config
	client   *meraki.Client
	logger   *slog.Logger
	group    string
	names    []string // enabled collectors in this group, in AllCollectors order
	interval time.Duration
	orgID    string
	orgName  string

	mu     sync.RWMutex
	cache  []prometheus.Metric
	primed bool
}

// New builds an Exporter for one poll group.
func New(cfg *config.Config, client *meraki.Client, logger *slog.Logger, group string, names []string, interval time.Duration) *Exporter {
	return &Exporter{
		cfg:      cfg,
		client:   client,
		logger:   logger.With("group", group),
		group:    group,
		names:    names,
		interval: interval,
		orgID:    cfg.OrgID,
	}
}

// Describe is intentionally empty: this is an "unchecked" collector, which
// is the recommended pattern for exporters emitting dynamic label sets.
func (*Exporter) Describe(chan<- *prometheus.Desc) {}

// Collect replays the cached metrics from the last poll.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, m := range e.cache {
		ch <- m
	}
}

// Ready reports whether at least one poll has completed (for /healthz).
func (e *Exporter) Ready() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.primed
}

// resolveOrg discovers the organization if none was configured.
func (e *Exporter) resolveOrg(ctx context.Context) error {
	orgs, err := e.client.GetOrganizations(ctx)
	if err != nil {
		return err
	}
	if e.orgID == "" {
		if len(orgs) != 1 {
			names := make([]string, 0, len(orgs))
			for _, o := range orgs {
				names = append(names, o.Name+" ("+o.ID+")")
			}
			e.logger.Error("API key has access to multiple organizations; set MERAKI_ORG_ID", "orgs", names)
			return ErrMultipleOrgs
		}
		e.orgID = orgs[0].ID
		e.orgName = orgs[0].Name
		e.logger.Info("auto-discovered organization", "id", e.orgID, "name", e.orgName)
		return nil
	}
	for _, o := range orgs {
		if o.ID == e.orgID {
			e.orgName = o.Name
			return nil
		}
	}
	e.logger.Warn("configured org ID not visible to this API key", "org_id", e.orgID)
	return nil
}

// Run starts the poll loop and blocks until ctx is cancelled.
func (e *Exporter) Run(ctx context.Context) {
	e.poll(ctx)
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.poll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

type collectorFn func(ctx context.Context, nets map[string]string) ([]prometheus.Metric, error)

// needsNetworks reports whether any collector in this group uses the
// networks id->name map for labels.
func (e *Exporter) needsNetworks() bool {
	for _, n := range e.names {
		switch n {
		case "devices", "uplinks", "clients", "wireless", "vpn", "switchports", "sensors":
			return true
		}
	}
	return false
}

// poll runs one full collection cycle and swaps the metric cache.
func (e *Exporter) poll(ctx context.Context) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.interval)
	defer cancel()

	var out []prometheus.Metric

	if len(e.names) > 0 {
		var resolveErr error
		if e.orgID == "" || e.orgName == "" {
			if resolveErr = e.resolveOrg(ctx); resolveErr != nil {
				e.logger.Error("failed to resolve organization", "err", resolveErr)
			}
		}

		if resolveErr != nil {
			// Org resolution failed (e.g. bad API key on the very first
			// poll). Still publish per-collector failure metrics so the
			// success/staleness alerts can fire; data metrics stay absent,
			// exactly as they would for any failed collector.
			for _, name := range e.names {
				out = append(out,
					prometheus.MustNewConstMetric(descCollectorSuccess, prometheus.GaugeValue, 0, name),
					prometheus.MustNewConstMetric(descCollectorDuration, prometheus.GaugeValue, 0, name),
				)
			}
		} else {
			// meraki_org_info is emitted by the fast group only, so the
			// combined /metrics endpoint never sees duplicate series.
			if e.group == "fast" {
				out = append(out, prometheus.MustNewConstMetric(descOrgInfo, prometheus.GaugeValue, 1, e.orgID, e.orgName))
			}

			// Networks map (id -> name) is shared by several collectors.
			nets := map[string]string{}
			if e.needsNetworks() {
				if networks, err := e.client.GetNetworks(ctx, e.orgID); err != nil {
					e.logger.Warn("failed to list networks; network_name labels will be empty", "err", err)
				} else {
					for _, n := range networks {
						nets[n.ID] = n.Name
					}
				}
			}

			collectors := map[string]collectorFn{
				"devices":     e.collectDevices,
				"uplinks":     e.collectUplinks,
				"clients":     e.collectClients,
				"wireless":    e.collectWireless,
				"licenses":    e.collectLicenses,
				"apiusage":    e.collectAPIUsage,
				"vpn":         e.collectVPN,
				"switchports": e.collectSwitchPorts,
				"sensors":     e.collectSensors,
				"alerts":      e.collectAlerts,
			}

			for _, name := range e.names {
				cStart := time.Now()
				metrics, err := collectors[name](ctx, nets)
				success := 1.0
				if err != nil {
					success = 0
					e.logger.Error("collector failed", "collector", name, "err", err)
				} else {
					out = append(out, metrics...)
				}
				out = append(out,
					prometheus.MustNewConstMetric(descCollectorSuccess, prometheus.GaugeValue, success, name),
					prometheus.MustNewConstMetric(descCollectorDuration, prometheus.GaugeValue, time.Since(cStart).Seconds(), name),
				)
			}
		}
	}

	out = append(out, prometheus.MustNewConstMetric(descLastPoll, prometheus.GaugeValue, float64(time.Now().Unix()), e.group))

	e.mu.Lock()
	e.cache = out
	e.primed = true
	e.mu.Unlock()
	e.logger.Debug("poll cycle complete", "duration", time.Since(start), "metrics", len(out))
}
