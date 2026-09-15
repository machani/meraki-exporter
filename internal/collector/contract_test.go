package collector

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file checks that every metric and label the dashboards and alert rules
// reference actually exists in the exporter's descriptors.
//
// It exists because that class of breakage is SILENT. Renaming uplink ->
// interface, adding role, adding lat/lng, or disabling a collector all produce
// a panel that reads "No data" and an alert that quietly matches nothing —
// indistinguishable from a healthy system with nothing to report. Unit tests
// cannot see it because they never look at the dashboards; promtool cannot see
// it because it runs against synthetic series it was handed.
//
// Deliberately checked against the DECLARED descriptors rather than a live
// scrape: declarations are the right source of truth for "does this label
// exist", they need no API access, and they cannot drift out of date.

var (
	// Captures the metric name and label list from each NewDesc(...) call.
	// Every descriptor in this package ends its argument list with ", nil)".
	descRe  = regexp.MustCompile(`(?s)NewDesc\(\s*"(meraki_[a-z0-9_]+)"(.*?),\s*nil\)`)
	labelRe = regexp.MustCompile(`\[\]string\{([^}]*)\}`)
	quoted  = regexp.MustCompile(`"([a-z_][a-z0-9_]*)"`)

	metricRe = regexp.MustCompile(`meraki_[a-z0-9_]+`)
	// Label selectors: {foo="bar"} / {foo=~"bar"} / {foo!="bar"}
	selectorRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*[=!]~?\s*"`)
	// Aggregation and join label lists: by (a, b), group_left(a), without (a)
	groupRe = regexp.MustCompile(`(?:by|without|group_left|group_right)\s*\(([^)]*)\)`)
	// Grafana legend references: {{network_name}}
	legendRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
	// Alert annotation references: {{ $labels.network_name }}
	labelRefRe = regexp.MustCompile(`\$labels\.([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// metricsOutsideCollector are declared elsewhere and so are invisible to the
// descriptor scan: the API request counter lives on the client, and build_info
// is registered by main. Both are legitimately used by the health dashboard.
var metricsOutsideCollector = map[string][]string{
	"meraki_exporter_api_requests_total": {"code"},
	"meraki_exporter_build_info":         {"version", "goversion"},
}

// labelsNotOurs are supplied by Prometheus, Grafana or PromQL itself rather
// than by the exporter.
var labelsNotOurs = map[string]bool{
	"job": true, "instance": true, "__name__": true, "le": true, "quantile": true,
	// Added by a Prometheus Operator ServiceMonitor scrape.
	"namespace": true, "service": true, "endpoint": true, "pod": true, "container": true,
	// Grafana template variable, and words that appear inside PromQL functions.
	"network": true, "mode": true,
}

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// declaredMetrics returns metric name -> set of label names, read from the
// NewDesc calls in collector.go.
func declaredMetrics(t *testing.T) map[string]map[string]bool {
	t.Helper()
	src, err := os.ReadFile("collector.go")
	if err != nil {
		t.Fatalf("reading descriptors: %v", err)
	}
	out := map[string]map[string]bool{}
	for _, m := range descRe.FindAllStringSubmatch(string(src), -1) {
		labels := map[string]bool{}
		if ls := labelRe.FindStringSubmatch(m[2]); ls != nil {
			for _, q := range quoted.FindAllStringSubmatch(ls[1], -1) {
				labels[q[1]] = true
			}
		}
		out[m[1]] = labels
	}
	for name, labels := range metricsOutsideCollector {
		set := map[string]bool{}
		for _, l := range labels {
			set[l] = true
		}
		out[name] = set
	}
	if len(out) < 20 {
		t.Fatalf("only %d descriptors found — the NewDesc regex has probably stopped matching", len(out))
	}
	return out
}

// queryText returns every string in the repo that contains PromQL or legend
// references, tagged with where it came from.
func queryText(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}

	files, globErr := filepath.Glob(repoFile(t, "grafana", "*.json"))
	if globErr != nil || len(files) == 0 {
		t.Fatalf("no dashboards found: %v", globErr)
	}
	for _, f := range files {
		//nolint:gosec // test-only read of repo files from a fixed glob, not user input
		raw, readErr := os.ReadFile(f)
		if readErr != nil {
			t.Fatalf("reading %s: %v", f, readErr)
		}
		var dash struct {
			Panels []struct {
				Title   string `json:"title"`
				Targets []struct {
					Expr         string `json:"expr"`
					LegendFormat string `json:"legendFormat"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatalf("%s is not valid JSON: %v", filepath.Base(f), err)
		}
		for _, p := range dash.Panels {
			where := filepath.Base(f) + " / " + p.Title
			for _, tg := range p.Targets {
				if tg.Expr != "" {
					out[where] = append(out[where], tg.Expr)
				}
				if tg.LegendFormat != "" {
					out[where] = append(out[where], tg.LegendFormat)
				}
			}
		}
	}

	// Only the expr: fields and the $labels references in annotations are taken
	// from alerts.yml, never the file as a whole. Scanning the raw text worked
	// until the binary was renamed to meraki-exporter: "journalctl -u
	// meraki-exporter" in an annotation then reads exactly like a metric name,
	// because the binary name collides with the meraki_* metric namespace.
	raw, err := os.ReadFile(repoFile(t, "prometheus", "alerts.yml"))
	if err != nil {
		t.Fatalf("reading alerts.yml: %v", err)
	}
	out["prometheus/alerts.yml"] = alertExpressions(string(raw))
	return out
}

