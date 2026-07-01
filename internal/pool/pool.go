// Package pool implements a concurrency-safe rotating pool of tokens with
// per-token health, cooldown, and JSON state persistence.
package pool

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is a token's health state.
type Status string

const (
	Unknown Status = "unknown" // not yet checked
	Live    Status = "live"    // last check passed
	Dead    Status = "dead"    // last check or use failed
)

// Token holds a value and its runtime state. Value is never persisted.
type Token struct {
	ID           string    `json:"id"`
	Value        string    `json:"-"`
	Status       Status    `json:"status"`
	LastCheck    time.Time `json:"last_check"`
	LastUsed     time.Time `json:"last_used"`
	CoolingUntil time.Time `json:"cooling_until"`
	Fails        int       `json:"fails"`
}

// Lease is a token handed out by Take.
type Lease struct {
	ID    string
	Value string
}

// Options configures a Pool. Now and Rand are injectable for tests.
type Options struct {
	Rotation  string        // lru | round_robin | random (default lru)
	Cooldown  time.Duration // rest period applied on Take and on failed Release
	StateFile string        // JSON state file; empty disables persistence
	Now       func() time.Time
	Rand      *rand.Rand
}

// Pool is a thread-safe token pool.
type Pool struct {
	mu        sync.Mutex
	order     []*Token
	byID      map[string]*Token
	rotation  string
	cooldown  time.Duration
	rrCursor  int
	rng       *rand.Rand
	stateFile string
	now       func() time.Time
}

// New builds an empty pool. Add tokens with SetTokens.
func New(o Options) *Pool {
	if o.Rotation == "" {
		o.Rotation = "lru"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Pool{
		byID:      map[string]*Token{},
		rotation:  o.Rotation,
		cooldown:  o.Cooldown,
		rrCursor:  -1,
		rng:       o.Rand,
		stateFile: o.StateFile,
		now:       o.Now,
	}
}

// SetTokens reconciles the pool with the given lines. Each non-empty,
// non-comment line is "id,value" or "value" (id derived from a value hash).
// Existing tokens keep their state; missing ones are dropped.
func (p *Pool) SetTokens(lines []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool)
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		id, val := parseLine(ln)
		seen[id] = true
		if t, ok := p.byID[id]; ok {
			t.Value = val
			continue
		}
		t := &Token{ID: id, Value: val, Status: Unknown}
		p.byID[id] = t
		p.order = append(p.order, t)
	}
	kept := p.order[:0]
	for _, t := range p.order {
		if seen[t.ID] {
			kept = append(kept, t)
		} else {
			delete(p.byID, t.ID)
		}
	}
	p.order = kept
}

func parseLine(ln string) (id, val string) {
	if c := strings.IndexByte(ln, ','); c >= 0 {
		return strings.TrimSpace(ln[:c]), strings.TrimSpace(ln[c+1:])
	}
	h := fnv.New32a()
	h.Write([]byte(ln))
	return "t" + strconv.FormatUint(uint64(h.Sum32()), 16), ln
}

// Take returns an available token per the rotation policy and puts it on
// cooldown. It returns false if none are available.
func (p *Pool) Take() (Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var cand []*Token
	for _, t := range p.order {
		if t.Status != Dead && !now.Before(t.CoolingUntil) {
			cand = append(cand, t)
		}
	}
	if len(cand) == 0 {
		return Lease{}, false
	}
	var pick *Token
	switch p.rotation {
	case "round_robin":
		pick = p.roundRobin(cand)
	case "random":
		pick = cand[p.rng.Intn(len(cand))]
	default: // lru
		pick = cand[0]
		for _, t := range cand[1:] {
			if t.LastUsed.Before(pick.LastUsed) {
				pick = t
			}
		}
	}
	pick.LastUsed = now
	pick.CoolingUntil = now.Add(p.cooldown)
	return Lease{ID: pick.ID, Value: pick.Value}, true
}

// roundRobin advances the cursor to the next candidate in insertion order.
func (p *Pool) roundRobin(cand []*Token) *Token {
	n := len(p.order)
	in := make(map[*Token]bool, len(cand))
	for _, c := range cand {
		in[c] = true
	}
	for i := 1; i <= n; i++ {
		idx := (p.rrCursor + i) % n
		if t := p.order[idx]; in[t] {
			p.rrCursor = idx
			return t
		}
	}
	return cand[0]
}

// Release returns a token. ok=false marks it dead and starts a cooldown.
func (p *Pool) Release(id string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byID[id]
	if t == nil {
		return
	}
	if !ok {
		t.Fails++
		t.Status = Dead
		t.CoolingUntil = p.now().Add(p.cooldown)
	}
}

// MarkChecked records a health-check outcome. A live result revives a token.
func (p *Pool) MarkChecked(id string, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byID[id]
	if t == nil {
		return
	}
	t.LastCheck = p.now()
	if live {
		t.Status = Live
		t.Fails = 0
	} else {
		t.Status = Dead
	}
}

// SetValue updates a token's value (used after a refresh) and revives it.
func (p *Pool) SetValue(id, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.byID[id]; t != nil {
		t.Value = value
		t.Status = Live
		t.Fails = 0
	}
}

// Entries returns a snapshot of id/value pairs for iteration (e.g. rechecks).
func (p *Pool) Entries() []Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Lease, len(p.order))
	for i, t := range p.order {
		out[i] = Lease{ID: t.ID, Value: t.Value}
	}
	return out
}

// TokenView is a persistence/reporting projection of a Token.
type TokenView struct {
	ID        string    `json:"id"`
	Status    Status    `json:"status"`
	LastCheck time.Time `json:"last_check"`
	LastUsed  time.Time `json:"last_used"`
	Cooling   bool      `json:"cooling"`
	Fails     int       `json:"fails"`
}

// Snapshot is an aggregate + per-token view of the pool.
type Snapshot struct {
	Total   int         `json:"total"`
	Live    int         `json:"live"`
	Dead    int         `json:"dead"`
	Unknown int         `json:"unknown"`
	Cooling int         `json:"cooling"`
	Tokens  []TokenView `json:"tokens"`
}

// Status returns aggregate counters and a per-token view.
func (p *Pool) Status() Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	s := Snapshot{Total: len(p.order)}
	for _, t := range p.order {
		cooling := now.Before(t.CoolingUntil)
		if cooling {
			s.Cooling++
		}
		switch t.Status {
		case Live:
			s.Live++
		case Dead:
			s.Dead++
		default:
			s.Unknown++
		}
		s.Tokens = append(s.Tokens, TokenView{
			ID: t.ID, Status: t.Status, LastCheck: t.LastCheck,
			LastUsed: t.LastUsed, Cooling: cooling, Fails: t.Fails,
		})
	}
	return s
}

// Save atomically writes token state (without values) to the state file.
func (p *Pool) Save() error {
	p.mu.Lock()
	sf := p.stateFile
	snap := make([]*Token, len(p.order))
	copy(snap, p.order)
	p.mu.Unlock()
	if sf == "" {
		return nil
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := sf + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, sf)
}

// LoadState applies persisted state onto the current tokens, matched by id.
// Call after SetTokens so values come from the tokens file.
func (p *Pool) LoadState() error {
	if p.stateFile == "" {
		return nil
	}
	b, err := os.ReadFile(p.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var saved []Token
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range saved {
		s := &saved[i]
		if t, ok := p.byID[s.ID]; ok {
			t.Status = s.Status
			t.LastCheck = s.LastCheck
			t.LastUsed = s.LastUsed
			t.CoolingUntil = s.CoolingUntil
			t.Fails = s.Fails
		}
	}
	return nil
}
