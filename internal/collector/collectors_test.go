package collector

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

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
	ms := stateSet(descUplinkStatus, uplinkStates, "hibernating", "Q234", "N_1", "HQ", "wan1")
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