// alertExpressions pulls the expr: values out of a rules file, including block
// scalars (expr: |), plus the $labels references inside annotations. Hand-rolled
// rather than parsed as YAML so that a test is not the only reason this module
// depends on a YAML library.
//
// Only those two things are taken, never the file as a whole. Scanning the raw
// text worked until the binary was renamed to meraki-exporter: "journalctl -u
// meraki-exporter" in a runbook annotation then reads exactly like a metric
// name, because the binary name collides with the meraki_* metric namespace.
func alertExpressions(src string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}

	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, m := range labelRefRe.FindAllStringSubmatch(line, -1) {
			// Re-emitted in selector form so the label extractor picks it up.
			out = append(out, m[1]+`="x"`)
		}
		if !strings.HasPrefix(trimmed, "expr:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "expr:"))
		if rest != "" && rest != "|" && rest != ">" {
			out = append(out, rest)
			continue
		}
		// Block scalar: take every following line indented deeper than expr:.
		indent := len(line) - len(strings.TrimLeft(line, " "))
		var block []string
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.TrimSpace(next) == "" {
				continue
			}
			if len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			block = append(block, next)
			i = j
		}
		// Joined with a space rather than a newline: only metric names and
		// selectors are regex-extracted from it, so the separator is arbitrary.
		out = append(out, strings.Join(block, " "))
	}
	return out
}

func TestDashboardsAndAlertsOnlyUseMetricsThatExist(t *testing.T) {
	declared := declaredMetrics(t)
	for where, texts := range queryText(t) {
		for _, text := range texts {
			for _, name := range metricRe.FindAllString(text, -1) {
				if _, ok := declared[name]; !ok {
					t.Errorf("%s references %s, which no descriptor declares", where, name)
				}
			}
		}
	}
}

func TestDashboardsAndAlertsOnlyUseLabelsThatExist(t *testing.T) {
	declared := declaredMetrics(t)
	known := map[string]bool{}
	for _, labels := range declared {
		for l := range labels {
			known[l] = true
		}
	}
	// The state-set label is appended by stateSet rather than declared in the
	// slice literal, so it never appears in the descriptor source.
	known["status"] = true

	seen := map[string][]string{} // unknown label -> where it was used
	for where, texts := range queryText(t) {
		for _, text := range texts {
			var refs []string
			for _, m := range selectorRe.FindAllStringSubmatch(text, -1) {
				refs = append(refs, m[1])
			}
			for _, m := range legendRe.FindAllStringSubmatch(text, -1) {
				refs = append(refs, m[1])
			}
			for _, m := range groupRe.FindAllStringSubmatch(text, -1) {
				for _, part := range strings.Split(m[1], ",") {
					if p := strings.TrimSpace(part); p != "" {
						refs = append(refs, p)
					}
				}
			}
			for _, l := range refs {
				if !known[l] && !labelsNotOurs[l] {
					seen[l] = append(seen[l], where)
				}
			}
		}
	}
	for label, wheres := range seen {
		sort.Strings(wheres)
		t.Errorf("label %q is used but no metric declares it (in: %s)", label, strings.Join(wheres, "; "))
	}
}
