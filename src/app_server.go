package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	errServerExited   = errors.New("Codex app-server disconnected")
	errUnconfirmedRPC = errors.New("Codex app-server did not confirm the request")
	errSlowConsumer   = errors.New("Codex turn event buffer full")
)

// A new login, provider configuration or CLI version gets a fresh connection.
// Existing turns keep their lease on the original process until they finish.
type fileStamp struct {
	inode          uint64
	size, modified int64
}

func stamp(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{info.Sys().(*syscall.Stat_t).Ino, info.Size(), info.ModTime().UnixNano()}, nil
}

type serverConfig struct {
	runner       runner
	auth, config fileStamp
}

type appServers struct {
	ctx      context.Context
	stop     context.CancelFunc
	mu       sync.Mutex
	current  map[bool]*appServer
	children sync.WaitGroup
}

type appServer struct {
	config serverConfig
	image  bool
	// leases is protected by the manager's mutex.
	leases   int
	ctx      context.Context
	stop     context.CancelFunc
	ready    chan struct{}
	startErr error
	writes   chan M
	denials  chan M
	mu       sync.Mutex
	seq      uint64
	pending  map[uint64]chan packet
	threads  map[string]*turnInbox
}

func newAppServers(parent context.Context) *appServers {
	ctx, stop := context.WithCancel(parent)
	return &appServers{ctx: ctx, stop: stop, current: make(map[bool]*appServer)}
}

func (m *appServers) close() {
	m.mu.Lock()
	m.stop()
	m.mu.Unlock()
	m.children.Wait()
}

func (m *appServers) acquire(ctx context.Context, r runner, image bool) (*appServer, error) {
	auth, err := stamp(filepath.Join(r.home, "auth.json"))
	if err != nil {
		return nil, err
	}
	config, err := stamp(filepath.Join(r.home, "config.toml"))
	if err != nil {
		return nil, err
	}
	key := serverConfig{r, auth, config}
	m.mu.Lock()
	if m.ctx.Err() != nil || ctx.Err() != nil {
		m.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errServerExited
	}
	// Retire both modes when the account or CLI changes, including an idle image process.
	for mode, old := range m.current {
		if old.config != key || old.ctx.Err() != nil {
			delete(m.current, mode)
			if old.leases == 0 {
				old.stop()
			}
		}
	}
	s := m.current[image]
	if s == nil {
		life, stop := context.WithCancel(m.ctx)
		s = &appServer{config: key, image: image, ctx: life, stop: stop, ready: make(chan struct{}),
			writes: make(chan M), denials: make(chan M, 32), seq: 1,
			pending: make(map[uint64]chan packet), threads: make(map[string]*turnInbox)}
		m.current[image] = s
		m.children.Add(1)
		go func() {
			defer m.children.Done()
			s.serve()
		}()
	}
	s.leases++
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.release(s, false)
		return nil, ctx.Err()
	case <-s.ready:
		if s.startErr != nil {
			m.release(s, true)
			return nil, s.startErr
		}
		return s, nil
	}
}

func (m *appServers) release(s *appServer, retire bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.leases--
	if retire && m.current[s.image] == s {
		delete(m.current, s.image)
	}
	if s.leases == 0 && m.current[s.image] != s {
		s.stop()
	}
}

func (s *appServer) serve() {
	timeout := time.AfterFunc(20*time.Second, s.stop)
	p, err := s.config.runner.open(s.ctx, s.image)
	timeout.Stop()
	s.startErr = err
	close(s.ready)
	if err != nil {
		s.stop()
		return
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			var frame M
			select {
			case <-s.ctx.Done():
				return
			case frame = <-s.writes:
			case frame = <-s.denials:
			}
			if err := p.send(frame); err != nil {
				s.stop()
				return
			}
		}
	}()
	for {
		v, err := p.read()
		if err != nil || !s.dispatch(v) {
			break
		}
	}
	s.stop()
	<-writerDone
	p.close()
}

// The stdout reader never waits for a client's HTTP writer or invokes its callbacks.
func (s *appServer) dispatch(v packet) bool {
	if len(v.ID) != 0 && v.Method != "" {
		select {
		case s.denials <- disabledAction(v):
			return true
		default:
			return false
		}
	}
	if len(v.ID) != 0 {
		id, err := strconv.ParseUint(string(v.ID), 10, 64)
		if err != nil {
			return true
		}
		s.mu.Lock()
		if reply := s.pending[id]; reply != nil {
			delete(s.pending, id)
			reply <- v
		}
		s.mu.Unlock()
		return true
	}
	switch v.Method {
	case "item/started", "item/completed", "item/agentMessage/delta", "thread/tokenUsage/updated", "turn/completed":
	default:
		return true
	}
	var route struct{ ThreadID string }
	if json.Unmarshal(v.Params, &route) != nil {
		return false
	}
	s.mu.Lock()
	if inbox := s.threads[route.ThreadID]; inbox != nil {
		inbox.push(v)
	}
	s.mu.Unlock()
	return true
}

