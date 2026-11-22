package desktop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxUnixSocketPathLen is the conservative portable limit for sockaddr_un.sun_path
// (104 bytes including NUL on macOS/BSD, 108 on Linux).
const maxUnixSocketPathLen = 103

// maxRPCRequestBytes bounds a single newline-delimited JSON request from a socket client.
const maxRPCRequestBytes = 1 << 20 // 1 MiB

// DefaultSocketPath returns the Unix domain socket path for the desktop host: <dataDir>/run/faro.sock.
func DefaultSocketPath(dataDir string) string {
	return filepath.Join(dataDir, "run", "faro.sock")
}

// rpcRequest is one newline-delimited JSON request from a local CLI client.
type rpcRequest struct {
	Method         string `json:"method"`
	ConversationID string `json:"conversationId,omitempty"`
	BotSlug        string `json:"botSlug,omitempty"`
	Text           string `json:"text,omitempty"`
	ApprovalID     string `json:"approvalId,omitempty"`
	Decision       string `json:"decision,omitempty"`
}

// rpcResponse is the single JSON reply line for one-shot RPCs and the opening frame for attach.
type rpcResponse struct {
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
	Chats    []ChatSummary `json:"chats,omitempty"`
	Chat     *ChatInfo     `json:"chat,omitempty"`
	TurnID   string        `json:"turnId,omitempty"`
	Snapshot *Snapshot     `json:"snapshot,omitempty"`
}

// Server exposes a Controller over a local Unix domain socket using newline-delimited JSON.
//
// Security note: access control relies solely on filesystem permissions on the parent directory
// (<dataDir>/run, mode 0700, owned by the current OS user). Any process running as the same OS
// user can connect to this socket, inspect conversations, send messages, and approve actions.
// On Windows, POSIX file modes are not enforced.
type Server struct {
	path string
	ctl  *Controller
	ln   net.Listener

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// StartServer listens on socketPath (creating its parent directory with mode 0700 and removing
// a stale socket file if nothing is listening on it) and serves requests against ctl.
func StartServer(socketPath string, ctl *Controller) (*Server, error) {
	if ctl == nil {
		return nil, errors.New("controller is required")
	}
	socketPath = filepath.Clean(strings.TrimSpace(socketPath))
	if socketPath == "" || socketPath == "." {
		return nil, errors.New("socket path is required")
	}
	if len(socketPath) > maxUnixSocketPathLen {
		return nil, fmt.Errorf("socket path %q is too long (%d bytes; max %d for Unix domain sockets)", socketPath, len(socketPath), maxUnixSocketPathLen)
	}

	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("set permissions on socket dir %q: %w", dir, err)
	}

	if _, err := os.Lstat(socketPath); err == nil {
		conn, dialErr := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("socket %q is already in use", socketPath)
		}
		if rmErr := os.Remove(socketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale socket %q: %w", socketPath, rmErr)
		}
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", socketPath, err)
	}

	s := &Server{
		path:  socketPath,
		ctl:   ctl,
		ln:    ln,
		done:  make(chan struct{}),
		conns: map[net.Conn]struct{}{},
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Path returns the bound socket path.
func (s *Server) Path() string { return s.path }

// Close stops accepting connections, closes active client connections, waits for handlers, and
// removes the socket file.
func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.done)
		closeErr = s.ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
		_ = os.Remove(s.path)
	})
	return closeErr
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				return
			}
		}
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			_ = conn.Close()
			return
		default:
			s.conns[conn] = struct{}{}
		}
		s.mu.Unlock()

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.handleConn(c)
		}(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	line, err := readBoundedLine(conn, maxRPCRequestBytes)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			_ = writeJSONLine(conn, rpcResponse{OK: false, Error: err.Error()})
		}
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_ = writeJSONLine(conn, rpcResponse{OK: false, Error: fmt.Sprintf("invalid request JSON: %v", err)})
		return
	}

	ctx := s.ctl.rootCtx
	switch req.Method {
	case "list_chats":
		chats, err := s.ctl.ListChats(ctx)
		if err != nil {
			_ = writeJSONLine(conn, rpcResponse{OK: false, Error: err.Error()})
			return
		}
		_ = writeJSONLine(conn, rpcResponse{OK: true, Chats: chats})

	case "new_chat":
		info, err := s.ctl.NewChat(ctx, req.BotSlug)
		if err != nil {
			_ = writeJSONLine(conn, rpcResponse{OK: false, Error: err.Error()})
			return
		}
		_ = writeJSONLine(conn, rpcResponse{OK: true, Chat: &info})

	case "send":
		turnID, err := s.ctl.Send(ctx, req.ConversationID, req.Text)
		if err != nil {
			_ = writeJSONLine(conn, rpcResponse{OK: false, Error: err.Error()})
			return
		}
		_ = writeJSONLine(conn, rpcResponse{OK: true, TurnID: turnID})

	case "cancel":
		s.ctl.Cancel(req.ConversationID)
		_ = writeJSONLine(conn, rpcResponse{OK: true})

	case "respond_approval":
		if err := s.ctl.RespondApproval(req.ApprovalID, req.Decision); err != nil {
			_ = writeJSONLine(conn, rpcResponse{OK: false, Error: err.Error()})
			return
		}
		_ = writeJSONLine(conn, rpcResponse{OK: true})

	case "attach":
		s.handleAttach(ctx, conn, req.ConversationID)

	default:
		_ = writeJSONLine(conn, rpcResponse{OK: false, Error: fmt.Sprintf("unknown method %q", req.Method)})
	}
}

func (s *Server) handleAttach(ctx context.Context, conn net.Conn, convID string) {
	snap, ch, unsub, err := s.ctl.Attach(ctx, convID)
	if err != nil {
		_ = writeJSONLine(conn, rpcResponse{OK: false, Error: err.Error()})
		return
	}
	defer unsub()

	if err := writeJSONLine(conn, rpcResponse{OK: true, Snapshot: &snap}); err != nil {
		return
	}

	// Watch for client disconnect so the subscriber is removed promptly even while idle.
	clientClosed := make(chan struct{})
	go func() {
		defer close(clientClosed)
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-clientClosed:
			return
		case <-s.done:
			return
		case ev, ok := <-ch:
			if !ok {
				// Subscriber buffer overflowed or controller shut down; closing conn signals disconnect.
				return
			}
			if err := writeJSONLine(conn, ev); err != nil {
				return
			}
		}
	}
}

func readBoundedLine(r io.Reader, maxBytes int64) ([]byte, error) {
	lr := io.LimitReader(r, maxBytes+1)
	br := bufio.NewReader(lr)
	line, err := br.ReadBytes('\n')
	if int64(len(line)) > maxBytes {
		return nil, errors.New("request line too large")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(line) == 0 {
		return nil, io.EOF
	}
	return line, nil
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
