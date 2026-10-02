package http

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/avanha/pmaas-plugin-environment/internal/common"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	spi "github.com/avanha/pmaas-spi"
	environmental "github.com/avanha/pmaas-spi/environment"
)

//go:embed content/static content/templates
var contentFS embed.FS

var wirelessThermometerTemplate = spi.TemplateInfo{
	Name: "environment_wireless_thermometer",
	FuncMap: template.FuncMap{
		"CelsiusToFahrenheit": celsiusToFahrenheit,
		"RelativeTime":        relativeTime,
		"FormatClock":         formatClock,
		"IsStale":             isStale,
		"IsLowBattery":        isLowBattery,
	},
	Paths:  []string{"templates/wireless_thermometer.htmlt"},
	Styles: []string{"css/wireless_thermometer.css"},
}

var thermostatTemplate = spi.TemplateInfo{
	Name: "environment_thermostat",
	FuncMap: template.FuncMap{
		"CelsiusToFahrenheit":    celsiusToFahrenheit,
		"RelativeTime":           relativeTime,
		"FormatClock":            formatClock,
		"IsOffline":              isOffline,
		"IsOnline":               isOnline,
		"FormatConnectivityTime": formatConnectivityTime,
	},
	Paths:  []string{"templates/thermostat.htmlt"},
	Styles: []string{"css/thermostat.css"},
}

var listRenderOptions = spi.RenderListOptions{
	Title: "Environmental Devices",
}

// Handler owns this plugin's HTTP surface: the entity list route, template/static content, and
// the entity renderers registered for it. It reaches back into the plugin only through
// entityStore - see common.EntityStore.
type Handler struct {
	container   spi.IPMAASContainer
	entityStore common.EntityStore
}

func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Init(container spi.IPMAASContainer, entityStore common.EntityStore) {
	h.container = container
	h.entityStore = entityStore

	container.ProvideContentFS(&contentFS, "content")
	container.EnableStaticContent("static")
	container.AddRoute("", h.handleHttpListRequest)
	container.RegisterEntityRenderer(
		reflect.TypeFor[thermometer.WirelessThermometer](), h.wirelessThermometerRendererFactory)
	container.RegisterEntityRenderer(
		reflect.TypeFor[thermometer.Thermostat](), h.thermostatRendererFactory)
}

func (h *Handler) handleHttpListRequest(w http.ResponseWriter, r *http.Request) {
	// entityStore.GetEntities crosses onto the plugin's own goroutine - HTTP requests come in on
	// arbitrary goroutines, so this is how we get all states atomically.
	items, err := h.entityStore.GetEntities()
	if err != nil {
		fmt.Printf("environment.http handleHttpListRequest: Error retrieving entities: %s\n", err)
		items = nil
	}

	// Second, pass pointers to the entities rather than copies, to avoid copying them again while
	// rendering. Entities arrive as the concrete value snapshots from GetState; anything else is
	// something the renderers registered in Init couldn't render anyway, so it's skipped, not
	// passed on as a nil placeholder.
	itemRefs := make([]any, 0, len(items))

	for _, item := range items {
		switch typedItem := item.(type) {
		case thermometer.WirelessThermometer:
			itemRefs = append(itemRefs, &typedItem)
		case thermometer.Thermostat:
			itemRefs = append(itemRefs, &typedItem)
		default:
			fmt.Printf("environment.http handleHttpListRequest: skipping unexpected entity type %T\n", item)
		}
	}

	// Third, sort the entities using their sort keys
	sortBySortKey(itemRefs)

	// Lastly, render the sorted entity list.  The render plugin will choose a matching rendered based on
	// the entity type.
	h.container.RenderList(w, r, listRenderOptions, itemRefs)
}

// sortBySortKey orders items by their common.ISortable key. Items that aren't sortable go after
// all sortable ones, otherwise keeping their original order, so the comparator is a consistent
// strict weak ordering.
func sortBySortKey(items []any) {
	sort.SliceStable(items, func(i int, j int) bool {
		left, leftOk := items[i].(common.ISortable)
		right, rightOk := items[j].(common.ISortable)

		if !leftOk || !rightOk {
			return leftOk && !rightOk
		}

		return left.GetSortKey() < right.GetSortKey()
	})
}

