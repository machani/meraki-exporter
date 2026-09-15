package meraki

import (
	"encoding/json"
	"time"
)

// ---- API response types ----

type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Network struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Device struct {
	Serial      string `json:"serial"`
	Name        string `json:"name"`
	Model       string `json:"model"`
	MAC         string `json:"mac"`
	NetworkID   string `json:"networkId"`
	Firmware    string `json:"firmware"`
	ProductType string `json:"productType"`

	// Lat/Lng are json.Number rather than float64 so an ABSENT coordinate
	// stays empty instead of becoming 0 — a device with no position would
	// otherwise be placed at 0,0 in the Atlantic rather than left off the map.
	// json.Number also avoids reformatting the value the API sent.
	//
	// The API reference documents these as JSON numbers. If Meraki ever sends
	// them quoted, decoding fails and the devices collector reports failure
	// loudly (see MerakiCollectorFailing) rather than degrading quietly.
	Lat     json.Number `json:"lat"`
	Lng     json.Number `json:"lng"`
	Address string      `json:"address"`
}

type DeviceAvailability struct {
	Serial      string `json:"serial"`
	Name        string `json:"name"`
	Status      string `json:"status"` // online, alerting, offline, dormant
	ProductType string `json:"productType"`
	Network     struct {
		ID string `json:"id"`
	} `json:"network"`
}

type ApplianceUplinkStatus struct {
	Serial    string `json:"serial"`
	NetworkID string `json:"networkId"`
	// HighAvailability is present for MX appliances; nil when the API omits
	// it. Role is the appliance's current warm-spare role.
	HighAvailability *struct {
		Enabled bool   `json:"enabled"`
		Role    string `json:"role"` // primary, spare; standalone units send "primary" with enabled=false
	} `json:"highAvailability"`
	Uplinks []struct {
		Interface string `json:"interface"`
		Status    string `json:"status"` // active, ready, failed, not connected
		IP        string `json:"ip"`
	} `json:"uplinks"`
}

// UplinkUsageByNetwork is one network's uplink usage over the requested
// timespan.
//
// Sent and Received are already **bytes** (confirmed against the published API
// reference 2026-08-13), so no unit conversion is needed — unusual for this
// family of endpoints, several of which report kilobytes.
//
// The shape does force one thing on the collector: a row carries a serial, so
// an HA pair reports the same network+interface twice. Consumers must
// aggregate rather than assume the pair is unique, or the registry rejects the
// duplicate series.
type UplinkUsageByNetwork struct {
	NetworkID string `json:"networkId"`
	Name      string `json:"name"`
	ByUplink  []struct {
		Serial    string  `json:"serial"`
		Interface string  `json:"interface"` // wan1, wan2, cellular
		Sent      float64 `json:"sent"`      // bytes over the timespan
		Received  float64 `json:"received"`  // bytes over the timespan
	} `json:"byUplink"`
}

type UplinkLossLatency struct {
	Serial     string `json:"serial"`
	NetworkID  string `json:"networkId"`
	Uplink     string `json:"uplink"`
	IP         string `json:"ip"`
	TimeSeries []struct {
		Ts          time.Time `json:"ts"`
		LossPercent *float64  `json:"lossPercent"`
		LatencyMs   *float64  `json:"latencyMs"`
	} `json:"timeSeries"`
}

type OrgClientsOverview struct {
	Counts struct {
		Total float64 `json:"total"`
	} `json:"counts"`
}

type NetworkClientsOverview struct {
	Counts struct {
		Total          float64 `json:"total"`
		WithHeavyUsage float64 `json:"withHeavyUsage"`
	} `json:"counts"`
}

type ChannelUtilization struct {
	Serial  string `json:"serial"`
	Network struct {
		ID string `json:"id"`
	} `json:"network"`
	ByBand []struct {
		Band string `json:"band"`
		WiFi struct {
			Percentage float64 `json:"percentage"`
		} `json:"wifi"`
		NonWiFi struct {
			Percentage float64 `json:"percentage"`
		} `json:"nonWifi"`
		Total struct {
			Percentage float64 `json:"percentage"`
		} `json:"total"`
	} `json:"byBand"`
}

