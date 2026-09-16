# Grafana dashboards

Import via Grafana: Dashboards > New > Import > Upload JSON file, then select
your Prometheus data source in the `Data source` variable.

All dashboards except `meraki-exporter-health.json` have a multi-select
`Network` filter. The health dashboard deliberately has none: every metric on
it describes the exporter or the organization, and none carries a network
label. Individual panels elsewhere that cannot follow the filter say so in
their description rather than appearing broken.

- `meraki-overview.json` — org-wide summary: device status stats
  (online/alerting/offline), org client count, license days remaining and
  status, licensed device count, fast-group poll freshness, failing-collector
  count, uplink loss/latency, uplink throughput per site, clients per network,
  wireless channel utilization, a "devices not online" table, and (opt-in
  collectors) open critical alerts, sensors reporting water, and sensor doors
  open. Deliberately carries only two exporter-health signals; the rest live on
  the health dashboard rather than being repeated here.
- `meraki-exporter-health.json` — is the exporter itself working: scrape
  target up, failing and running collector counts, poll age per group,
  collector success and duration over time, a current collector status table,
  the exporter's own API requests by status code, and org-wide API response
  codes across all consumers (needs the `apiusage` collector).
- `meraki-device-health.json` — device fleet: status stats + availability %,
  devices by product type, offline/alerting/dormant counts over time, a table
  naming the devices that are not online, a Geomap of device locations,
  firmware distribution, a full device inventory table, and (opt-in
  `switchports` collector) a count and table of ports that were in use within
  the last 7 days and are down now.
- `meraki-uplink-wan.json` — WAN quality: uplink state stats (active /
  standby / down), avg loss and latency, per-uplink loss/latency time series,
  sent/received traffic per site uplink, top-10 worst uplinks, a current uplink
  status table, plus warm-spare HA panels (failover stat + role table) from the
  `uplinks` collector, no opt-in needed. VPN moved to its own dashboard on
  2026-08-14, when this one had grown to cover three unrelated concerns.
- `meraki-vpn.json` — site-to-site VPN (opt-in `vpn` collector): peer
  reachable/unreachable counts, total tunnel traffic sent/received, per-tunnel
  traffic graphs, a per-site summary combining each site's tunnels, and a peer
  status table. Note the reachability panels cover
  third-party peers while the traffic panels cannot — Meraki's VPN stats
  endpoint returns Meraki peers only.
- `meraki-wireless.json` — Wi-Fi: AP counts and online state, channel
  utilization by band, WiFi vs non-WiFi (interference) split, top-10 busiest
  APs (joined with device names), clients per network, and heavy-usage clients
  per network.

Panels for `vpn`, `switchports`, `sensors`, and `alerts` render as "No data"
until the corresponding collector is enabled via `COLLECTORS_ENABLED` (all
four are opt-in).

## Clicking a number to see the rows behind it

The tiles that state a count are clickable. Alerting, Offline and Dormant on
the device-health dashboard, and Devices Alerting / Devices Offline on the
overview, each open the table naming those devices, narrowed to the status you
clicked. The Devices by Product Type donut opens the Device Inventory filtered
to the slice you clicked.

Every link carries the current time range and the current Network selection
through to the target, along with the status or product type you clicked, so a
drill-down never silently widens the scope you were looking at.

Three notes for whoever edits these next:

- The links open a panel on the same dashboard via `viewPanel=<id>`, so **the
  panel ids in those URLs matter**. Renumbering a panel breaks its link quietly:
  Grafana opens the dashboard without the full-screen view rather than erroring.
- The `Status` variable exists only so a tile has something to hand to the table.
  It filters the not-online table and nothing else - not the tiles that feed it,
  and not the over-time graph, both of which must keep counting every status.
- The `Product type` variable on device-health exists only to give the donut
  something to drill into. It filters the Device Inventory table and nothing
  else; the Geomap runs the same query and is deliberately left unfiltered.

Worth knowing that these already work, without any configuration, and are often
quicker: drag across a time series to zoom into that window, click a legend
entry to isolate a series, hover for a tooltip covering every series at that
instant, and use the panel menu's View and Explore entries.

Three things worth knowing when a panel looks wrong rather than empty:

- **Traffic metrics are windowed sums, not counters.** Uplink and VPN byte
  metrics report bytes over the poll window, so the panels divide by the
  matching `*_usage_window_seconds` metric to get throughput. `rate()` on them
  produces nonsense.
- **A disabled collector is invisible.** It emits no series at all, including
  no failure metric, so nothing turns red — the panels simply read "No data".
  The `Collectors Running` tile on the health dashboard is the quickest way to
  notice; compare it against `COLLECTORS_ENABLED`.
- **The map only shows devices placed on the Meraki dashboard map.** Coordinates
  come from Meraki, not from the exporter; an unplaced device has empty lat/lng
  and is absent from the map while still appearing everywhere else. If the map
  looks sparse, the fix is in the Meraki dashboard.

`internal/collector/contract_test.go` checks that every metric and label these
dashboards reference actually exists in the exporter, so a renamed metric fails
CI rather than silently producing a "No data" panel.
