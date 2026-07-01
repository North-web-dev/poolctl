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
