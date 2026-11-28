package desktop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// ErrHostNotRunning indicates that the Faro desktop app socket could not be dialed.
var ErrHostNotRunning = errors.New("Faro desktop app is not running")

// Client talks to a running desktop.Server over its Unix domain socket.
type Client struct {
	socketPath string
}

// NewClient returns a Client targeting socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// SocketPath returns the socket path this client connects to.
func (c *Client) SocketPath() string { return c.socketPath }

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w (could not connect to %s: %v)", ErrHostNotRunning, c.socketPath, err)
	}
	return conn, nil
}

func (c *Client) call(ctx context.Context, req rpcRequest) (rpcResponse, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return rpcResponse{}, err
	}
	defer conn.Close()

	if err := writeJSONLine(conn, req); err != nil {
		return rpcResponse{}, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return rpcResponse{}, fmt.Errorf("read response: %w", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return rpcResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if !resp.OK {
		if resp.Error == "" {
			return rpcResponse{}, errors.New("request failed")
		}
		return rpcResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}

// ListChats returns recent single-bot conversations from the desktop host.
func (c *Client) ListChats(ctx context.Context) ([]ChatSummary, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "list_chats"})
	if err != nil {
		return nil, err
	}
	if resp.Chats == nil {
		return []ChatSummary{}, nil
	}
	return resp.Chats, nil
}

// BotChat returns botSlug's one chat on the desktop host, creating it on first use.
func (c *Client) BotChat(ctx context.Context, botSlug string) (ChatInfo, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "bot_chat", BotSlug: botSlug})
	if err != nil {
		return ChatInfo{}, err
	}
	if resp.Chat == nil {
		return ChatInfo{}, errors.New("missing chat info in response")
	}
	return *resp.Chat, nil
}

// Send submits a message to convID on the desktop host and returns its turn id.
func (c *Client) Send(ctx context.Context, convID, text string) (string, error) {
	resp, err := c.call(ctx, rpcRequest{Method: "send", ConversationID: convID, Text: text})
	if err != nil {
		return "", err
	}
	return resp.TurnID, nil
}

// Cancel stops the running turn on the desktop host if it belongs to convID.
func (c *Client) Cancel(ctx context.Context, convID string) error {
	_, err := c.call(ctx, rpcRequest{Method: "cancel", ConversationID: convID})
	return err
}

// RespondApproval delivers an approval decision ("allow", "allow_session", or "deny").
func (c *Client) RespondApproval(ctx context.Context, approvalID, decision string) error {
	_, err := c.call(ctx, rpcRequest{Method: "respond_approval", ApprovalID: approvalID, Decision: decision})
	return err
}

// Stream delivers live events from an attached conversation until disconnected or closed.
type Stream struct {
	conn   net.Conn
	events chan Event
	done   chan struct{} // closed by Close so readLoop never blocks on an abandoned events channel

	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	err       error
}

// Attach opens a long-lived subscription to convID and returns its initial Snapshot plus a live
// event Stream. Callers must call Stream.Close when done.
func (c *Client) Attach(ctx context.Context, convID string) (Snapshot, *Stream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if err := writeJSONLine(conn, rpcRequest{Method: "attach", ConversationID: convID}); err != nil {
		_ = conn.Close()
		return Snapshot{}, nil, err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		_ = conn.Close()
		return Snapshot{}, nil, fmt.Errorf("read attach snapshot: %w", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		_ = conn.Close()
		return Snapshot{}, nil, fmt.Errorf("decode attach snapshot: %w", err)
	}
	if !resp.OK {
		_ = conn.Close()
		if resp.Error == "" {
			return Snapshot{}, nil, errors.New("attach failed")
		}
		return Snapshot{}, nil, errors.New(resp.Error)
	}
	if resp.Snapshot == nil {
		_ = conn.Close()
		return Snapshot{}, nil, errors.New("missing snapshot in attach response")
	}

	st := &Stream{
		conn:   conn,
		events: make(chan Event, 64),
		done:   make(chan struct{}),
	}
	go st.readLoop(br)
	return *resp.Snapshot, st, nil
}

// Events returns the channel of live events. It is closed when the stream disconnects or Close is called.
func (s *Stream) Events() <-chan Event { return s.events }

// Err returns the read error that ended the stream, or nil if closed locally or ended cleanly with EOF.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close closes the underlying socket connection and terminates the stream.
func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		err = s.conn.Close()
	})
	return err
}

func (s *Stream) readLoop(br *bufio.Reader) {
	defer close(s.events)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var ev Event
			if uErr := json.Unmarshal(line, &ev); uErr != nil {
				s.setErr(fmt.Errorf("decode event: %w", uErr))
				return
			}
			select {
			case s.events <- ev:
			case <-s.done:
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.setErr(err)
			}
			return
		}
	}
}

func (s *Stream) setErr(err error) {
	s.mu.Lock()
	if !s.closed {
		s.err = err
	}
	s.mu.Unlock()
}
