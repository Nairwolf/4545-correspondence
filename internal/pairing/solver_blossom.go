// The matching algorithm in this file is a Go port of max_weight_matching
// from NetworkX (networkx/algorithms/matching.py), which is distributed
// under the following licence:
//
//   Copyright (c) 2004-2026, NetworkX Developers
//   Aric Hagberg <hagberg@lanl.gov>
//   Dan Schult <dschult@colgate.edu>
//   Pieter Swart <swart@lanl.gov>
//   All rights reserved.
//
//   Redistribution and use in source and binary forms, with or without
//   modification, are permitted provided that the following conditions are
//   met:
//
//     * Redistributions of source code must retain the above copyright
//       notice, this list of conditions and the following disclaimer.
//
//     * Redistributions in binary form must reproduce the above
//       copyright notice, this list of conditions and the following
//       disclaimer in the documentation and/or other materials provided
//       with the distribution.
//
//     * Neither the name of the NetworkX Developers nor the names of its
//       contributors may be used to endorse or promote products derived
//       from this software without specific prior written permission.
//
//   THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
//   "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
//   LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
//   A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
//   OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
//   SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
//   LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
//   DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
//   THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
//   (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
//   OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package pairing

import (
	"cmp"
	"fmt"
	"slices"
)

// Blossom is §6.2 step 4's preferred solver: of every way to pair all
// the slots, it returns one with the lowest total cost. Where Greedy
// can pair the top of the list well and leave an avoidable rematch at
// the bottom, Blossom never does.
//
// It runs Edmonds' blossom algorithm, as described by Zvi Galil in
// "Efficient Algorithms for Finding Maximum Matching in Graphs" (ACM
// Computing Surveys, 1986), in O(n³). That algorithm finds the pairing
// with the highest total *weight* among those that pair as many slots
// as possible, so each edge is weighted M − cost with M above every
// cost. Every perfect matching has the same number of edges, so the
// highest total weight is exactly the lowest total cost. Costs are
// integers, so every step is exact.
//
// The terms in the comments below (S- and T-labels, blossoms, dual
// variables, slack) are Galil's; the paper is needed to follow the
// algorithm itself, not to use it.
type Blossom struct{}

func (Blossom) Match(g *Graph) [][2]int {
	m := newMatcher(g)
	m.run()

	pairs := make([][2]int, 0, g.N()/2)
	for v, w := range m.mate {
		if v < w {
			pairs = append(pairs, [2]int{v, w})
		}
	}
	if len(pairs) != g.N()/2 {
		// Unreachable for the same reason as in Greedy: every two
		// distinct players have an edge, so a perfect matching exists.
		panic("pairing: blossom: no perfect matching")
	}
	return pairs
}

// edge is an ordered pair of vertices (slot indices); the order carries
// meaning in the labels and in a blossom's connecting edges.
type edge struct{ v, w int }

var noEdge = edge{-1, -1}

// Labels of a top-level blossom during a stage.
const (
	labelNone = 0
	labelS    = 1
	labelT    = 2
	// breadcrumb marks an S-blossom already visited by scanBlossom.
	breadcrumb = 4
)

// blossom is either a single vertex (a trivial blossom) or an odd cycle
// of sub-blossoms. NetworkX keeps these fields in dictionaries keyed by
// vertex or blossom; a vertex and its trivial blossom share one key,
// which here is one *blossom.
type blossom struct {
	vertex int // the vertex, or -1 for a non-trivial blossom

	// childs are the sub-blossoms, starting with the base and going
	// round the cycle; edges[i] connects a vertex in childs[i] to one
	// in childs[i+1] (wrapping). Non-trivial blossoms only.
	childs []*blossom
	edges  []edge
	// myBestEdges, once computed (hasMyBestEdges), are the least-slack
	// edges from this S-blossom to each neighbouring S-blossom.
	myBestEdges    []edge
	hasMyBestEdges bool

	parent *blossom // nil for a top-level blossom
	base   int      // base vertex
	dual   int      // z(b), non-trivial blossoms only
	dead   bool     // expanded and removed

	label     int
	labelEdge edge // the edge through which the label was obtained
	bestEdge  edge // least-slack edge used for delta2 / delta3
}

