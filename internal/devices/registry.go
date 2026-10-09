package devices

import (
	"sort"
	"sync"
	"time"

	"github.com/sinthmux/sinthmux/pkg/protocol"
)

type Device struct {
	ID               string    `json:"id"`
	SpaceID          string    `json:"spaceId,omitempty"`
	Name             string    `json:"name"`
	Platform         string    `json:"platform"`
	Architecture     string    `json:"architecture"`
	ConnectorVersion string    `json:"connectorVersion"`
	Capabilities     []string  `json:"capabilities"`
	Status           string    `json:"status"`
	ConnectedAt      time.Time `json:"connectedAt"`
	LastSeenAt       time.Time `json:"lastSeenAt"`
	// Notifications are the sessions the device asked to highlight.
	Notifications []protocol.SessionNotification `json:"notifications"`
}

type Registry struct {
	mu      sync.RWMutex
	devices map[string]Device
}

func NewRegistry() *Registry {
	return &Registry{devices: make(map[string]Device)}
}

func (r *Registry) Connect(hello protocol.ConnectorHello) Device {
	now := time.Now().UTC()
	device := Device{ID: hello.DeviceID, Name: hello.Name, Platform: hello.Platform, Architecture: hello.Architecture, ConnectorVersion: hello.ConnectorVersion, Capabilities: hello.Capabilities, Status: "online", ConnectedAt: now, LastSeenAt: now, Notifications: []protocol.SessionNotification{}}
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
		device.Notifications = []protocol.SessionNotification{}
		r.devices[id] = device
	}
	r.mu.Unlock()
}

// SetNotifications replaces a device's notifications with the connector's snapshot.
func (r *Registry) SetNotifications(id string, items []protocol.SessionNotification) {
	clean := make([]protocol.SessionNotification, 0, len(items))
	for _, item := range items {
		if len(clean) == protocol.MaxNotifications {
			break
		}
		if !protocol.ValidSessionName(item.Session) || len(item.Message) > protocol.MaxNotificationMessage {
			continue
		}
		if !protocol.NotificationColors[item.Color] {
			item.Color = "blue"
		}
		clean = append(clean, item)
	}
	r.mu.Lock()
	if device, ok := r.devices[id]; ok {
		device.Notifications = clean
		r.devices[id] = device
	}
	r.mu.Unlock()
}

// HasCapability reports whether an online device advertised capability.
func (r *Registry) HasCapability(id, capability string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	device, ok := r.devices[id]
	if !ok || device.Status != "online" {
		return false
	}
	for _, item := range device.Capabilities {
		if item == capability {
			return true
		}
	}
	return false
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
