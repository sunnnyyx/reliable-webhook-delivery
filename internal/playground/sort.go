package playground

import (
	"relayforge/internal/delivery"
	"relayforge/internal/events"
	"sort"
)

func sortEvents(v []events.Event) {
	sort.Slice(v, func(i, j int) bool { return v[i].CreatedAt.After(v[j].CreatedAt) })
}
func sortDeliveries(v []delivery.Delivery) {
	sort.Slice(v, func(i, j int) bool { return v[i].CreatedAt.After(v[j].CreatedAt) })
}
