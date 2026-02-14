package environment

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/avanha/pmaas-plugin-environment/data"
	"github.com/avanha/pmaas-plugin-environment/entities"
	"github.com/avanha/pmaas-plugin-environment/internal/common"
	"github.com/avanha/pmaas-plugin-environment/internal/thermometer"
	environmental "github.com/avanha/pmaas-spi/environment"
	"github.com/avanha/pmaas-spi/events"
	"github.com/avanha/pmaas-spi/tracking"

	"github.com/avanha/pmaas-spi"
)

//go:embed content/static content/templates
var contentFS embed.FS

var IWirelessThermometerType = reflect.TypeOf((*environmental.IWirelessThermometer)(nil)).Elem()

var WirelessThermometerTemplate = spi.TemplateInfo{
	Name: "environment_wireless_thermometer",
	FuncMap: template.FuncMap{
		"CelsiusToFahrenheit": CelsiusToFahrenheit,
		"RelativeTime":        RelativeTime,
	},
	Paths:  []string{"templates/wireless_thermometer.htmlt"},
	Styles: []string{"css/wireless_thermometer.css"},
}

type state struct {
	container            spi.IPMAASContainer
	entities             map[string]common.IStateTracker
	entityCounter        int
	eventReceiverHandles map[string]int
}

func (s *state) nextEntityId() int {
	s.entityCounter = s.entityCounter + 1
	return s.entityCounter
}

type plugin struct {
	config PluginConfig
	state  state
}

type Plugin interface {
	spi.IPMAASPlugin
}

func NewPlugin(config PluginConfig) Plugin {
	fmt.Printf("New, config: %v\n", config)
	instance := &plugin{
		config: config,
		state: state{
			container:            nil,
			entities:             make(map[string]common.IStateTracker),
			entityCounter:        0,
			eventReceiverHandles: make(map[string]int),
		},
	}

	return instance
}

// Force implementation of spi.IPMAASPlugin
var _ spi.IPMAASPlugin = (*plugin)(nil)

func (p *plugin) Init(container spi.IPMAASContainer) {
	p.state.container = container
	container.ProvideContentFS(&contentFS, "content")
	container.EnableStaticContent("static")
	container.AddRoute("/plugins/environment/", p.handleHttpListRequest)

}

func (p *plugin) Start() {
	fmt.Printf("%T Starting...\n", *p)
	p.state.container.RegisterEntityRenderer(
		reflect.TypeOf((*thermometer.WirelessThermometer)(nil)).Elem(), p.wirelessThermometerRendererFactory)

	p.registerEventHandlers()
	// TODO: Retrieve the list of possible entities to add to our map.
	// Without it, we depend on the plugin ordering to ensure we get any devices in existence prior to our registration.
}

func (p *plugin) Stop() {
	fmt.Printf("%T Stopping...\n", *p)
}

func (p *plugin) registerEventHandlers() {
	var handle int
	var err error

	handle, err = p.state.container.RegisterEventReceiver(
		func(eventInfo *events.EventInfo) bool {
			// We only want entity registrations
			entityRegisteredEvent, ok := eventInfo.Event.(events.EntityRegisteredEvent)

			if !ok {
				return false
			}

			return isCompatibleEntityType(entityRegisteredEvent.EntityType)
		},
		p.onEntityRegistered,
	)

	if err != nil {
		panic(fmt.Sprintf("Unable to register for entity registration events: %v", err))
	}

	p.state.eventReceiverHandles["onEntityRegistered"] = handle

	handle, err = p.state.container.RegisterEventReceiver(
		func(eventInfo *events.EventInfo) bool {
			entityStateChangedEvent, ok := eventInfo.Event.(events.EntityStateChangedEvent)

			if !ok {
				return false
			}

			return isCompatibleEntityType(entityStateChangedEvent.EntityType)
		},
		p.onEntityStateChanged,
	)

	if err != nil {
		panic(fmt.Sprintf("Unable to register for entity state change events: %v", err))
	}

	p.state.eventReceiverHandles["onEntityStateChange"] = handle
}

var listRenderOptions spi.RenderListOptions = spi.RenderListOptions{
	Title: "Environmental Devices",
}