func (b *blossom) trivial() bool { return b.vertex >= 0 }

// leaves returns the vertices inside b.
func (b *blossom) leaves() []int {
	var out []int
	stack := slices.Clone(b.childs)
	for len(stack) > 0 {
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if t.trivial() {
			out = append(out, t.vertex)
		} else {
			stack = append(stack, t.childs...)
		}
	}
	return out
}

// childAt and edgeAt index round the cycle, accepting the negative
// indexes the algorithm walks backwards with.
func (b *blossom) childAt(i int) *blossom { return b.childs[wrap(i, len(b.childs))] }
func (b *blossom) edgeAt(i int) edge      { return b.edges[wrap(i, len(b.edges))] }

func wrap(i, n int) int { return ((i % n) + n) % n }

type matcher struct {
	n      int
	weight []int  // n×n, M − cost; meaningful only for edges
	edges  []edge // every edge once, v < w, in the §6.2 step 4 order
	adj    [][]int

	vertices  []*blossom // each vertex's trivial blossom
	blossoms  []*blossom // live non-trivial blossoms, oldest first
	inBlossom []*blossom // each vertex's top-level blossom
	mate      []int      // -1 while single
	dual      []int      // 2·u(v), doubled so that it stays an integer
	allowEdge []bool     // n×n: known to have zero slack this stage
	queue     []int      // newly discovered S-vertices
}

// newMatcher builds the weighted graph. Edges are listed, and each
// vertex's neighbours scanned, in §6.2 step 4's total order — cost,
// then lower id, then higher id, then slot index for the volunteer's
// two slots — so that among equally cheap matchings the one returned
// depends only on the graph, never on the order it was built in.
func newMatcher(g *Graph) *matcher {
	n := g.N()
	m := &matcher{
		n:         n,
		weight:    make([]int, n*n),
		adj:       make([][]int, n),
		vertices:  make([]*blossom, n),
		inBlossom: make([]*blossom, n),
		mate:      make([]int, n),
		dual:      make([]int, n),
		allowEdge: make([]bool, n*n),
	}

	maxCost := 0
	for i := range n {
		for j := i + 1; j < n; j++ {
			if g.Allowed(i, j) {
				m.edges = append(m.edges, edge{i, j})
				maxCost = max(maxCost, g.Cost(i, j))
			}
		}
	}
	slices.SortFunc(m.edges, func(a, b edge) int {
		if c := cmp.Compare(g.Cost(a.v, a.w), g.Cost(b.v, b.w)); c != 0 {
			return c
		}
		alo, ahi := orderedIDs(g, a)
		blo, bhi := orderedIDs(g, b)
		if c := cmp.Compare(alo, blo); c != 0 {
			return c
		}
		if c := cmp.Compare(ahi, bhi); c != 0 {
			return c
		}
		if c := cmp.Compare(a.v, b.v); c != 0 {
			return c
		}
		return cmp.Compare(a.w, b.w)
	})

	maxWeight := 0
	for _, e := range m.edges {
		w := maxCost + 1 - g.Cost(e.v, e.w)
		m.weight[e.v*n+e.w], m.weight[e.w*n+e.v] = w, w
		maxWeight = max(maxWeight, w)
		m.adj[e.v] = append(m.adj[e.v], e.w)
		m.adj[e.w] = append(m.adj[e.w], e.v)
	}

	for v := range n {
		b := &blossom{vertex: v, base: v, labelEdge: noEdge, bestEdge: noEdge}
		m.vertices[v], m.inBlossom[v] = b, b
		m.mate[v] = -1
		m.dual[v] = maxWeight
	}
	return m
}

