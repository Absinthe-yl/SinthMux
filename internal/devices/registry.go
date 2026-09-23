package devices

import (
	"sort"
	"sync"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type Device struct {
	ID           string    `json:"id"`
	SpaceID      string    `json:"spaceId,omitempty"`
	Name         string    `json:"name"`
	Platform     string    `json:"platform"`
	Architecture string    `json:"architecture"`
	AgentVersion string    `json:"agentVersion"`
	Capabilities []string  `json:"capabilities"`
	Status       string    `json:"status"`
	ConnectedAt  time.Time `json:"connectedAt"`
	LastSeenAt   time.Time `json:"lastSeenAt"`
}

type Registry struct {
	mu      sync.RWMutex
	devices map[string]Device
}

func NewRegistry() *Registry {
	return &Registry{devices: make(map[string]Device)}
}

func (r *Registry) Connect(hello protocol.AgentHello) Device {
	now := time.Now().UTC()
	device := Device{ID: hello.DeviceID, Name: hello.Name, Platform: hello.Platform, Architecture: hello.Architecture, AgentVersion: hello.AgentVersion, Capabilities: hello.Capabilities, Status: "online", ConnectedAt: now, LastSeenAt: now}
	r.mu.Lock()
	r.devices[device.ID] = device
	r.mu.Unlock()
	return device
}

func (r *Registry) Touch(id string) {
	r.mu.Lock()
	device, ok := r.devices[id]
	if ok {
		device.LastSeenAt = time.Now().UTC()
		device.Status = "online"
		r.devices[id] = device
	}
	r.mu.Unlock()
}

func (r *Registry) Disconnect(id string) {
	r.mu.Lock()
	device, ok := r.devices[id]
	if ok {
		device.Status = "offline"
		device.LastSeenAt = time.Now().UTC()
		r.devices[id] = device
	}
	r.mu.Unlock()
}

func (r *Registry) List() []Device {
	r.mu.RLock()
	result := make([]Device, 0, len(r.devices))
	for _, device := range r.devices {
		result = append(result, device)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
