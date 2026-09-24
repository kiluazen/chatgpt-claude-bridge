package bridge

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/config"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

// sessions holds one Session per Codex thread. Lock order: a Session's mu may
// be held while taking sessions.mu, never the other way round.
type sessions struct {
	cfg      config.Config
	commands *commands
	mu       sync.Mutex
	byThread map[string]*Session
}

// get returns the thread's session, starting Claude Code for a new one.
func (m *sessions) get(req *responses.Request) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.byThread[req.ThreadID()]; s != nil {
		return s, nil
	}
	s, err := newSession(m.cfg, m.commands, req, m.remove)
	if err != nil {
		return nil, err
	}
	m.byThread[s.threadID] = s
	return s, nil
}

func (m *sessions) remove(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byThread[s.threadID] == s {
		delete(m.byThread, s.threadID)
	}
}

// byMCP finds the session an MCP request belongs to.
func (m *sessions) byMCP(id, secret string) *Session {
	for _, s := range m.list() {
		if s.id == id && subtle.ConstantTimeCompare([]byte(s.secret), []byte(secret)) == 1 {
			return s
		}
	}
	return nil
}

func (m *sessions) list() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*Session, 0, len(m.byThread))
	for _, s := range m.byThread {
		list = append(list, s)
	}
	return list
}

func (m *sessions) sweep(now time.Time) {
	for _, s := range m.list() {
		if s.sweep(now) {
			m.remove(s)
			go s.close()
		}
	}
}

func (m *sessions) busy() int {
	n := 0
	for _, s := range m.list() {
		if s.busy() {
			n++
		}
	}
	return n
}

// shutdown ends every session for a restart and waits for their Claude Code
// processes to exit.
func (m *sessions) shutdown() {
	var procs []*claude.Process
	for _, s := range m.list() {
		if p := s.shutdown(); p != nil {
			procs = append(procs, p)
		}
	}
	// Give Claude Code a moment to record the interrupted turns first.
	time.Sleep(400 * time.Millisecond)
	var wg sync.WaitGroup
	for _, p := range procs {
		wg.Go(func() { p.Terminate(3 * time.Second) })
	}
	wg.Wait()
}
