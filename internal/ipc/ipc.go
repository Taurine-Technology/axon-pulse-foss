// Package ipc implements the local-only daemon control protocol.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

type (
	Request struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
	}

	Response struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data,omitempty"`
		Error *Error          `json:"error,omitempty"`
	}

	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	Handler interface {
		HandleIPC(context.Context, Request) (any, *Error)
	}

	HandlerFunc func(context.Context, Request) (any, *Error)

	Stream struct {
		C     <-chan any
		Close func()
	}

	Server struct {
		Path         string
		Handler      Handler
		DrainTimeout time.Duration

		mu          sync.Mutex
		listener    net.Listener
		cancel      context.CancelFunc
		connections map[net.Conn]struct{}
		wg          sync.WaitGroup
		closeOnce   sync.Once
		closeErr    error
		closed      bool
		drained     chan struct{}
	}
)

var (
	ErrDrainTimeout = errors.New("IPC handlers did not drain before the shutdown deadline")
)

const (
	maxMessageBytes          = 1 << 20
	maxConcurrentConnections = 32
	maxConcurrentStreams     = 24
	defaultRequestTimeout    = 2 * time.Minute
	updateStageTimeout       = 7 * time.Minute
	streamWriteTimeout       = 10 * time.Second
	defaultDrainTimeout      = 30 * time.Second
)

func (e *Error) Error() string { return e.Message }

func (f HandlerFunc) HandleIPC(ctx context.Context, request Request) (any, *Error) {
	return f(ctx, request)
}

func (s *Server) Listen() error {
	if s.Path == "" || s.Handler == nil {
		return errors.New("IPC path and handler are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("IPC server is closed")
	}
	if s.listener != nil {
		return errors.New("IPC server is already listening")
	}
	listener, err := listenLocal(s.Path)
	if err != nil {
		return err
	}
	s.listener = listener
	s.connections = make(map[net.Conn]struct{})
	return nil
}

func (s *Server) Serve(ctx context.Context) (serveErr error) {
	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	if listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
		s.mu.Lock()
		listener = s.listener
		s.mu.Unlock()
	}
	serveCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancel = cancel
	drained := make(chan struct{})
	s.drained = drained
	s.mu.Unlock()
	stopClose := context.AfterFunc(serveCtx, func() { _ = s.Close() })
	defer func() {
		stopClose()
		cancel()
		closeErr := s.Close()
		go func() {
			s.wg.Wait()
			close(drained)
		}()
		drainTimeout := s.DrainTimeout
		if drainTimeout <= 0 {
			drainTimeout = defaultDrainTimeout
		}
		timer := time.NewTimer(drainTimeout)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			serveErr = errors.Join(serveErr, fmt.Errorf("%w: %s", ErrDrainTimeout, drainTimeout))
		}
		serveErr = errors.Join(serveErr, closeErr)
	}()
	connectionLimit := make(chan struct{}, maxConcurrentConnections)
	streamLimit := make(chan struct{}, maxConcurrentStreams)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if serveCtx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept IPC connection: %w", err)
		}
		select {
		case connectionLimit <- struct{}{}:
		case <-serveCtx.Done():
			_ = connection.Close()
			return nil
		}
		if !s.trackConnection(connection) {
			<-connectionLimit
			_ = connection.Close()
			return nil
		}
		s.wg.Add(1)
		go func() {
			defer func() {
				s.untrackConnection(connection)
				s.wg.Done()
				<-connectionLimit
				_ = connection.Close()
			}()
			s.handle(serveCtx, connection, streamLimit)
		}()
	}
}

// Drained closes after every accepted handler has returned. It remains useful
// when Serve reports ErrDrainTimeout, allowing an owner to keep dependent
// resources open until non-cooperative handlers eventually exit.
func (s *Server) Drained() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drained != nil {
		return s.drained
	}
	drained := make(chan struct{})
	close(drained)
	return drained
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		cancel := s.cancel
		listener := s.listener
		connections := make([]net.Conn, 0, len(s.connections))
		for connection := range s.connections {
			connections = append(connections, connection)
		}
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		var closeErr error
		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				closeErr = errors.Join(closeErr, fmt.Errorf("close IPC listener: %w", err))
			}
		}
		for _, connection := range connections {
			if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				closeErr = errors.Join(closeErr, fmt.Errorf("close IPC connection: %w", err))
			}
		}
		if listener != nil {
			if err := cleanupLocal(s.Path); err != nil {
				closeErr = errors.Join(closeErr, fmt.Errorf("remove IPC endpoint: %w", err))
			}
		}
		s.closeErr = closeErr
	})
	return s.closeErr
}