func orderedIDs(g *Graph, e edge) (lo, hi string) {
	a, b := g.Slots[e.v].ID, g.Slots[e.w].ID
	return min(a, b), max(a, b)
}

// slack returns 2 × the slack of edge (v, w); not valid inside blossoms.
func (m *matcher) slack(e edge) int {
	return m.dual[e.v] + m.dual[e.w] - 2*m.weight[e.v*m.n+e.w]
}

func (m *matcher) allowed(v, w int) bool { return m.allowEdge[v*m.n+w] }

func (m *matcher) allow(v, w int) {
	m.allowEdge[v*m.n+w], m.allowEdge[w*m.n+v] = true, true
}

// assignLabel gives label t to the top-level blossom containing vertex
// w, reached through an edge from vertex v (-1 when w is single).
func (m *matcher) assignLabel(w, t, v int) {
	b, wb := m.inBlossom[w], m.vertices[w]
	wb.label, b.label = t, t
	e := noEdge
	if v >= 0 {
		e = edge{v, w}
	}
	wb.labelEdge, b.labelEdge = e, e
	wb.bestEdge, b.bestEdge = noEdge, noEdge

	switch t {
	case labelS:
		// b became an S-blossom: its vertices go on the queue.
		if b.trivial() {
			m.queue = append(m.queue, b.vertex)
		} else {
			m.queue = append(m.queue, b.leaves()...)
		}
	case labelT:
		// b became a T-blossom: its base's mate becomes S.
		base := b.base
		m.assignLabel(m.mate[base], labelS, base)
	}
}

// scanBlossom traces back from v and w to find either a new blossom,
// whose base vertex it returns, or an augmenting path (-1).
func (m *matcher) scanBlossom(v, w int) int {
	var path []*blossom
	base := -1
	for v != -1 {
		b := m.inBlossom[v]
		if b.label&breadcrumb != 0 {
			base = b.base
			break
		}
		path = append(path, b)
		b.label = labelS | breadcrumb
		if b.labelEdge == noEdge {
			// The base of b is single: this path ends here.
			v = -1
		} else {
			// Step back through b's mate, a T-blossom, to its S-parent.
			v = b.labelEdge.v
			v = m.inBlossom[v].labelEdge.v
		}
		// Alternate between the two paths.
		if w != -1 {
			v, w = w, v
		}
	}
	for _, b := range path {
		b.label = labelS
	}
	return base
}

