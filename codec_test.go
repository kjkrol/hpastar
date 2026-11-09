package hpastar_test

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/kjkrol/hpastar"
	"github.com/kjkrol/hpastar/graph"
	"github.com/kjkrol/hpastar/internal/testmap"
)

// saved is s's hierarchy as Save writes it
func saved(t *testing.T, s *hpastar.Solver[int]) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := s.Save(&buf); err != nil {
		t.Fatalf("saving: %v", err)
	}
	return buf.Bytes()
}

// loaded is a Solver over g, opts, holding the hierarchy file holds
func loaded(t *testing.T, g graph.Implicit[int], file []byte,
	opts ...hpastar.Option) *hpastar.Solver[int] {
	t.Helper()
	s := hpastar.NewSolver(g, opts...)
	if err := s.Load(bytes.NewReader(file)); err != nil {
		t.Fatalf("loading: %v", err)
	}
	return s
}

// samePaths fails t where a and b find other paths between the pairs of m
func samePaths(t *testing.T, m *testmap.Map, a, b *hpastar.Solver[int], class int) {
	t.Helper()
	qa, qb := a.Search(), b.Search()
	qa.Class(class)
	qb.Class(class)
	pa, pb := make([]int, 4096), make([]int, 4096)
	for _, p := range m.Pairs(11, 40, 40, math.Inf(1)) {
		na, oka := qa.Path(p[0], p[1], pa)
		nb, okb := qb.Path(p[0], p[1], pb)
		if oka != okb || !slices.Equal(pa[:na], pb[:nb]) {
			t.Fatalf("from %d to %d the loaded hierarchy finds %d states (%v), the saved one %d "+
				"(%v), or other ones", p[0], p[1], nb, okb, na, oka)
		}
	}
	if st := qb.Stats(); st.Climbs == 0 {
		t.Fatalf("the loaded hierarchy climbed nothing: %+v", st)
	}
}

func TestSolver_LoadsTheHierarchyItSaved(t *testing.T) {
	m := testmap.New(5, 256, 0.05, false)
	s := hpastar.NewSolver(m.Graph())
	if err := s.Save(&bytes.Buffer{}); !errors.Is(err, hpastar.ErrNotReady) {
		t.Fatalf("saved before it was built: %v, want ErrNotReady", err)
	}
	s.Update(0)
	file := saved(t, s)
	l := loaded(t, m.Graph(), file)
	if !l.Ready() {
		t.Fatal("loaded, the hierarchy is not ready")
	}
	if diff := hpastar.GraphsDiffer(s, l); diff != "" {
		t.Fatalf("loaded against saved: %s", diff)
	}
	if got := hpastar.CountRebuilds(l); slices.Max(got.Whole) != 0 || got.Labelled != 0 {
		t.Errorf("loaded, the hierarchy built %+v, want nothing", got)
	}
	samePaths(t, m, s, l, 0)
	// a change after the load is built as the saved one builds it
	c := 100*256 + 100
	m.Share[c] = 0
	s.Changed(c)
	l.Changed(c)
	s.Update(0)
	l.Update(0)
	if diff := hpastar.GraphsDiffer(s, l); diff != "" {
		t.Fatalf("changed alike after the load: %s", diff)
	}
	if n, err := l.Dump(&bytes.Buffer{}); err != nil || n == 0 {
		t.Errorf("after a change built, the loaded hierarchy dumps %d clusters (%v), want some",
			n, err)
	}
}