func (h *Handler) wirelessThermometerRendererFactory() (spi.EntityRenderer, error) {
	// Load the template
	t, err := h.container.GetTemplate(&wirelessThermometerTemplate)

	if err != nil {
		return spi.EntityRenderer{}, fmt.Errorf("unable to load wireless_thermometer template: %v", err)
	}

	// Declare a function that casts the entity to the expected type and evaluates it via the template loaded above
	renderer := func(w io.Writer, entity any) error {
		wt, ok := entity.(*thermometer.WirelessThermometer)

		if !ok {
			return errors.New("item is not an instance of *WirelessThermometer")
		}

		err := t.Instance.Execute(w, wt)

		if err != nil {
			return fmt.Errorf("unable to execute wireless_thermometer template: %w", err)
		}

		return nil
	}

	return spi.EntityRenderer{StreamingRenderFunc: renderer, Styles: t.Styles, Scripts: t.Scripts}, nil
}

func (h *Handler) thermostatRendererFactory() (spi.EntityRenderer, error) {
	// Load the template
	compiledTemplate, err := h.container.GetTemplate(&thermostatTemplate)

	if err != nil {
		return spi.EntityRenderer{}, fmt.Errorf("unable to load thermostat template: %v", err)
	}

	// Declare a function that casts the entity to the expected type and evaluates it via the template loaded above
	renderer := func(w io.Writer, entity any) error {
		thermostatEntity, ok := entity.(*thermometer.Thermostat)

		if !ok {
			return errors.New("item is not an instance of *Thermostat")
		}

		err := compiledTemplate.Instance.Execute(w, thermostatEntity)

		if err != nil {
			return fmt.Errorf("unable to execute thermostat template: %w", err)
		}

		return nil
	}

	return spi.EntityRenderer{StreamingRenderFunc: renderer, Styles: compiledTemplate.Styles, Scripts: compiledTemplate.Scripts}, nil
}

func celsiusToFahrenheit(celsiusValue float32) float32 {
	return celsiusValue*float32(9)/float32(5) + float32(32)
}

func isOffline(connectivity environmental.Connectivity) bool {
	return connectivity == environmental.ConnectivityOffline
}

func isOnline(connectivity environmental.Connectivity) bool {
	return connectivity == environmental.ConnectivityOnline
}

// formatConnectivityTime formats an OfflineSince/OnlineSince timestamp for display, distinguishing a
// state that's genuinely never been observed (zero time.Time) from a real timestamp — text/template has
// no way to test IsZero on its own.
func formatConnectivityTime(timeValue time.Time) string {
	if timeValue.IsZero() {
		return "Unknown"
	}

	return timeValue.Format("2006-01-02 3:04:05 PM")
}

// staleThreshold is how long a wireless thermometer can go without a sensor
// update before it's considered stale (e.g. the device went offline).
const staleThreshold = time.Hour

func isStale(timeValue time.Time) bool {
	return !timeValue.IsZero() && time.Since(timeValue) > staleThreshold
}

// lowBatteryThreshold is the battery percentage below which a wireless thermometer's
// battery icon is flagged as low.
const lowBatteryThreshold = 10

func isLowBattery(level int) bool {
	return level < lowBatteryThreshold
}

func relativeTime(timeValue time.Time) string {
	if timeValue.IsZero() {
		return "never"
	}

	elapsed := time.Since(timeValue).Truncate(time.Second)

	if elapsed.Seconds() < 30 {
		return "< 30s"
	}

	if elapsed.Seconds() < 60 {
		return "< 1m"
	}

	elapsed = elapsed.Truncate(time.Minute)

	if elapsed.Minutes() < 60 {
		return fmt.Sprintf("%vm", elapsed.Minutes())
	}

	elapsed = elapsed.Truncate(time.Hour)

	if elapsed.Hours() < 48 {
		return fmt.Sprintf("%vh", elapsed.Hours())
	}

	return fmt.Sprintf("%vd", int(elapsed.Hours()/24))
}

// formatClock renders a time of day for the high/low extremes, or nothing for a time that was
// never set.
func formatClock(timeValue time.Time) string {
	if timeValue.IsZero() {
		return ""
	}

	return timeValue.Format("3:04 PM")
}
