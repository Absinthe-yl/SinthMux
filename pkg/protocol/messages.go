package protocol

import (
	"regexp"
	"time"
)

const Version uint32 = 1

type MessageType string

const (
	MessageConnectorHello MessageType = "connector.hello"
	MessageHeartbeat      MessageType = "connector.heartbeat"
	MessageAck            MessageType = "hub.ack"
	MessageError          MessageType = "hub.error"
	MessageRPCRequest     MessageType = "rpc.request"
	MessageRPCResponse    MessageType = "rpc.response"
	MessageStreamOpen     MessageType = "stream.open"
	MessageStreamData     MessageType = "stream.data"
	MessageStreamResize   MessageType = "stream.resize"
	MessageStreamClose    MessageType = "stream.close"
	// MessageNotifications carries the connector's full set of session notifications.
	MessageNotifications MessageType = "connector.notifications"
)

// Capabilities a connector may advertise in its hello beyond the base set.
const (
	CapabilityFileUpload     = "file.upload.v1"
	CapabilityTerminalExport = "terminal.export.v1"
	CapabilitySessionNotify  = "session.notify.v1"
)

// TransferChunkSize is the largest raw payload in one upload or export RPC.
// JSON encodes []byte as base64, so 32 KiB stays well inside the 64 KiB
// WebSocket read limit on both sides.
const TransferChunkSize = 32 << 10

// Upload and notification bounds shared by the Hub, connector and tests.
const (
	MaxUploadSize          = 20 << 20
	MaxNotificationMessage = 200
	MaxNotifications       = 200
)

type Envelope struct {
	Version      uint32          `json:"version"`
	Type         MessageType     `json:"type"`
	RequestID    string          `json:"requestId,omitempty"`
	Hello        *ConnectorHello `json:"hello,omitempty"`
	Heartbeat    *Heartbeat      `json:"heartbeat,omitempty"`
	Ack          *Ack            `json:"ack,omitempty"`
	Error        *Error          `json:"error,omitempty"`
	Request      *RPCRequest     `json:"request,omitempty"`
	Response     *RPCResponse    `json:"response,omitempty"`
	StreamOpen   *StreamOpen     `json:"streamOpen,omitempty"`
	StreamData   *StreamData     `json:"streamData,omitempty"`
	StreamResize *StreamResize   `json:"streamResize,omitempty"`
	StreamClose  *StreamClose    `json:"streamClose,omitempty"`
	// Notifications is set on connector.notifications messages.
	Notifications *NotificationSnapshot `json:"notifications,omitempty"`
}

type ConnectorHello struct {
	DeviceID         string   `json:"deviceId"`
	Name             string   `json:"name"`
	Platform         string   `json:"platform"`
	Architecture     string   `json:"architecture"`
	ConnectorVersion string   `json:"connectorVersion"`
	Capabilities     []string `json:"capabilities"`
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
	Method   string           `json:"method"`
	Name     string           `json:"name,omitempty"`
	NewName  string           `json:"newName,omitempty"`
	Transfer *TransferRequest `json:"transfer,omitempty"`
}

type RPCResponse struct {
	OK        bool            `json:"ok"`
	Sessions  []TmuxSession   `json:"sessions,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorCode string          `json:"errorCode,omitempty"`
	Name      string          `json:"name,omitempty"`
	Transfer  *TransferResult `json:"transfer,omitempty"`
}

// TransferRequest carries the arguments of file.upload.* and terminal.export.* RPCs.
type TransferRequest struct {
	ID       string `json:"id,omitempty"`
	FileName string `json:"fileName,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
	Data     []byte `json:"data,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// Lines limits an export to the last N history lines; 0 exports everything.
	Lines int `json:"lines,omitempty"`
}

type TransferResult struct {
	ID       string `json:"id,omitempty"`
	Path     string `json:"path,omitempty"`
	FileName string `json:"fileName,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Data     []byte `json:"data,omitempty"`
	EOF      bool   `json:"eof,omitempty"`
}

// SessionNotification is one highlighted tmux session on a device.
type SessionNotification struct {
	Session string `json:"session"`
	Color   string `json:"color"`
	Message string `json:"message"`
	At      int64  `json:"at"`
}

type NotificationSnapshot struct {
	Items []SessionNotification `json:"items"`
}

// NotificationColors lists the colors a notification may use; others become blue.
var NotificationColors = map[string]bool{"blue": true, "green": true, "yellow": true, "red": true}

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

type StreamOpen struct {
	StreamID string `json:"streamId"`
	Session  string `json:"session"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
}

type StreamData struct {
	StreamID string `json:"streamId"`
	Data     []byte `json:"data"`
}

type StreamResize struct {
	StreamID string `json:"streamId"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
}

type StreamClose struct {
	StreamID string `json:"streamId"`
	Reason   string `json:"reason,omitempty"`
}
