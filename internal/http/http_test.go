package http

import (
	"testing"
	"time"

	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	"html/template"
	"io"
	"path"
)

type sortable string

func (s sortable) GetSortKey() string { return string(s) }

func TestSortBySortKey_UnsortableItemsGoLastInOriginalOrder(t *testing.T) {
	items := []any{1, sortable("b"), "x", sortable("a")}

	sortBySortKey(items)

	if items[0] != sortable("a") || items[1] != sortable("b") || items[2] != 1 || items[3] != "x" {
		t.Fatalf("expected sortable items first, then the rest in original order, got %v", items)
	}
}

func TestRelativeTime(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero", time.Time{}, "never"},
		{"recent", time.Now().Add(-5 * time.Second), "< 30s"},
		{"minutes", time.Now().Add(-5*time.Minute - 10*time.Second), "5m"},
		{"hours", time.Now().Add(-3*time.Hour - time.Minute), "3h"},
		{"days", time.Now().Add(-72*time.Hour - time.Minute), "3d"},
	}

	for _, c := range cases {
		if got := relativeTime(c.in); got != c.want {
			t.Errorf("%s: relativeTime = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFormatClock_ZeroIsEmpty(t *testing.T) {
	if got := formatClock(time.Time{}); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

// TestTemplates_ParseAndExecute catches a template referencing a function or field that doesn't
// exist, which otherwise only surfaces when the page is first rendered.
func TestTemplates_ParseAndExecute(t *testing.T) {
	cases := []struct {
		info   string
		path   string
		funcs  template.FuncMap
		entity any
	}{
		{"wireless", wirelessThermometerTemplate.Paths[0], wirelessThermometerTemplate.FuncMap, &thermometer.WirelessThermometer{}},
		{"thermostat", thermostatTemplate.Paths[0], thermostatTemplate.FuncMap, &thermometer.Thermostat{}},
	}

	for _, c := range cases {
		name := path.Base(c.path)
		tmpl, err := template.New(name).Funcs(c.funcs).ParseFS(contentFS, "content/"+c.path)
		if err != nil {
			t.Fatalf("%s: parse: %v", c.info, err)
		}

		if err := tmpl.ExecuteTemplate(io.Discard, name, c.entity); err != nil {
			t.Fatalf("%s: execute (no data): %v", c.info, err)
		}

		// And the with-data branch, which includes the extremes section.
		switch e := c.entity.(type) {
		case *thermometer.WirelessThermometer:
			e.SensorData.HasData = true
		case *thermometer.Thermostat:
			e.SensorData.HasData = true
		}

		if err := tmpl.ExecuteTemplate(io.Discard, name, c.entity); err != nil {
			t.Fatalf("%s: execute (with data): %v", c.info, err)
		}
	}
}
