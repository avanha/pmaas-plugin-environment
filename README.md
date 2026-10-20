# PMAAS Environment Plugin

A plugin that wraps lower-level environmental sensors and controllers
and provides additional functionality and a user interface.

The idea is to re-use the same interface for multiple sensors.

## Notes

- Tracks daily highs and lows.
- Provides a single entity type over multiple lower-level types. 

## Groups

The plugin's card for a thermometer or thermostat has more on it than the source plugin's own (the Bluetooth plugin's, the Nest plugin's), so an assembly that organises entities into groups (the groups plugin) can hold this plugin's entity instead of the source's. `Of` takes what the source plugin's config gave back and says which entity of this plugin shows it:

```go
environmentConfig := environment.NewPluginConfig()
...
cougar.Group("Master Bedroom").Add(environmentConfig.Of(masterBedroomThermometer))
```

The entity has a view that covers the source's, so a page that lists everything, such as the groups plugin's root, shows this plugin's card and not both. Entities from a peer (net discovery) have no view.