func (p *plugin) handleHttpListRequest(w http.ResponseWriter, r *http.Request) {
	// First, get the current state of all entities.  HTTP requests come in on arbitrary Go routines,
	// so execute getEntities on the main plugin Go routine to get all states atomically.
	resultCh := make(chan []any)
	err := p.state.container.EnqueueOnPluginGoRoutine(
		func() {
			resultCh <- p.getEntities()
			close(resultCh)
		})
	var items []any = nil
	if err == nil {
		items = <-resultCh
	} else {
		fmt.Printf("%T handleHttpListRequest: Error retrieving entities: %s\n", *p, err)
		items = make([]any, 0)
	}

	// Second, we want to pass a list of pointers to the entities we received to avoid
	// copying, so convert the entity list to a list of pointers to the entities.
	itemRefs := make([]any, len(items))

	for i := 0; i < len(items); i = i + 1 {
		switch typedItem := items[i].(type) {
		case thermometer.Thermometer:
			itemRefs[i] = &typedItem
		case thermometer.WirelessThermometer:
			// This is the type-specific way to get a pointer to a struct.  It should be faster
			// than the reflection-based approach below.
			itemRefs[i] = &typedItem
		default:
			itemType := reflect.TypeOf(typedItem)
			itemTypeKind := itemType.Kind()
			if itemTypeKind == reflect.Struct {
				// This is a generic way to construct a pointer to struct
				//fmt.Printf("items[%v] kind: %v\n", i, itemTypeKind)
				typedItemPointer := reflect.New(itemType)
				typedItemPointer.Elem().Set(reflect.ValueOf(typedItem))
				itemRefs[i] = typedItemPointer.Interface()
			} else if itemTypeKind == reflect.Interface || itemTypeKind == reflect.Ptr {
				// Interfaces and pointers are already references and don't need any conversion.
				//fmt.Printf("items[%v] kind: %v\n", i, itemTypeKind)
				itemRefs[i] = typedItem
			}
		}
	}

	// Third, sort the entities using their sort keys
	sort.Slice(
		itemRefs,
		func(i int, j int) bool {
			leftValue, leftOk := itemRefs[i].(common.ISortable)

			if leftOk {
				rightValue, rightOk := itemRefs[j].(common.ISortable)

				if rightOk {
					//fmt.Printf("Comparing %s < %s: %v\n",
					//	leftValue.GetSortKey(), rightValue.GetSortKey(), leftValue.GetSortKey() < rightValue.GetSortKey())
					return leftValue.GetSortKey() < rightValue.GetSortKey()
				}
			} else {
				//fmt.Printf("Unable to cast %T (%v) to ISortable\n", itemRefs[i], itemRefs[i])
			}
			return true
		})

	// Lastly, render the sorted entity list.  The render plugin will choose a matching rendered based on
	// the entity type.
	p.state.container.RenderList(w, r, listRenderOptions, itemRefs)
}

func (p *plugin) getEntities() []any {
	var entityList = make([]any, len(p.state.entities))
	i := 0
	for _, stateTrackingEntity := range p.state.entities {
		entityList[i] = stateTrackingEntity.GetState()
		i = i + 1
	}
	//fmt.Printf("getEntities(), list: %v\n", entityList)
	return entityList
}

func (p *plugin) onEntityRegistered(eventInfo *events.EventInfo) error {
	fmt.Printf("%T onEntityRegistered(%v)\n", *p, eventInfo)
	event := eventInfo.Event.(events.EntityRegisteredEvent)
	_, ok := p.state.entities[event.Id]

	if ok {
		return errors.New(fmt.Sprintf("Entity %s already tracked", event.Id))
	}

	var trackingConfig tracking.Config

	if event.Name == "" {
		// Do not track unnamed thermometers
		trackingConfig = tracking.Config{}
	} else {
		trackingConfig = tracking.Config{
			TrackingMode:        tracking.ModePoll,
			PollIntervalSeconds: 300,
			Name:                buildTrackingName("WirelessThermometer", event.Name),
			Schema: tracking.Schema{
				DataStructType:     data.WirelessThermometerDataType,
				InsertArgFactoryFn: data.WirelessThermometerDataToInsertArgs,
			},
		}
	}

	instance := thermometer.CreateWirelessThermometer(
		p.state.nextEntityId(), event.Id, event.Name, entities.WirelessThermometerType, trackingConfig)

	// This lambda captures both the plugin instance and the thermometer instance
	// and passes it to the entity manager.  However, since entities are deregistered on plugin
	// stop, so this is OK and doesn't leak memory.
	var stubFactoryFn spi.EntityStubFactoryFunc = func() (any, error) {
		return instance.GetStub(p.state.container), nil
	}
	p.state.entities[event.Id] = instance
	pmaasEntityId, err := p.state.container.RegisterEntity(
		instance.Id,
		entities.WirelessThermometerType,
		instance.Name,
		stubFactoryFn)

	if err == nil {
		instance.PmaasEntityId = pmaasEntityId
	} else {
		fmt.Printf("Device %s could not be registered: %v\n", instance.Id, err)
	}

	return nil
}

func buildTrackingName(prefix string, name string) string {
	result := fmt.Sprintf("%s_%s", prefix, name)
	result = strings.ReplaceAll(result, " ", "_")
	result = strings.ReplaceAll(result, "'", "")

	return result
}

func (p *plugin) onEntityStateChanged(eventInfo *events.EventInfo) error {
	fmt.Printf("%T onEntityStateChanged(%v)\n", *p, eventInfo)
	event := eventInfo.Event.(events.EntityStateChangedEvent)
	sourceEntityId := event.Id
	entity, ok := p.state.entities[sourceEntityId]

	if !ok {
		return errors.New(fmt.Sprintf("Entity %s is not tracked", event.Id))
	}

	return entity.ProcessNewState(event.NewState, func(pmassEntityId string, event any) {
		err := p.state.container.BroadcastEvent(pmassEntityId, event)
		if err != nil {
			fmt.Printf("%T Error broadcasting event %v", p, event)
		}
	})
}

func (p *plugin) wirelessThermometerRendererFactory() (spi.EntityRenderer, error) {
	// Load the template
	t, err := p.state.container.GetTemplate(&WirelessThermometerTemplate)

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

func isCompatibleEntityType(entityType reflect.Type) bool {
	result := entityType.AssignableTo(IWirelessThermometerType)
	//fmt.Printf("Checking entityType %v, result: %v\n", entityType, result)
	return result
}

func CelsiusToFahrenheit(celsiusValue float32) float32 {
	return celsiusValue*float32(9)/float32(5) + float32(32)
}

func RelativeTime(timeValue time.Time) string {
	elapsed := time.Now().Sub(timeValue).Truncate(time.Second)

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

	return fmt.Sprintf("%vh", elapsed.Hours())
}
