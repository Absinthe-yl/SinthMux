package protocol

import (
	"regexp"
	"time"
)

const Version uint32 = 1

type MessageType string

const (
	MessageAgentHello  MessageType = "agent.hello"
	MessageHeartbeat   MessageType = "agent.heartbeat"
	MessageAck         MessageType = "hub.ack"
	MessageError       MessageType = "hub.error"
	MessageRPCRequest  MessageType = "rpc.request"
	MessageRPCResponse MessageType = "rpc.response"
)

type Envelope struct {
	Version   uint32       `json:"version"`
	Type      MessageType  `json:"type"`
	RequestID string       `json:"requestId,omitempty"`
	Hello     *AgentHello  `json:"hello,omitempty"`
	Heartbeat *Heartbeat   `json:"heartbeat,omitempty"`
	Ack       *Ack         `json:"ack,omitempty"`
	Error     *Error       `json:"error,omitempty"`
	Request   *RPCRequest  `json:"request,omitempty"`
	Response  *RPCResponse `json:"response,omitempty"`
}

type AgentHello struct {
	DeviceID     string   `json:"deviceId"`
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	Architecture string   `json:"architecture"`
	AgentVersion string   `json:"agentVersion"`
	Capabilities []string `json:"capabilities"`
}

type Heartbeat struct {
	DeviceID string    `json:"deviceId"`
	SentAt   time.Time `json:"sentAt"`
}

type Ack struct {
	Message string `json:"message"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RPCRequest struct {
	Method  string `json:"method"`
	Name    string `json:"name,omitempty"`
	NewName string `json:"newName,omitempty"`
}

type RPCResponse struct {
	OK        bool          `json:"ok"`
	Sessions  []TmuxSession `json:"sessions,omitempty"`
	Error     string        `json:"error,omitempty"`
	ErrorCode string        `json:"errorCode,omitempty"`
	Name      string        `json:"name,omitempty"`
}

var sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func ValidSessionName(name string) bool {
	return sessionNamePattern.MatchString(name)
}

type TmuxSession struct {
	Name      string `json:"name"`
	Windows   int    `json:"windows"`
	Attached  bool   `json:"attached"`
	CreatedAt int64  `json:"createdAt"`
}
