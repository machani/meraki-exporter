# Grafana dashboards

Import via Grafana: Dashboards > New > Import > Upload JSON file, then select
your Prometheus data source in the `Data source` variable. All dashboards
except the overview also have a multi-select `Network` filter.

- `meraki-overview.json` — org-wide summary: device status stats
  (online/alerting/offline), org client count, license days remaining,
  exporter poll freshness, uplink loss/latency, clients per network, wireless
  channel utilization, a "devices not online" table, collector durations, and
  (opt-in collectors) open critical alerts, sensors reporting water, and
  sensor doors open.
- `meraki-device-health.json` — device fleet: status stats + availability %,
  devices by product type, offline/alerting/dormant counts over time,
  firmware distribution, a full device inventory table, and (opt-in
  `switchports` collector) enabled-port-down count, active PoE port count,
  and a table of enabled-but-disconnected ports.
- `meraki-uplink-wan.json` — WAN quality: uplink state stats (active /
  standby / down), avg loss and latency, per-uplink loss/latency time series,
  top-10 worst uplinks, a current uplink status table, and (opt-in `vpn`
  collector) VPN peer reachable/unreachable counts and a current peer status
  table, plus warm-spare HA panels (failover stat + role
  table) from the `uplinks` collector, no opt-in needed.
- `meraki-wireless.json` — Wi-Fi: AP counts and online state, channel
  utilization by band, WiFi vs non-WiFi (interference) split, top-10 busiest
  APs (joined with device names), and clients per network.

Panels for `vpn`, `switchports`, `sensors`, and `alerts` render as "No data"
until the corresponding collector is enabled via `COLLECTORS_ENABLED` (all
four are opt-in).