// Once stdin accepts a state-changing call, briefly wait for its acknowledgement
// even on disconnect. Its thread/turn ID is needed for targeted cleanup.
func (s *appServer) call(ctx context.Context, method string, params M) (result json.RawMessage, err error) {
	if t := tracing(ctx); t != nil && t.kind == "" {
		start := time.Now()
		defer func() { recordHop(ctx, "stdio", method, start, err) }()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.seq++
	id := s.seq
	reply := make(chan packet, 1)
	s.pending[id] = reply
	s.mu.Unlock()
	// A late turn/start acknowledgement still identifies work we need to interrupt.
	var lateTurn bool
	defer func() {
		if lateTurn {
			go s.interruptLateTurn(reply, params["threadId"].(string))
			return
		}
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, errServerExited
	case s.writes <- M{"id": id, "method": method, "params": params}:
	}
	var v packet
	select {
	case <-s.ctx.Done():
		return nil, errServerExited
	case v = <-reply:
	case <-ctx.Done():
		switch method {
		case "turn/start", "thread/start", "thread/resume":
		default:
			return nil, ctx.Err()
		}
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case v = <-reply:
		case <-s.ctx.Done():
			return nil, errServerExited
		case <-timer.C:
			lateTurn = method == "turn/start"
			return nil, errors.Join(ctx.Err(), errUnconfirmedRPC)
		}
	}
	if v.Error != nil {
		return nil, fmt.Errorf("%s: rpc error %d", method, v.Error.Code)
	}
	return v.Result, nil
}

// The process is retired after an unconfirmed start. If other threads still use it,
// interrupt this turn when its ID arrives. Never send the original input again.
func (s *appServer) interruptLateTurn(reply <-chan packet, thread string) {
	select {
	case <-s.ctx.Done():
	case v := <-reply:
		var started struct{ Turn struct{ ID string } }
		if v.Error == nil && json.Unmarshal(v.Result, &started) == nil && started.Turn.ID != "" {
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			defer cancel()
			s.call(ctx, "turn/interrupt", M{"threadId": thread, "turnId": started.Turn.ID})
		}
	}
}

type turnInbox struct {
	events   chan packet
	overflow chan struct{}
	bytes    atomic.Int64
}

func (s *appServer) subscribe(thread string) (*turnInbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threads[thread] != nil {
		return nil, errors.New("Codex thread already has an active turn")
	}
	i := &turnInbox{events: make(chan packet, 256), overflow: make(chan struct{})}
	s.threads[thread] = i
	return i, nil
}

func (i *turnInbox) push(v packet) {
	select {
	case <-i.overflow:
		return
	default:
	}
	if i.bytes.Add(int64(len(v.Params))) <= 40<<20 {
		select {
		case i.events <- v:
			return
		default:
		}
	}
	close(i.overflow)
}

func (i *turnInbox) next(ctx, server context.Context) (packet, error) {
	if err := ctx.Err(); err != nil {
		return packet{}, err
	}
	select {
	case <-i.overflow:
		return packet{}, errSlowConsumer
	default:
	}
	select {
	case <-ctx.Done():
		return packet{}, ctx.Err()
	case <-server.Done():
		return packet{}, errServerExited
	case <-i.overflow:
		return packet{}, errSlowConsumer
	case v := <-i.events:
		i.bytes.Add(-int64(len(v.Params)))
		return v, nil
	}
}

func (s *appServer) finishThread(thread, turn string, completed bool, inbox *turnInbox) bool {
	defer func() {
		s.mu.Lock()
		delete(s.threads, thread)
		s.mu.Unlock()
	}()
	if s.ctx.Err() != nil {
		return false
	}
	confirmed := true
	if turn != "" && !completed {
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		defer cancel()
		s.call(ctx, "turn/interrupt", M{"threadId": thread, "turnId": turn})
		// The turn can finish naturally just before the interrupt reaches the CLI.
		// Wait for its terminal event before the next request can resume this thread.
		for {
			v, readErr := inbox.next(ctx, s.ctx)
			if readErr != nil {
				confirmed = false
				break
			}
			var n struct{ Turn struct{ ID string } }
			if v.Method == "turn/completed" && json.Unmarshal(v.Params, &n) == nil && n.Turn.ID == turn {
				break
			}
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	_, err := s.call(ctx, "thread/unsubscribe", M{"threadId": thread})
	return confirmed && err == nil
}
