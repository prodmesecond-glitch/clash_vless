// Package control is the daemon↔client IPC. The daemon (`run`) serves a Unix
// socket that streams status/log/state events and accepts commands; the TUI and
// CLI attach as clients. Wire format is JSON values over the socket.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"clashvless/internal/engine"
	"clashvless/internal/happ"
	"clashvless/internal/store"
)

// Event is a daemon→client message.
type Event struct {
	Type   string          `json:"type"` // "state" | "status" | "log"
	State  json.RawMessage `json:"state,omitempty"`
	Status *engine.Status  `json:"status,omitempty"`
	Line   string          `json:"line,omitempty"`
}

// Command is a client→daemon message.
type Command struct {
	Cmd   string          `json:"cmd"` // "patch" | "addsub" | "rmsub" | "refetch" | "kick"
	Patch json.RawMessage `json:"patch,omitempty"`
	URL   string          `json:"url,omitempty"`
	Index int             `json:"index,omitempty"`
}

// --- hub: fan events out to connected clients --------------------------------

type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

func (h *Hub) Broadcast(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // slow client — drop rather than stall the engine
		}
	}
}

func (h *Hub) subscribe() chan Event {
	ch := make(chan Event, 128)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// --- server (daemon side) ----------------------------------------------------

type Server struct {
	sup *engine.Supervisor
	hub *Hub
}

func NewServer(sup *engine.Supervisor, hub *Hub) *Server { return &Server{sup: sup, hub: hub} }

// Serve listens on socketPath until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	_ = os.Remove(socketPath) // clear a stale socket
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(socketPath, 0o600)
	go func() {
		<-ctx.Done()
		ln.Close()
		_ = os.Remove(socketPath)
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil // listener closed (ctx done)
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// Initial snapshot so a fresh client can render immediately.
	_ = enc.Encode(Event{Type: "state", State: s.sup.Snapshot()})
	st := s.sup.CurrentStatus()
	_ = enc.Encode(Event{Type: "status", Status: &st})

	go func() {
		for e := range ch {
			if enc.Encode(e) != nil {
				conn.Close()
				return
			}
		}
	}()

	dec := json.NewDecoder(conn)
	for {
		var cmd Command
		if dec.Decode(&cmd) != nil {
			return // client gone
		}
		s.exec(cmd)
	}
}

func (s *Server) exec(cmd Command) {
	switch cmd.Cmd {
	case "kick":
		s.sup.Kick()
	case "patch":
		s.applyAndBroadcast(func(st *store.State) { _ = json.Unmarshal(cmd.Patch, st) })
	case "rmsub":
		i := cmd.Index
		s.applyAndBroadcast(func(st *store.State) { st.RemoveSub(i) })
	case "addsub":
		go s.addSub(cmd.URL)
	case "refetch":
		go s.refetch()
	}
}

func (s *Server) applyAndBroadcast(mut func(*store.State)) {
	s.sup.SetConfig(mut)
	_ = s.sup.Save()
	s.sup.Kick()
	s.hub.Broadcast(Event{Type: "state", State: s.sup.Snapshot()})
}

func (s *Server) addSub(url string) {
	var st store.State
	_ = json.Unmarshal(s.sup.Snapshot(), &st)
	nodes, title, err := happ.Fetch(st.Device, url, st.FetchProxyAddr())
	s.applyAndBroadcast(func(st *store.State) {
		st.AddSub(title, url)
		if err == nil {
			sb := &st.Subs[len(st.Subs)-1]
			sb.Nodes = nodes
			sb.LastFetch = time.Now()
		}
	})
	if err != nil {
		s.hub.Broadcast(Event{Type: "log", Line: fmt.Sprintf("add %s: %v", url, err)})
	} else {
		s.hub.Broadcast(Event{Type: "log", Line: fmt.Sprintf("added %q — %d nodes", title, len(nodes))})
	}
}

func (s *Server) refetch() {
	var st store.State
	_ = json.Unmarshal(s.sup.Snapshot(), &st)
	dev := st.Device
	type upd struct {
		url, title string
		nodes      []store.Node
		ok         bool
		err        error
	}
	ups := make([]upd, len(st.Subs))
	for i, sb := range st.Subs {
		nodes, title, err := happ.Fetch(dev, sb.URL, st.FetchProxyAddr())
		ups[i] = upd{sb.URL, title, nodes, err == nil, err}
	}
	s.applyAndBroadcast(func(st *store.State) {
		for _, u := range ups {
			if !u.ok {
				continue
			}
			for i := range st.Subs {
				if st.Subs[i].URL == u.url {
					st.Subs[i].Nodes = u.nodes
					st.Subs[i].LastFetch = time.Now()
					if u.title != "" {
						st.Subs[i].Name = u.title
					}
				}
			}
		}
	})
	if len(ups) == 0 {
		s.hub.Broadcast(Event{Type: "log", Line: "refetch: no subscriptions to fetch"})
	}
	for _, u := range ups {
		if u.ok {
			name := u.title
			if name == "" {
				name = u.url
			}
			s.hub.Broadcast(Event{Type: "log", Line: fmt.Sprintf("refetched %q — %d nodes", name, len(u.nodes))})
		} else {
			s.hub.Broadcast(Event{Type: "log", Line: fmt.Sprintf("refetch %s: %v", u.url, u.err)})
		}
	}
}

// --- client side -------------------------------------------------------------

type Client struct {
	conn   net.Conn
	enc    *json.Encoder
	events chan Event
}

// Dial connects to the daemon's control socket.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, enc: json.NewEncoder(conn), events: make(chan Event, 128)}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.events)
	dec := json.NewDecoder(c.conn)
	for {
		var e Event
		if dec.Decode(&e) != nil {
			return
		}
		c.events <- e
	}
}

// Events streams daemon events; the channel closes when the daemon disconnects.
func (c *Client) Events() <-chan Event { return c.events }

func (c *Client) Send(cmd Command) error { return c.enc.Encode(cmd) }

func (c *Client) SendPatch(patch json.RawMessage) error {
	return c.Send(Command{Cmd: "patch", Patch: patch})
}

func (c *Client) Close() error { return c.conn.Close() }
