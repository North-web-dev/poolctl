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
	Unknown     Status = "unknown"     // not yet checked
	Live        Status = "live"        // last check passed
	Dead        Status = "dead"        // last check or use failed
	Quarantined Status = "quarantined" // pulled after a hard auth failure (401/403)
)

// Token holds a value and its runtime state. Value is never persisted.
type Token struct {
	ID           string    `json:"id"`
	Value        string    `json:"-"`
	Weight       int       `json:"weight"`
	Status       Status    `json:"status"`
	LastCheck    time.Time `json:"last_check"`
	LastUsed     time.Time `json:"last_used"`
	CoolingUntil time.Time `json:"cooling_until"`
	Fails        int       `json:"fails"`
	Requests     int64     `json:"requests"`
	Errors       int64     `json:"errors"`
	LastStatus   int       `json:"last_status"`
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
		id, val, weight := parseLine(ln)
		seen[id] = true
		if t, ok := p.byID[id]; ok {
			t.Value = val
			t.Weight = weight
			continue
		}
		t := &Token{ID: id, Value: val, Weight: weight, Status: Unknown}
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

// parseLine reads "id,value", "id,value,weight", or a bare "value" (id derived
// from a value hash). A trailing integer field is taken as the token's weight
// for weighted rotation; anything else is folded back into the value, so values
// containing commas still round-trip.
func parseLine(ln string) (id, val string, weight int) {
	weight = 1
	parts := strings.Split(ln, ",")
	if len(parts) >= 3 {
		if w, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1])); err == nil {
			if w > 0 {
				weight = w
			}
			parts = parts[:len(parts)-1]
		}
	}
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(strings.Join(parts[1:], ",")), weight
	}
	h := fnv.New32a()
	h.Write([]byte(ln))
	return "t" + strconv.FormatUint(uint64(h.Sum32()), 16), ln, weight
}

// Take returns an available token per the rotation policy and puts it on
// cooldown. It returns false if none are available.
func (p *Pool) Take() (Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var cand []*Token
	for _, t := range p.order {
		if t.Status != Dead && t.Status != Quarantined && !now.Before(t.CoolingUntil) {
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
	case "weighted":
		pick = p.weighted(cand)
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

// weighted picks a candidate with probability proportional to its weight, so
// higher-quota keys carry more traffic. Weights below 1 count as 1.
func (p *Pool) weighted(cand []*Token) *Token {
	total := 0
	for _, t := range cand {
		if t.Weight < 1 {
			total++
		} else {
			total += t.Weight
		}
	}
	r := p.rng.Intn(total)
	for _, t := range cand {
		w := t.Weight
		if w < 1 {
			w = 1
		}
		if r < w {
			return t
		}
		r -= w
	}
	return cand[len(cand)-1]
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

// Penalize keeps a token alive but rests it for d (on top of any current
// cooldown). Use for soft failures such as 429 or 5xx, where the key is fine
// but should back off briefly.
func (p *Pool) Penalize(id string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byID[id]
	if t == nil {
		return
	}
	t.Fails++
	until := p.now().Add(d)
	if until.After(t.CoolingUntil) {
		t.CoolingUntil = until
	}
}

// Quarantine pulls a token out of rotation for d after a hard failure (e.g. a
// 401/403 auth rejection). A subsequent passing health check revives it.
func (p *Pool) Quarantine(id string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byID[id]
	if t == nil {
		return
	}
	t.Fails++
	t.Status = Quarantined
	t.CoolingUntil = p.now().Add(d)
}

// RecordUse accounts a request made with a token: it bumps the request count,
// records the last status, and counts anything >=400 (or 0 for a transport
// error) as an error.
func (p *Pool) RecordUse(id string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.byID[id]
	if t == nil {
		return
	}
	t.Requests++
	t.LastStatus = status
	if status == 0 || status >= 400 {
		t.Errors++
	}
}

// MarkChecked records a health-check outcome. A live result revives a token,
// including one in quarantine.
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
	} else if t.Status != Quarantined {
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
	ID         string    `json:"id"`
	Weight     int       `json:"weight"`
	Status     Status    `json:"status"`
	LastCheck  time.Time `json:"last_check"`
	LastUsed   time.Time `json:"last_used"`
	Cooling    bool      `json:"cooling"`
	Fails      int       `json:"fails"`
	Requests   int64     `json:"requests"`
	Errors     int64     `json:"errors"`
	LastStatus int       `json:"last_status"`
}

// Snapshot is an aggregate + per-token view of the pool.
type Snapshot struct {
	Total       int         `json:"total"`
	Live        int         `json:"live"`
	Dead        int         `json:"dead"`
	Unknown     int         `json:"unknown"`
	Quarantined int         `json:"quarantined"`
	Cooling     int         `json:"cooling"`
	Tokens      []TokenView `json:"tokens"`
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
		case Quarantined:
			s.Quarantined++
		default:
			s.Unknown++
		}
		s.Tokens = append(s.Tokens, TokenView{
			ID: t.ID, Weight: t.Weight, Status: t.Status, LastCheck: t.LastCheck,
			LastUsed: t.LastUsed, Cooling: cooling, Fails: t.Fails,
			Requests: t.Requests, Errors: t.Errors, LastStatus: t.LastStatus,
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
			t.Requests = s.Requests
			t.Errors = s.Errors
			t.LastStatus = s.LastStatus
		}
	}
	return nil
}