// addBlossom makes a new S-blossom with the given base out of the cycle
// through S-vertices v and w, with a zero dual variable.
func (m *matcher) addBlossom(base, v, w int) {
	bb, bv, bw := m.inBlossom[base], m.inBlossom[v], m.inBlossom[w]
	b := &blossom{vertex: -1, base: base, labelEdge: noEdge, bestEdge: noEdge}
	bb.parent = b

	childs := []*blossom{}
	edges := []edge{{v, w}}
	// Trace back from v to the base.
	for bv != bb {
		bv.parent = b
		childs = append(childs, bv)
		edges = append(edges, bv.labelEdge)
		bv = m.inBlossom[bv.labelEdge.v]
	}
	childs = append(childs, bb)
	slices.Reverse(childs)
	slices.Reverse(edges)
	// Trace back from w to the base.
	for bw != bb {
		bw.parent = b
		childs = append(childs, bw)
		edges = append(edges, edge{bw.labelEdge.w, bw.labelEdge.v})
		bw = m.inBlossom[bw.labelEdge.v]
	}
	b.childs, b.edges = childs, edges
	b.label = labelS
	b.labelEdge = bb.labelEdge
	m.blossoms = append(m.blossoms, b)

	// T-vertices inside become S-vertices and join the queue.
	for _, lv := range b.leaves() {
		if m.inBlossom[lv].label == labelT {
			m.queue = append(m.queue, lv)
		}
		m.inBlossom[lv] = b
	}

	// Least-slack edges from b to each neighbouring S-blossom, keyed by
	// that blossom; the map is only looked up, the slice keeps order.
	index := map[*blossom]int{}
	var best []edge
	for _, sub := range childs {
		var candidates []edge
		switch {
		case sub.trivial():
			for _, nw := range m.adj[sub.vertex] {
				candidates = append(candidates, edge{sub.vertex, nw})
			}
		case sub.hasMyBestEdges:
			candidates = sub.myBestEdges
			sub.myBestEdges, sub.hasMyBestEdges = nil, false
		default:
			for _, lv := range sub.leaves() {
				for _, nw := range m.adj[lv] {
					candidates = append(candidates, edge{lv, nw})
				}
			}
		}
		for _, k := range candidates {
			i, j := k.v, k.w
			if m.inBlossom[j] == b {
				i, j = j, i
			}
			bj := m.inBlossom[j]
			if bj == b || bj.label != labelS {
				continue
			}
			if at, ok := index[bj]; !ok {
				index[bj] = len(best)
				best = append(best, k)
			} else if m.slack(edge{i, j}) < m.slack(best[at]) {
				best[at] = k
			}
		}
		sub.bestEdge = noEdge
	}
	b.myBestEdges, b.hasMyBestEdges = best, true

	var bestSlack int
	for _, k := range best {
		if s := m.slack(k); b.bestEdge == noEdge || s < bestSlack {
			b.bestEdge, bestSlack = k, s
		}
	}
}

// expandBlossom turns b's sub-blossoms back into top-level blossoms.
// At the end of a stage, sub-blossoms with a zero dual are expanded
// too. Mid-stage, an expanding T-blossom's sub-blossoms are relabelled.
func (m *matcher) expandBlossom(b *blossom, endStage bool) {
	for _, s := range b.childs {
		s.parent = nil
		switch {
		case s.trivial():
			m.inBlossom[s.vertex] = s
		case endStage && s.dual == 0:
			m.expandBlossom(s, endStage)
		default:
			for _, lv := range s.leaves() {
				m.inBlossom[lv] = s
			}
		}
	}

	if !endStage && b.label == labelT {
		// Start at the sub-blossom through which b obtained its label,
		// and relabel sub-blossoms until the base is reached.
		entryChild := m.inBlossom[b.labelEdge.w]
		j := slices.Index(b.childs, entryChild)
		jstep := -1 // even start index: go backward
		if j&1 != 0 {
			// Odd start index: go forward and wrap.
			j -= len(b.childs)
			jstep = 1
		}
		v, w := b.labelEdge.v, b.labelEdge.w
		for j != 0 {
			// Relabel the T-sub-blossom.
			var p, q int
			if jstep == 1 {
				e := b.edgeAt(j)
				p, q = e.v, e.w
			} else {
				e := b.edgeAt(j - 1)
				q, p = e.v, e.w
			}
			m.vertices[w].label = labelNone
			m.vertices[q].label = labelNone
			m.assignLabel(w, labelT, v)
			// Step to the next S-sub-blossom and note its forward edge.
			m.allow(p, q)
			j += jstep
			if jstep == 1 {
				e := b.edgeAt(j)
				v, w = e.v, e.w
			} else {
				e := b.edgeAt(j - 1)
				w, v = e.v, e.w
			}
			// Step to the next T-sub-blossom.
			m.allow(v, w)
			j += jstep
		}
		// Relabel the base T-sub-blossom without stepping through to
		// its mate, so not through assignLabel.
		bw := b.childAt(j)
		m.vertices[w].label, bw.label = labelT, labelT
		m.vertices[w].labelEdge, bw.labelEdge = edge{v, w}, edge{v, w}
		bw.bestEdge = noEdge
		// Continue round the blossom until back at entryChild.
		j += jstep
		for b.childAt(j) != entryChild {
			bv := b.childAt(j)
			if bv.label == labelS {
				// Labelled S through one of its neighbours meanwhile.
				j += jstep
				continue
			}
			// A sub-blossom holding a vertex reachable from an
			// S-vertex outside b is labelled T.
			var lv int
			if bv.trivial() {
				lv = bv.vertex
			} else {
				for _, lv = range bv.leaves() {
					if m.vertices[lv].label != labelNone {
						break
					}
				}
			}
			if m.vertices[lv].label != labelNone {
				m.vertices[lv].label = labelNone
				m.vertices[m.mate[bv.base]].label = labelNone
				m.assignLabel(lv, labelT, m.vertices[lv].labelEdge.v)
			}
			j += jstep
		}
	}

	b.dead = true
	m.blossoms = slices.DeleteFunc(m.blossoms, func(x *blossom) bool { return x == b })
}