func TestSolver_LoadsTheHierarchyOfEveryClassItSaved(t *testing.T) {
	m := testmap.New(6, 128, 0.05, false)
	bars := m.Bars(3, 3, 0.1, func(int, int) bool { return true })
	g := m.Barred(3, bars)
	s := hpastar.NewSolver(g, hpastar.WithClusterSide(16))
	s.Update(0)
	l := loaded(t, g, saved(t, s), hpastar.WithClusterSide(16))
	if diff := hpastar.GraphsDiffer(s, l); diff != "" {
		t.Fatalf("three classes loaded against saved: %s", diff)
	}
	for k := range 3 {
		samePaths(t, m, s, l, k)
	}
	// two states at a place, trees of sixteen bits
	layered := m.Layered(64, 20, 100)
	s = hpastar.NewSolver(layered)
	s.Update(0)
	l = loaded(t, layered, saved(t, s))
	if diff := hpastar.GraphsDiffer(s, l); diff != "" {
		t.Fatalf("a layered graph loaded against saved: %s", diff)
	}
	samePaths(t, m, s, l, 0)
}

func TestSolver_LoadsNothingOfAnotherHierarchyOrASpoiltFile(t *testing.T) {
	m := testmap.New(7, 128, 0.05, false)
	s := hpastar.NewSolver(m.Graph())
	s.Update(0)
	file := saved(t, s)
	other := hpastar.NewSolver(m.Graph(), hpastar.WithClusterSide(12))
	if err := other.Load(bytes.NewReader(file)); !errors.Is(err, hpastar.ErrMismatch) {
		t.Errorf("loaded into a hierarchy of another side: %v, want ErrMismatch", err)
	}
	g := m.Graph()
	g.Of = nil
	if err := hpastar.NewSolver(g).Load(bytes.NewReader(file)); !errors.Is(err, hpastar.ErrNoOf) {
		t.Errorf("loaded over a graph without Of: %v, want ErrNoOf", err)
	}
	for name, spoilt := range map[string][]byte{
		"cut short": file[:len(file)*2/3],
		"a byte turned": func() []byte {
			b := slices.Clone(file)
			b[len(b)/2] ^= 0x55
			return b
		}(),
		"no header":        file[:3],
		"the records gone": file[:4+10*8],
	} {
		l := hpastar.NewSolver(m.Graph())
		l.Update(777)
		before := hpastar.CountRebuilds(l)
		if err := l.Load(bytes.NewReader(spoilt)); !errors.Is(err, hpastar.ErrCorrupt) {
			t.Errorf("loaded a file %s: %v, want ErrCorrupt", name, err)
		}
		if l.Ready() || hpastar.CountRebuilds(l).Whole[0] != before.Whole[0] {
			t.Errorf("a file %s refused, the hierarchy stands otherwise than before", name)
		}
		l.Update(0)
		sameAsWhole(t, l, m.Graph())
	}
}

func TestSolver_LoadsTheDumpsAppendedToWhatItSaved(t *testing.T) {
	m := testmap.New(8, 256, 0.05, false)
	s := hpastar.NewSolver(m.Graph())
	s.Update(0)
	var file bytes.Buffer
	if err := s.Save(&file); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Dump(&file); err != nil || n != 0 {
		t.Fatalf("dumped %d clusters (%v) right after a save, want none", n, err)
	}
	base := file.Len()
	// two changes, the second built in small budgets, dumped as they are finished
	for i, c := range []int{40*256 + 47, 200*256 + 16} {
		m.Share[c] = 0
		s.Changed(c)
		budget := 0
		if i == 1 {
			budget = 5000
		}
		for left := 1; left > 0; {
			_, left = s.Update(budget)
			if _, err := s.Dump(&file); err != nil {
				t.Fatal(err)
			}
		}
	}
	if file.Len() <= base {
		t.Fatal("the dumps appended nothing")
	}
	l := loaded(t, m.Graph(), file.Bytes())
	if diff := hpastar.GraphsDiffer(s, l); diff != "" {
		t.Fatalf("the dumps loaded against the hierarchy that dumped them: %s", diff)
	}
	samePaths(t, m, s, l, 0)
	sameAsWhole(t, l, m.Graph())
	if n, err := s.Dump(&bytes.Buffer{}); err != nil || n != 0 {
		t.Errorf("dumped %d clusters (%v) with nothing built since, want none", n, err)
	}
}