type LicenseOverview struct {
	Status               string             `json:"status"`
	ExpirationDate       string             `json:"expirationDate"` // e.g. "Mar 16, 2027 UTC" (co-term orgs)
	LicensedDeviceCounts map[string]float64 `json:"licensedDeviceCounts"`
}

type APIRequestsOverview struct {
	ResponseCodeCounts map[string]float64 `json:"responseCodeCounts"`
}

type ApplianceVPNStatus struct {
	NetworkID      string `json:"networkId"`
	NetworkName    string `json:"networkName"`
	MerakiVPNPeers []struct {
		NetworkID    string `json:"networkId"`
		NetworkName  string `json:"networkName"`
		Reachability string `json:"reachability"` // reachable, unreachable
	} `json:"merakiVpnPeers"`
	ThirdPartyVPNPeers []struct {
		Name         string `json:"name"`
		Reachability string `json:"reachability"` // reachable, unreachable
	} `json:"thirdPartyVpnPeers"`
}

// VPNStats is one network's site-to-site VPN statistics over the requested
// timespan, from /organizations/{orgId}/appliance/vpn/stats.
//
// Two traps in this shape:
//
//   - the usage figures are **kilobytes**, unlike the appliance uplink usage
//     endpoint which reports bytes. Do not carry an assumption between the
//     two; check each endpoint.
//   - they arrive as JSON **strings** ("29", not 29), hence the ,string tags.
//     Without them the response does not unmarshal at all.
//
// The probe saw only merakiVpnPeers populated; whether third-party peers ever
// appear here is unconfirmed, so the collector labels what it finds as meraki
// peers and does not invent a third-party branch.
type VPNStats struct {
	NetworkID      string `json:"networkId"`
	NetworkName    string `json:"networkName"`
	MerakiVPNPeers []struct {
		NetworkID    string `json:"networkId"`
		NetworkName  string `json:"networkName"`
		UsageSummary *struct {
			SentInKilobytes     float64 `json:"sentInKilobytes,string"`
			ReceivedInKilobytes float64 `json:"receivedInKilobytes,string"`
		} `json:"usageSummary"`
	} `json:"merakiVpnPeers"`
}

type SwitchPortsBySwitch struct {
	Serial  string `json:"serial"`
	Network struct {
		ID string `json:"id"`
	} `json:"network"`
	Ports []struct {
		PortID  string `json:"portId"`
		Enabled bool   `json:"enabled"`
		Status  string `json:"status"` // Connected, Disconnected, Disabled
		Poe     struct {
			IsAllocated bool `json:"isAllocated"`
		} `json:"poe"`
	} `json:"ports"`
}

// SensorReadingsLatest holds the latest reading of each metric type an MT
// sensor reports. Each metric-value field is a pointer since the API only
// populates the field matching the reading's Metric discriminator.
type SensorReadingsLatest struct {
	Serial  string `json:"serial"`
	Network struct {
		ID string `json:"id"`
	} `json:"network"`
	Readings []struct {
		Metric      string `json:"metric"` // temperature, humidity, door, water, co2, tvoc, noise, battery, ...
		Temperature *struct {
			Celsius float64 `json:"celsius"`
		} `json:"temperature"`
		Humidity *struct {
			RelativePercentage float64 `json:"relativePercentage"`
		} `json:"humidity"`
		Door *struct {
			Open bool `json:"open"`
		} `json:"door"`
		Water *struct {
			Present bool `json:"present"`
		} `json:"water"`
		Co2 *struct {
			Concentration float64 `json:"concentration"`
		} `json:"co2"`
		Tvoc *struct {
			Concentration float64 `json:"concentration"`
		} `json:"tvoc"`
		Noise *struct {
			Ambient struct {
				Level float64 `json:"level"`
			} `json:"ambient"`
		} `json:"noise"`
		Battery *struct {
			Percentage float64 `json:"percentage"`
		} `json:"battery"`
	} `json:"readings"`
}

// AssuranceAlert is a single organization-wide Assurance alert. Only the
// fields needed for severity/category aggregation are kept — GetOrgAssuranceAlerts
// already filters to open (active, undismissed, unresolved) alerts server-side.
type AssuranceAlert struct {
	CategoryType string `json:"categoryType"`
	Severity     string `json:"severity"` // critical, warning, informational
}