// augmentBlossom swaps matched and unmatched edges along the
// alternating path through b from vertex v to b's base, which v then
// becomes.
func (m *matcher) augmentBlossom(b *blossom, v int) {
	// The immediate sub-blossom of b that holds v.
	t := m.vertices[v]
	for t.parent != b {
		t = t.parent
	}
	if !t.trivial() {
		m.augmentBlossom(t, v)
	}

	i := slices.Index(b.childs, t)
	j, jstep := i, -1 // even start index: go backward
	if i&1 != 0 {
		// Odd start index: go forward and wrap.
		j -= len(b.childs)
		jstep = 1
	}
	for j != 0 {
		// Step to the next sub-blossom and augment it.
		j += jstep
		t = b.childAt(j)
		var w, x int
		if jstep == 1 {
			e := b.edgeAt(j)
			w, x = e.v, e.w
		} else {
			e := b.edgeAt(j - 1)
			x, w = e.v, e.w
		}
		if !t.trivial() {
			m.augmentBlossom(t, w)
		}
		// And the one after it.
		j += jstep
		t = b.childAt(j)
		if !t.trivial() {
			m.augmentBlossom(t, x)
		}
		// Match the edge connecting the two.
		m.mate[w], m.mate[x] = x, w
	}
	// Rotate the cycle so that the new base comes first.
	b.childs = append(slices.Clone(b.childs[i:]), b.childs[:i]...)
	b.edges = append(slices.Clone(b.edges[i:]), b.edges[:i]...)
	b.base = b.childs[0].base
}

// augmentMatching swaps matched and unmatched edges along the
// augmenting path through S-vertices v and w, which grows the matching
// by one edge.
func (m *matcher) augmentMatching(v, w int) {
	for _, start := range [2]edge{{v, w}, {w, v}} {
		// Match s to j, then trace back from s to a single vertex,
		// swapping edges on the way.
		s, j := start.v, start.w
		for {
			bs := m.inBlossom[s]
			if !bs.trivial() {
				m.augmentBlossom(bs, s)
			}
			m.mate[s] = j
			if bs.labelEdge == noEdge {
				break // reached a single vertex
			}
			bt := m.inBlossom[bs.labelEdge.v]
			s, j = bt.labelEdge.v, bt.labelEdge.w
			if !bt.trivial() {
				m.augmentBlossom(bt, j)
			}
			m.mate[j] = s
		}
	}
}

