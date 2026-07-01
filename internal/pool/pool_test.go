package pool

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) add(d time.Duration) {
	c.t = c.t.Add(d)
}

func newPool(t *testing.T, rotation string, cooldown time.Duration, clk *clock, ids ...string) *Pool {
	t.Helper()
	p := New(Options{
		Rotation: rotation,
		Cooldown: cooldown,
		Now:      clk.now,
		Rand:     rand.New(rand.NewSource(1)),
	})
	lines := make([]string, len(ids))
	for i, id := range ids {
		lines[i] = id + "," + "val-" + id
	}
	p.SetTokens(lines)
	return p
}

func takeN(t *testing.T, p *Pool, clk *clock, n int, step time.Duration) []string {
	t.Helper()
	var got []string
	for i := 0; i < n; i++ {
		l, ok := p.Take()
		if !ok {
			t.Fatalf("take %d: no token available", i)
		}
		got = append(got, l.ID)
		clk.add(step)
	}
	return got
}

func TestRotationLRU(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "lru", 0, clk, "a", "b", "c")
	got := takeN(t, p, clk, 6, time.Second)
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lru order = %v, want %v", got, want)
		}
	}
}

func TestRotationRoundRobin(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "round_robin", 0, clk, "a", "b", "c")
	got := takeN(t, p, clk, 5, time.Second)
	want := []string{"a", "b", "c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round_robin order = %v, want %v", got, want)
		}
	}
}

func TestRotationWeighted(t *testing.T) {
	p := New(Options{Rotation: "weighted", Rand: rand.New(rand.NewSource(1))})
	p.SetTokens([]string{"heavy,val,9", "light,val,1"}) // no cooldown: both always available
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		l, ok := p.Take()
		if !ok {
			t.Fatal("weighted take should always succeed with cooldown 0")
		}
		counts[l.ID]++
	}
	if counts["heavy"] <= counts["light"]*3 {
		t.Fatalf("weighted skew off: heavy=%d light=%d", counts["heavy"], counts["light"])
	}
}

func TestQuarantineExcludesToken(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "lru", 0, clk, "a", "b")

	p.Quarantine("a", 5*time.Minute)
	if s := p.Status(); s.Quarantined != 1 {
		t.Fatalf("quarantined = %d, want 1", s.Quarantined)
	}
	for i := 0; i < 5; i++ {
		l, ok := p.Take()
		if !ok {
			t.Fatal("b should stay available")
		}
		if l.ID == "a" {
			t.Fatal("quarantined token a was handed out")
		}
		clk.add(time.Second)
	}
	// A passing health check revives it.
	p.MarkChecked("a", true)
	if s := p.Status(); s.Quarantined != 0 || s.Live != 1 {
		t.Fatalf("after revive: quarantined=%d live=%d", s.Quarantined, s.Live)
	}
}

func TestPenalizeKeepsTokenAlive(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "lru", 0, clk, "solo")

	p.Penalize("solo", 30*time.Second)
	if _, ok := p.Take(); ok {
		t.Fatal("penalized token should be cooling")
	}
	clk.add(31 * time.Second)
	if _, ok := p.Take(); !ok {
		t.Fatal("penalized token should return after cooldown, not be dead")
	}
	if s := p.Status(); s.Dead != 0 {
		t.Fatalf("penalize must not mark dead, got dead=%d", s.Dead)
	}
}

func TestRecordUseAccounting(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "lru", 0, clk, "a")

	p.RecordUse("a", 200)
	p.RecordUse("a", 500)
	p.RecordUse("a", 0) // transport error
	tok := p.Status().Tokens[0]
	if tok.Requests != 3 {
		t.Fatalf("requests = %d, want 3", tok.Requests)
	}
	if tok.Errors != 2 {
		t.Fatalf("errors = %d, want 2", tok.Errors)
	}
	if tok.LastStatus != 0 {
		t.Fatalf("last_status = %d, want 0", tok.LastStatus)
	}
}

func TestCooldownExcludesToken(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "lru", 30*time.Second, clk, "solo")

	if _, ok := p.Take(); !ok {
		t.Fatal("first take should succeed")
	}
	if _, ok := p.Take(); ok {
		t.Fatal("second take should fail while cooling")
	}
	clk.add(31 * time.Second)
	if _, ok := p.Take(); !ok {
		t.Fatal("take should succeed after cooldown")
	}
}

func TestReleaseFailMarksDead(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := newPool(t, "lru", 0, clk, "a", "b")

	l, _ := p.Take()
	p.Release(l.ID, false)

	s := p.Status()
	if s.Dead != 1 {
		t.Fatalf("dead = %d, want 1", s.Dead)
	}
	// Dead token must not be handed out again.
	for i := 0; i < 5; i++ {
		got, ok := p.Take()
		if !ok {
			t.Fatal("other token should remain available")
		}
		if got.ID == l.ID {
			t.Fatalf("dead token %s was handed out", l.ID)
		}
		clk.add(time.Second)
	}
}

func TestStatePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	a := New(Options{Cooldown: 30 * time.Second, StateFile: state, Now: clk.now, Rand: rand.New(rand.NewSource(1))})
	a.SetTokens([]string{"a,val-a", "b,val-b", "c,val-c"})
	a.MarkChecked("a", true)
	a.Release("b", false) // b -> dead
	a.Take()              // puts one token on cooldown
	if err := a.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	b := New(Options{Cooldown: 30 * time.Second, StateFile: state, Now: clk.now, Rand: rand.New(rand.NewSource(1))})
	b.SetTokens([]string{"a,val-a", "b,val-b", "c,val-c"})
	if err := b.LoadState(); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := b.Status()
	if s.Dead != 1 {
		t.Fatalf("restored dead = %d, want 1", s.Dead)
	}
	if s.Live != 1 {
		t.Fatalf("restored live = %d, want 1", s.Live)
	}
	// Value must survive (it comes from the tokens file, not the state file).
	for _, e := range b.Entries() {
		if e.Value == "" {
			t.Fatalf("token %s lost its value after reload", e.ID)
		}
	}
}

func TestSetTokensReconcile(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	p := New(Options{Now: clk.now, Rand: rand.New(rand.NewSource(1))})
	p.SetTokens([]string{"a,1", "b,2"})
	p.MarkChecked("a", true)

	p.SetTokens([]string{"a,1-new", "c,3"}) // b removed, c added, a kept
	s := p.Status()
	if s.Total != 2 {
		t.Fatalf("total = %d, want 2", s.Total)
	}
	if s.Live != 1 { // a keeps its live status
		t.Fatalf("live = %d, want 1", s.Live)
	}
	for _, e := range p.Entries() {
		if e.ID == "a" && e.Value != "1-new" {
			t.Fatalf("a value = %q, want 1-new", e.Value)
		}
	}
}