func (s *Server) trackConnection(connection net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil || s.closed {
		return false
	}
	s.connections[connection] = struct{}{}
	return true
}

func (s *Server) untrackConnection(connection net.Conn) {
	s.mu.Lock()
	delete(s.connections, connection)
	s.mu.Unlock()
}

func (s *Server) handle(ctx context.Context, connection net.Conn, streamLimit chan struct{}) {
	_ = connection.SetDeadline(time.Now().Add(defaultRequestTimeout))
	reader := bufio.NewReader(io.LimitReader(connection, maxMessageBytes+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		writeResponse(connection, nil, &Error{Code: "bad_request", Message: "could not read request"})
		return
	}
	if len(line) > maxMessageBytes {
		writeResponse(connection, nil, &Error{Code: "too_large", Message: "request exceeds 1 MiB"})
		return
	}
	var request Request
	if err := json.Unmarshal(line, &request); err != nil || request.Method == "" {
		writeResponse(connection, nil, &Error{Code: "bad_request", Message: "invalid request"})
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout(request.Method))
	defer cancel()
	_ = connection.SetDeadline(time.Now().Add(requestTimeout(request.Method)))
	data, ipcErr := s.Handler.HandleIPC(requestCtx, request)
	stream, streaming := data.(Stream)
	if !streaming {
		writeResponse(connection, data, ipcErr)
		return
	}
	select {
	case streamLimit <- struct{}{}:
		defer func() { <-streamLimit }()
	default:
		if stream.Close != nil {
			stream.Close()
		}
		writeResponse(connection, nil, &Error{Code: "stream_limit", Message: "too many active IPC watch streams"})
		return
	}
	if stream.Close != nil {
		defer stream.Close()
	}
	_ = connection.SetReadDeadline(time.Time{})
	for {
		select {
		case <-ctx.Done():
			return
		case value, ok := <-stream.C:
			if !ok {
				return
			}
			_ = connection.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			if !writeStreamResponse(connection, value) {
				return
			}
		}
	}
}

func writeStreamResponse(writer io.Writer, data any) bool {
	encoded, err := json.Marshal(data)
	if err != nil {
		return false
	}
	return json.NewEncoder(writer).Encode(Response{OK: true, Data: encoded}) == nil
}

func writeResponse(writer io.Writer, data any, ipcErr *Error) {
	response := Response{OK: ipcErr == nil, Error: ipcErr}
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			response.OK = false
			response.Error = &Error{Code: "internal", Message: "could not encode response"}
		} else {
			response.Data = encoded
		}
	}
	_ = json.NewEncoder(writer).Encode(response)
}

func Call(ctx context.Context, path, method string, params any, out any) error {
	var raw json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode IPC parameters: %w", err)
		}
		raw = encoded
	}
	connection, err := dialLocal(ctx, path)
	if err != nil {
		return fmt.Errorf("connect to pulsed at %s: %w", path, err)
	}
	defer func() { _ = connection.Close() }()
	deadline := time.Now().Add(requestTimeout(method))
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	if err := json.NewEncoder(connection).Encode(Request{Method: method, Params: raw}); err != nil {
		return fmt.Errorf("send IPC request: %w", err)
	}
	var response Response
	decoder := json.NewDecoder(io.LimitReader(connection, maxMessageBytes+1))
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("decode IPC response: %w", err)
	}
	if !response.OK {
		if response.Error == nil {
			return errors.New("pulsed rejected request")
		}
		return response.Error
	}
	if out != nil && len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, out); err != nil {
			return fmt.Errorf("decode IPC result: %w", err)
		}
	}
	return nil
}

func requestTimeout(method string) time.Duration {
	if method == "stage_update" {
		return updateStageTimeout
	}
	return defaultRequestTimeout
}

func Watch(ctx context.Context, path, method string, params any, receive func(json.RawMessage)) error {
	var raw json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encode IPC watch parameters: %w", err)
		}
		raw = encoded
	}
	connection, err := dialLocal(ctx, path)
	if err != nil {
		return fmt.Errorf("connect IPC watch: %w", err)
	}
	defer func() { _ = connection.Close() }()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-done:
		}
	}()
	defer close(done)
	if err := json.NewEncoder(connection).Encode(Request{Method: method, Params: raw}); err != nil {
		return fmt.Errorf("send IPC watch: %w", err)
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 4096), maxMessageBytes)
	for scanner.Scan() {
		var response Response
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			return fmt.Errorf("decode IPC watch: %w", err)
		}
		if !response.OK {
			if response.Error != nil {
				return response.Error
			}
			return errors.New("pulsed rejected watch")
		}
		if receive != nil {
			receive(append(json.RawMessage(nil), response.Data...))
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read IPC watch: %w", err)
	}
	return io.EOF
}