// run is the main loop. Each stage finds one augmenting path and grows
// the matching by one edge; when no path exists under the current dual
// variables, a substage adjusts them (the "delta" step) to make more
// edges usable. It always looks for a maximum-cardinality matching, so
// NetworkX's delta1 never applies.
func (m *matcher) run() {
	for {
		// A new stage: forget the previous stage's labels and edges.
		for _, b := range m.vertices {
			b.label, b.labelEdge, b.bestEdge = labelNone, noEdge, noEdge
		}
		for _, b := range m.blossoms {
			b.label, b.labelEdge, b.bestEdge = labelNone, noEdge, noEdge
			b.myBestEdges, b.hasMyBestEdges = nil, false
		}
		clear(m.allowEdge)
		m.queue = m.queue[:0]

		// Single vertices start as S-vertices.
		for v := range m.n {
			if m.mate[v] == -1 && m.inBlossom[v].label == labelNone {
				m.assignLabel(v, labelS, -1)
			}
		}

		augmented := false
		for {
			m.labelReachable(&augmented)
			if augmented {
				break
			}
			if optimum := m.adjustDuals(); optimum {
				break
			}
		}

		if !augmented {
			return
		}

		// End of stage: expand top-level S-blossoms with a zero dual.
		for _, b := range slices.Clone(m.blossoms) {
			if !b.dead && b.parent == nil && b.label == labelS && b.dual == 0 {
				m.expandBlossom(b, true)
			}
		}
	}
}

// labelReachable labels every vertex reachable through an alternating
// path of zero-slack edges, until the queue is empty or an augmenting
// path has been found and used.
func (m *matcher) labelReachable(augmented *bool) {
	for len(m.queue) > 0 && !*augmented {
		v := m.queue[len(m.queue)-1]
		m.queue = m.queue[:len(m.queue)-1]

		for _, w := range m.adj[v] {
			bv, bw := m.inBlossom[v], m.inBlossom[w]
			if bv == bw {
				continue // internal to a blossom
			}
			var kslack int
			if !m.allowed(v, w) {
				kslack = m.slack(edge{v, w})
				if kslack <= 0 {
					m.allow(v, w)
				}
			}
			switch {
			case m.allowed(v, w):
				switch {
				case bw.label == labelNone:
					// w is free: label it T, and its mate S.
					m.assignLabel(w, labelT, v)
				case bw.label == labelS:
					// Two S-blossoms meet: a new blossom, or a path.
					if base := m.scanBlossom(v, w); base != -1 {
						m.addBlossom(base, v, w)
					} else {
						m.augmentMatching(v, w)
						*augmented = true
						return
					}
				case m.vertices[w].label == labelNone:
					// w is inside a T-blossom and now reached from
					// outside it; remember how, for a later expansion.
					m.vertices[w].label = labelT
					m.vertices[w].labelEdge = edge{v, w}
				}
			case bw.label == labelS:
				// Least-slack edge to a different S-blossom.
				if bv.bestEdge == noEdge || kslack < m.slack(bv.bestEdge) {
					bv.bestEdge = edge{v, w}
				}
			case m.vertices[w].label == labelNone:
				// Least-slack edge reaching a free vertex (or one not
				// yet reached inside a T-blossom).
				wb := m.vertices[w]
				if wb.bestEdge == noEdge || kslack < m.slack(wb.bestEdge) {
					wb.bestEdge = edge{v, w}
				}
			}
		}
	}
}

