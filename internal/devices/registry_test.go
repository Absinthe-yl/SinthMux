package devices

import (
	"testing"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

func TestRegistryLifecycle(t *testing.T) {
	registry := NewRegistry()
	registry.Connect(protocol.AgentHello{DeviceID: "laptop", Name: "Laptop"})
	registry.Touch("laptop")
	registry.Disconnect("laptop")

	devices := registry.List()
	if len(devices) != 1 || devices[0].Status != "offline" {
		t.Fatalf("unexpected devices: %#v", devices)
	}
}
