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
	ErrOffline          = errors.New("agent is offline")
	ErrTimeout          = errors.New("agent RPC timed out")
	ErrInvalidResponse  = errors.New("invalid agent RPC response")
	ErrDuplicateRequest = errors.New("duplicate request ID")
)

type rpcResult struct {
	response *protocol.RPCResponse
	err      error
}

type agentConnection struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	pending map[string]chan rpcResult
}

func (a *agentConnection) finish(id string, result rpcResult) bool {
	a.mu.Lock()
	ch, ok := a.pending[id]
	if ok {
		delete(a.pending, id)
		ch <- result
	}
	a.mu.Unlock()
	return ok
}

func (a *agentConnection) failAll(err error) {
	a.mu.Lock()
	for id, ch := range a.pending {
		delete(a.pending, id)
		ch <- rpcResult{err: err}
	}
	a.mu.Unlock()
}

type Manager struct {
	mu      sync.RWMutex
	agents  map[string]*agentConnection
	Timeout time.Duration
}

func NewManager() *Manager {
	return &Manager{agents: make(map[string]*agentConnection), Timeout: 10 * time.Second}
}

func (m *Manager) Register(id string, conn *websocket.Conn) *agentConnection {
	newAgent := &agentConnection{conn: conn, pending: make(map[string]chan rpcResult)}
	m.mu.Lock()
	old := m.agents[id]
	m.agents[id] = newAgent
	m.mu.Unlock()
	if old != nil {
		old.failAll(ErrOffline)
		old.conn.CloseNow()
	}
	return newAgent
}

// Unregister only removes the connection being closed, so an older agent cannot
// mark a replacement connection offline.
func (m *Manager) Unregister(id string, agent *agentConnection) bool {
	m.mu.Lock()
	if m.agents[id] != agent {
		m.mu.Unlock()
		return false
	}
	delete(m.agents, id)
	m.mu.Unlock()
	agent.failAll(ErrOffline)
	return true
}

func (m *Manager) Call(ctx context.Context, deviceID, method string) (*protocol.RPCResponse, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	return m.callWithID(ctx, deviceID, hex.EncodeToString(idBytes), method)
}

func (m *Manager) callWithID(ctx context.Context, deviceID, requestID, method string) (*protocol.RPCResponse, error) {
	m.mu.RLock()
	agent := m.agents[deviceID]
	if agent == nil {
		m.mu.RUnlock()
		return nil, ErrOffline
	}
	agent.mu.Lock()
	if _, exists := agent.pending[requestID]; exists {
		agent.mu.Unlock()
		m.mu.RUnlock()
		return nil, ErrDuplicateRequest
	}
	resultCh := make(chan rpcResult, 1)
	agent.pending[requestID] = resultCh
	agent.mu.Unlock()
	m.mu.RUnlock()
	defer agent.finish(requestID, rpcResult{})

	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := writeEnvelope(callCtx, agent.conn, protocol.Envelope{Version: protocol.Version, Type: protocol.MessageRPCRequest, RequestID: requestID, Request: &protocol.RPCRequest{Method: method}}); err != nil {
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