// adjustDuals finds the smallest change to the dual variables that
// makes progress possible, applies it and acts on it. It reports true
// when no further progress is possible: the optimum has been reached.
func (m *matcher) adjustDuals() (optimum bool) {
	deltaType := -1
	var delta int
	var deltaEdge edge
	var deltaBlossom *blossom

	// delta2: least slack on an edge from an S-vertex to a free vertex.
	for v := range m.n {
		if e := m.vertices[v].bestEdge; m.inBlossom[v].label == labelNone && e != noEdge {
			if d := m.slack(e); deltaType == -1 || d < delta {
				delta, deltaType, deltaEdge = d, 2, e
			}
		}
	}

	// delta3: half the least slack on an edge between two S-blossoms.
	// Vertices first, then blossoms oldest first: NetworkX's order.
	delta3 := func(b *blossom) {
		if b.parent != nil || b.label != labelS || b.bestEdge == noEdge {
			return
		}
		if d := m.slack(b.bestEdge) / 2; deltaType == -1 || d < delta {
			delta, deltaType, deltaEdge = d, 3, b.bestEdge
		}
	}
	for _, b := range m.vertices {
		delta3(b)
	}
	for _, b := range m.blossoms {
		delta3(b)
	}

	// delta4: the least dual variable of a top-level T-blossom.
	for _, b := range m.blossoms {
		if b.parent == nil && b.label == labelT && (deltaType == -1 || b.dual < delta) {
			delta, deltaType, deltaBlossom = b.dual, 4, b
		}
	}

	if deltaType == -1 {
		// No further improvement possible: a final update makes the
		// optimum verifiable.
		deltaType = 1
		delta = max(0, slices.Min(m.dual))
	}

	for v := range m.n {
		switch m.inBlossom[v].label {
		case labelS:
			m.dual[v] -= delta
		case labelT:
			m.dual[v] += delta
		}
	}
	for _, b := range m.blossoms {
		if b.parent != nil {
			continue
		}
		switch b.label {
		case labelS:
			b.dual += delta
		case labelT:
			b.dual -= delta
		}
	}

	switch deltaType {
	case 1:
		return true
	case 2, 3:
		// Continue the search along the least-slack edge.
		m.allow(deltaEdge.v, deltaEdge.w)
		m.queue = append(m.queue, deltaEdge.v)
	case 4:
		m.expandBlossom(deltaBlossom, false)
	}
	return false
}

// verifyOptimum checks the linear-programming certificate that the
// matching is optimal: the duals are feasible, and every matched edge
// and every blossom with a positive dual is tight. A pure check, used
// by the tests on pools far too large for brute force.
func (m *matcher) verifyOptimum() error {
	// With maximum cardinality, vertex duals may go negative; shift
	// them all by one constant.
	offset := max(0, -slices.Min(m.dual))
	for _, b := range m.blossoms {
		if b.dual < 0 {
			return fmt.Errorf("blossom with negative dual %d", b.dual)
		}
	}
	for _, e := range m.edges {
		i, j := e.v, e.w
		s := m.dual[i] + m.dual[j] - 2*m.weight[i*m.n+j]
		iChain, jChain := m.ancestors(i), m.ancestors(j)
		for k := 0; k < min(len(iChain), len(jChain)) && iChain[k] == jChain[k]; k++ {
			s += 2 * iChain[k].dual
		}
		if s < 0 {
			return fmt.Errorf("edge (%d, %d) has negative slack %d", i, j, s)
		}
		if m.mate[i] == j || m.mate[j] == i {
			if m.mate[i] != j || m.mate[j] != i {
				return fmt.Errorf("edge (%d, %d) is matched one way only", i, j)
			}
			if s != 0 {
				return fmt.Errorf("matched edge (%d, %d) has slack %d", i, j, s)
			}
		}
	}
	for v := range m.n {
		if m.mate[v] == -1 && m.dual[v]+offset != 0 {
			return fmt.Errorf("single vertex %d has dual %d", v, m.dual[v]+offset)
		}
	}
	for _, b := range m.blossoms {
		if b.dual <= 0 {
			continue
		}
		if len(b.edges)%2 != 1 {
			return fmt.Errorf("blossom with positive dual has %d edges", len(b.edges))
		}
		for k := 1; k < len(b.edges); k += 2 {
			if e := b.edges[k]; m.mate[e.v] != e.w || m.mate[e.w] != e.v {
				return fmt.Errorf("blossom with positive dual is not full at edge (%d, %d)", e.v, e.w)
			}
		}
	}
	return nil
}

// ancestors returns v's enclosing blossoms, outermost first, ending
// with v's own trivial blossom.
func (m *matcher) ancestors(v int) []*blossom {
	var chain []*blossom
	for b := m.vertices[v]; b != nil; b = b.parent {
		chain = append(chain, b)
	}
	slices.Reverse(chain)
	return chain
}
