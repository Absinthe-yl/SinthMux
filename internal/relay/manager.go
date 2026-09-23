package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/sinthmux/sinthmux/pkg/protocol"
)

var (
	ErrOffline          = errors.New("connector is offline")
	ErrTimeout          = errors.New("connector RPC timed out")
	ErrInvalidResponse  = errors.New("invalid connector RPC response")
	ErrDuplicateRequest = errors.New("duplicate request ID")
)

type rpcResult struct {
	response *protocol.RPCResponse
	err      error
}

type connectorConnection struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	pending map[string]chan rpcResult
	streams map[string]chan protocol.Envelope
	writeMu sync.Mutex
}

func (a *connectorConnection) send(ctx context.Context, envelope protocol.Envelope) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return writeEnvelope(ctx, a.conn, envelope)
}

func (a *connectorConnection) finish(id string, result rpcResult) bool {
	a.mu.Lock()
	ch, ok := a.pending[id]
	if ok {
		delete(a.pending, id)
		ch <- result
	}
	a.mu.Unlock()
	return ok
}

func (a *connectorConnection) failAll(err error) {
	a.mu.Lock()
	for id, ch := range a.pending {
		delete(a.pending, id)
		ch <- rpcResult{err: err}
	}
	for id, ch := range a.streams {
		delete(a.streams, id)
		closeEvent := protocol.Envelope{Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: id, Reason: err.Error()}}
		select {
		case ch <- closeEvent:
		default:
			<-ch
			ch <- closeEvent
		}
	}
	a.mu.Unlock()
}

func (a *connectorConnection) streamEvent(envelope protocol.Envelope) {
	var id string
	switch envelope.Type {
	case protocol.MessageStreamData:
		if envelope.StreamData != nil {
			id = envelope.StreamData.StreamID
		}
	case protocol.MessageStreamClose:
		if envelope.StreamClose != nil {
			id = envelope.StreamClose.StreamID
		}
	}
	if id == "" {
		return
	}
	a.mu.Lock()
	ch := a.streams[id]
	if ch != nil {
		if envelope.Type == protocol.MessageStreamClose {
			delete(a.streams, id)
		}
		select {
		case ch <- envelope:
		default:
			delete(a.streams, id)
			<-ch
			ch <- protocol.Envelope{Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: id, Reason: "terminal output exceeded buffer"}}
		}
	}
	a.mu.Unlock()
}

type Manager struct {
	mu         sync.RWMutex
	connectors map[string]*connectorConnection
	Timeout    time.Duration
}

func NewManager() *Manager {
	return &Manager{connectors: make(map[string]*connectorConnection), Timeout: 10 * time.Second}
}

func (m *Manager) Register(id string, conn *websocket.Conn) *connectorConnection {
	newConnector := &connectorConnection{conn: conn, pending: make(map[string]chan rpcResult), streams: make(map[string]chan protocol.Envelope)}
	m.mu.Lock()
	old := m.connectors[id]
	m.connectors[id] = newConnector
	m.mu.Unlock()
	if old != nil {
		old.failAll(ErrOffline)
		old.conn.CloseNow()
	}
	return newConnector
}

func (m *Manager) Online(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connectors[id] != nil
}

func (m *Manager) Revoke(id string) {
	m.mu.Lock()
	connector := m.connectors[id]
	delete(m.connectors, id)
	m.mu.Unlock()
	if connector != nil {
		connector.failAll(ErrOffline)
		connector.conn.CloseNow()
	}
}

func (m *Manager) OpenStream(ctx context.Context, deviceID, session string, cols, rows uint16) (string, <-chan protocol.Envelope, func(), error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, nil, err
	}
	id := hex.EncodeToString(idBytes)
	m.mu.RLock()
	connector := m.connectors[deviceID]
	if connector == nil {
		m.mu.RUnlock()
		return "", nil, nil, ErrOffline
	}
	ch := make(chan protocol.Envelope, 64)
	connector.mu.Lock()
	connector.streams[id] = ch
	connector.mu.Unlock()
	m.mu.RUnlock()
	closeStream := func() {
		connector.mu.Lock()
		delete(connector.streams, id)
		connector.mu.Unlock()
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = connector.send(closeCtx, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamClose, StreamClose: &protocol.StreamClose{StreamID: id}})
	}
	if err := connector.send(ctx, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageStreamOpen, StreamOpen: &protocol.StreamOpen{StreamID: id, Session: session, Cols: cols, Rows: rows}}); err != nil {
		closeStream()
		return "", nil, nil, err
	}
	return id, ch, closeStream, nil
}

func (m *Manager) SendStream(ctx context.Context, deviceID, id string, envelope protocol.Envelope) error {
	m.mu.RLock()
	connector := m.connectors[deviceID]
	if connector == nil {
		m.mu.RUnlock()
		return ErrOffline
	}
	connector.mu.Lock()
	_, exists := connector.streams[id]
	connector.mu.Unlock()
	m.mu.RUnlock()
	if !exists {
		return ErrOffline
	}
	return connector.send(ctx, envelope)
}

// Unregister only removes the connection being closed, so an older connector cannot
// mark a replacement connection offline.
func (m *Manager) Unregister(id string, connector *connectorConnection) bool {
	m.mu.Lock()
	if m.connectors[id] != connector {
		m.mu.Unlock()
		return false
	}
	delete(m.connectors, id)
	m.mu.Unlock()
	connector.failAll(ErrOffline)
	return true
}

func (m *Manager) Call(ctx context.Context, deviceID string, request protocol.RPCRequest) (*protocol.RPCResponse, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	return m.callWithID(ctx, deviceID, hex.EncodeToString(idBytes), request)
}

func (m *Manager) callWithID(ctx context.Context, deviceID, requestID string, request protocol.RPCRequest) (*protocol.RPCResponse, error) {
	m.mu.RLock()
	connector := m.connectors[deviceID]
	if connector == nil {
		m.mu.RUnlock()
		return nil, ErrOffline
	}
	connector.mu.Lock()
	if _, exists := connector.pending[requestID]; exists {
		connector.mu.Unlock()
		m.mu.RUnlock()
		return nil, ErrDuplicateRequest
	}
	resultCh := make(chan rpcResult, 1)
	connector.pending[requestID] = resultCh
	connector.mu.Unlock()
	m.mu.RUnlock()
	defer connector.finish(requestID, rpcResult{})

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := connector.send(callCtx, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: requestID, Request: &request}); err != nil {
		return nil, err
	}
	select {
	case result := <-resultCh:
		return result.response, result.err
	case <-callCtx.Done():
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, callCtx.Err()
	}
}
