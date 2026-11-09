package hierarchy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
)

// The file of a hierarchy: a header of what the hierarchy is laid over, then records to the
// end, each a cluster of one level as every class sees it or the labels of one class, a later
// record of the same standing in for an earlier one, so a file saved whole takes the records a
// Dump appends. Every number is little-endian; a record carries its length and a CRC-32 of its
// body, so a file cut short or spoilt is told from a sound one.

var (
	// ErrNotReady is a Save asked while a cluster is stale or the labels are being worked out.
	ErrNotReady = errors.New("hpastar: the hierarchy is not ready to be saved")
	// ErrMismatch is a Load of a file saved of a hierarchy of another shape or over another
	// graph.
	ErrMismatch = errors.New("hpastar: the file holds a hierarchy of another shape")
	// ErrNoOf is a Load over a graph without Of, which stands no state again.
	ErrNoOf = errors.New("hpastar: a load needs the graph's Of")
	// ErrCorrupt is a file that does not read as one saved.
	ErrCorrupt = errors.New("hpastar: the file does not read as a hierarchy saved")
)

var magic = [4]byte{'H', 'P', 'A', '1'}

const (
	recordCluster byte = 1
	recordLabels  byte = 2
)

// header is what a hierarchy is laid over, which a file must match to be loaded into it
type header struct {
	Side, Factor, Top, Span, Trees int64
	Near                           float64
	Width, Height, Size, Classes   int64
}

func (h *Hierarchy[T]) header() header {
	c := h.config
	return header{Side: int64(c.Side), Factor: int64(c.Factor), Top: int64(c.Top),
		Span: int64(c.Span), Trees: int64(c.Trees), Near: c.Near, Width: int64(h.w),
		Height: int64(h.h), Size: int64(h.graph.Size), Classes: int64(len(h.views))}
}

// Save writes h whole to w: its header, every cluster of every level and every class's
// labels. It fails with ErrNotReady while h is not Ready. What it writes is what was built since
// Dump last wrote; both leave nothing to dump.
func (h *Hierarchy[T]) Save(w io.Writer) error {
	if !h.Ready() {
		return ErrNotReady
	}
	bw := bufio.NewWriter(w)
	if _, err := bw.Write(magic[:]); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.LittleEndian, h.header()); err != nil {
		return err
	}
	var body bytes.Buffer
	for j := range h.levels {
		for c := range h.levels[j].stale {
			if err := h.writeCluster(bw, &body, j, int32(c)); err != nil {
				return err
			}
		}
	}
	for k := range h.views {
		if err := h.writeLabels(bw, &body, k); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// Dump appends to w the records of what was built since Save or Dump last wrote: the clusters
// finished and not stale again, and those of level 1 whose nodes were named anew, every class's
// view of each, and the labels of a class worked out anew; it reports how many clusters it
// wrote. A file Save wrote with the Dumps appended
// loads as the hierarchy stood after the last of them.
func (h *Hierarchy[T]) Dump(w io.Writer) (clusters int, err error) {
	bw := bufio.NewWriter(w)
	var body bytes.Buffer
	for j := range h.levels {
		lv := &h.levels[j]
		for c := range lv.built {
			if !lv.built[c] && !(j == 0 && h.renamed(int32(c))) || lv.stale[c] != fresh {
				continue
			}
			if err := h.writeCluster(bw, &body, j, int32(c)); err != nil {
				return clusters, err
			}
			clusters++
		}
	}
	for k := range h.views {
		l := &h.views[k].labels
		if l.pending() || l.made == l.savedMade && l.local == l.savedLocal {
			continue
		}
		if err := h.writeLabels(bw, &body, k); err != nil {
			return clusters, err
		}
	}
	return clusters, bw.Flush()
}

// writeCluster writes the record of cluster c of level j as every class sees it, and notes it
// saved
func (h *Hierarchy[T]) writeCluster(w io.Writer, body *bytes.Buffer, j int, c int32) error {
	body.Reset()
	e := encoder[T]{w: body}
	e.i32(int32(j))
	e.i32(c)
	for k := range h.views {
		e.cluster(&h.views[k].levels[j].clusters[c])
	}
	h.levels[j].built[c] = false
	if j == 0 {
		for k := range h.views {
			h.views[k].labels.renamed[c] = false
		}
	}
	return writeRecord(w, recordCluster, body.Bytes())
}

// renamed reports whether a class named the nodes of cluster c of level 1 since it was saved or
// dumped
func (h *Hierarchy[T]) renamed(c int32) bool {
	for k := range h.views {
		if h.views[k].labels.renamed[c] {
			return true
		}
	}
	return false
}

// writeLabels writes the record of class k's labels, and notes them saved
func (h *Hierarchy[T]) writeLabels(w io.Writer, body *bytes.Buffer, k int) error {
	body.Reset()
	l := &h.views[k].labels
	e := encoder[T]{w: body}
	e.i32(int32(k))
	e.i32s(l.alias)
	e.i32(int32(l.given))
	l.savedMade, l.savedLocal = l.made, l.local
	return writeRecord(w, recordLabels, body.Bytes())
}

// writeRecord writes one record: its kind, its body's length, the body and the CRC-32 of the
// kind and the body
func writeRecord(w io.Writer, kind byte, body []byte) error {
	var head [5]byte
	head[0] = kind
	binary.LittleEndian.PutUint32(head[1:], uint32(len(body)))
	sum := crc32.NewIEEE()
	sum.Write(head[:1])
	sum.Write(body)
	var tail [4]byte
	binary.LittleEndian.PutUint32(tail[:], sum.Sum32())
	for _, b := range [][]byte{head[:], body, tail[:]} {
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// Load reads into h a file Save wrote of a hierarchy of the same shape over the same graph, the
// Dumps appended to it taken in their order, and leaves h Ready, nothing to dump. It fails with
// ErrMismatch where the file holds another hierarchy, ErrNoOf where the graph has no Of, and
// ErrCorrupt where the file does not read as saved; whatever it fails with, h stands as before.
func (h *Hierarchy[T]) Load(r io.Reader) error {
	if h.graph.Of == nil {
		return ErrNoOf
	}
	br := bufio.NewReader(r)
	var m [4]byte
	if _, err := io.ReadFull(br, m[:]); err != nil || m != magic {
		return fmt.Errorf("%w: no header", ErrCorrupt)
	}
	var hd header
	if err := binary.Read(br, binary.LittleEndian, &hd); err != nil {
		return fmt.Errorf("%w: a header cut short", ErrCorrupt)
	}
	if own := h.header(); hd != own {
		return fmt.Errorf("%w: the file's %+v, the hierarchy's %+v", ErrMismatch, hd, own)
	}
	views := make([]view[T], len(h.views))
	for k := range views {
		for j := range h.levels {
			n := len(h.levels[j].stale)
			views[k].levels = append(views[k].levels,
				tier[T]{clusters: make([]cluster[T], n), base: make([]int32, n+1)})
		}
	}
	loaded := make([][]bool, len(h.levels))
	for j := range loaded {
		loaded[j] = make([]bool, len(h.levels[j].stale))
	}
	labelled := make([]bool, len(views))
	for {
		kind, body, err := readRecord(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		d := decoder[T]{b: body, of: h.graph.Of}
		switch kind {
		case recordCluster:
			j, c := int(d.i32()), d.i32()
			if d.err != nil || j < 0 || j >= len(h.levels) || c < 0 ||
				int(c) >= len(h.levels[j].stale) {
				return fmt.Errorf("%w: a cluster record off the levels", ErrCorrupt)
			}
			for k := range views {
				d.cluster(&views[k].levels[j].clusters[c], j)
			}
			loaded[j][c] = true
		case recordLabels:
			k := d.i32()
			if d.err != nil || k < 0 || int(k) >= len(views) {
				return fmt.Errorf("%w: a labels record of no class", ErrCorrupt)
			}
			l := &views[k].labels
			l.alias = d.i32s(nil)
			l.given = int(d.i32())
			labelled[k] = true
		default:
			return fmt.Errorf("%w: a record of kind %d", ErrCorrupt, kind)
		}
		if d.err != nil {
			return d.err
		}
		if len(d.b) != 0 {
			return fmt.Errorf("%w: a record longer than its fields", ErrCorrupt)
		}
	}
	for j := range loaded {
		for c, ok := range loaded[j] {
			if !ok {
				return fmt.Errorf("%w: cluster %d of level %d missing", ErrCorrupt, c, j+1)
			}
		}
	}
	for k, ok := range labelled {
		if !ok {
			return fmt.Errorf("%w: the labels of class %d missing", ErrCorrupt, k)
		}
	}
	h.stand(views)
	return nil
}

// stand has h hold views, loaded whole: every cluster fresh, the labels of every class worked
// out, no job under way, nothing to dump
func (h *Hierarchy[T]) stand(views []view[T]) {
	for k := range views {
		l := &views[k].labels
		l.at = make([]int32, len(h.levels[0].stale))
		for c := range l.at {
			l.at[c] = -1
		}
		l.renamed = make([]bool, len(h.levels[0].stale))
		h.trees[k] = 0
		for c := range views[k].levels[0].clusters {
			h.trees[k] += views[k].levels[0].clusters[c].bytes
		}
	}
	h.views = views
	for j := range h.levels {
		lv := &h.levels[j]
		clear(lv.stale)
		clear(lv.built)
		lv.stales, lv.next = 0, 0
		for k := range h.views {
			h.views[k].based(j)
		}
	}
	h.job = job{was: h.job.was, across: h.job.across[:0]}
}

// readRecord reads one record: its kind and its body, io.EOF at a clean end
func readRecord(r *bufio.Reader) (kind byte, body []byte, err error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		if err == io.EOF {
			return 0, nil, io.EOF
		}
		return 0, nil, fmt.Errorf("%w: a record cut short", ErrCorrupt)
	}
	n := binary.LittleEndian.Uint32(head[1:])
	if n > 1<<30 {
		return 0, nil, fmt.Errorf("%w: a record of %d bytes", ErrCorrupt, n)
	}
	body = make([]byte, n)
	var tail [4]byte
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, fmt.Errorf("%w: a record cut short", ErrCorrupt)
	}
	if _, err := io.ReadFull(r, tail[:]); err != nil {
		return 0, nil, fmt.Errorf("%w: a record cut short", ErrCorrupt)
	}
	sum := crc32.NewIEEE()
	sum.Write(head[:1])
	sum.Write(body)
	if sum.Sum32() != binary.LittleEndian.Uint32(tail[:]) {
		return 0, nil, fmt.Errorf("%w: a record spoilt", ErrCorrupt)
	}
	return head[0], body, nil
}

// encoder writes the fields of a record
type encoder[T comparable] struct {
	w   *bytes.Buffer
	buf [8]byte
}

func (e *encoder[T]) u32(v uint32) {
	binary.LittleEndian.PutUint32(e.buf[:4], v)
	e.w.Write(e.buf[:4])
}

func (e *encoder[T]) i32(v int32) { e.u32(uint32(v)) }

func (e *encoder[T]) i64(v int64) {
	binary.LittleEndian.PutUint64(e.buf[:], uint64(v))
	e.w.Write(e.buf[:])
}

func (e *encoder[T]) f32(v float32) { e.u32(math.Float32bits(v)) }

func (e *encoder[T]) i32s(vs []int32) {
	e.i32(int32(len(vs)))
	for _, v := range vs {
		e.i32(v)
	}
}

func (e *encoder[T]) ints(vs []int) {
	e.i32(int32(len(vs)))
	for _, v := range vs {
		e.i64(int64(v))
	}
}

func (e *encoder[T]) f32s(vs []float32) {
	e.i32(int32(len(vs)))
	for _, v := range vs {
		e.f32(v)
	}
}

func (e *encoder[T]) u64s(vs []uint64) {
	e.i32(int32(len(vs)))
	for _, v := range vs {
		e.i64(int64(v))
	}
}

func (e *encoder[T]) u16s(vs []uint16) {
	e.i32(int32(len(vs)))
	for _, v := range vs {
		binary.LittleEndian.PutUint16(e.buf[:2], v)
		e.w.Write(e.buf[:2])
	}
}

func (e *encoder[T]) bytes(vs []byte) {
	e.i32(int32(len(vs)))
	e.w.Write(vs)
}

// cluster writes cl's fields but its states, which its keys stand again through Of
func (e *encoder[T]) cluster(cl *cluster[T]) {
	e.ints(cl.keys)
	e.i32s(cl.local)
	e.i32(int32(len(cl.down)))
	for _, d := range cl.down {
		e.i32(d.c)
		e.i32(d.i)
	}
	e.i32s(cl.class)
	e.i32s(cl.twinAt)
	e.i32(int32(len(cl.twins)))
	for _, tw := range cl.twins {
		e.i32(tw.other)
		e.i64(int64(tw.key))
		e.f32(tw.out)
		e.f32(tw.in)
	}
	e.f32s(cl.intra)
	e.u64s(cl.out)
	e.u64s(cl.in)
	e.i32s(cl.offset)
	e.i32(cl.span)
	e.i32(cl.layers)
	e.i64(int64(cl.bytes))
	e.bytes(cl.tree8)
	e.u16s(cl.tree16)
	e.i32s(cl.label)
}

// decoder reads the fields of a record's body, keeping the first error: a field past the body's
// end
type decoder[T comparable] struct {
	b   []byte
	of  func(int) T
	err error
}

func (d *decoder[T]) take(n int) []byte {
	if d.err != nil || len(d.b) < n {
		d.err = fmt.Errorf("%w: a record shorter than its fields", ErrCorrupt)
		d.b = nil
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder[T]) u32() uint32 {
	if b := d.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (d *decoder[T]) i32() int32 { return int32(d.u32()) }

func (d *decoder[T]) i64() int64 {
	if b := d.take(8); b != nil {
		return int64(binary.LittleEndian.Uint64(b))
	}
	return 0
}

func (d *decoder[T]) f32() float32 { return math.Float32frombits(d.u32()) }

// count is the length of a slice to read, held to what the body may hold
func (d *decoder[T]) count(size int) int {
	n := int(d.i32())
	if n < 0 || n*size > len(d.b) {
		d.err = fmt.Errorf("%w: a slice longer than its record", ErrCorrupt)
		d.b = nil
		return 0
	}
	return n
}

func (d *decoder[T]) i32s(dst []int32) []int32 {
	n := d.count(4)
	dst = dst[:0]
	for range n {
		dst = append(dst, d.i32())
	}
	return dst
}

func (d *decoder[T]) ints() []int {
	n := d.count(8)
	var dst []int
	for range n {
		dst = append(dst, int(d.i64()))
	}
	return dst
}

func (d *decoder[T]) f32s() []float32 {
	n := d.count(4)
	var dst []float32
	for range n {
		dst = append(dst, d.f32())
	}
	return dst
}

func (d *decoder[T]) u64s() []uint64 {
	n := d.count(8)
	var dst []uint64
	for range n {
		dst = append(dst, uint64(d.i64()))
	}
	return dst
}

func (d *decoder[T]) u16s() []uint16 {
	n := d.count(2)
	var dst []uint16
	for range n {
		if b := d.take(2); b != nil {
			dst = append(dst, binary.LittleEndian.Uint16(b))
		}
	}
	return dst
}

func (d *decoder[T]) bytes() []byte {
	n := d.count(1)
	if b := d.take(n); b != nil {
		return append([]byte(nil), b...)
	}
	return nil
}

// cluster reads cl, a cluster of level j, its states stood again through Of
func (d *decoder[T]) cluster(cl *cluster[T], j int) {
	cl.keys = d.ints()
	cl.states = make([]T, 0, len(cl.keys))
	for _, key := range cl.keys {
		cl.states = append(cl.states, d.of(key))
	}
	cl.local = d.i32s(nil)
	cl.down = nil
	for range d.count(8) {
		cl.down = append(cl.down, nodeRef{c: d.i32(), i: d.i32()})
	}
	cl.class = d.i32s(nil)
	cl.twinAt = d.i32s(nil)
	cl.twins = nil
	for range d.count(20) {
		cl.twins = append(cl.twins, twin{other: d.i32(), key: int(d.i64()), out: d.f32(),
			in: d.f32()})
	}
	cl.intra = d.f32s()
	cl.out = d.u64s()
	cl.in = d.u64s()
	cl.offset = d.i32s(nil)
	cl.span = d.i32()
	cl.layers = d.i32()
	cl.bytes = int(d.i64())
	cl.tree8 = d.bytes()
	cl.tree16 = d.u16s()
	cl.label = d.i32s(nil)
	if d.err == nil && (len(cl.states) != len(cl.keys) || len(cl.class) != len(cl.keys) ||
		len(cl.twinAt) != len(cl.keys)+1 || len(cl.intra) != len(cl.keys)*len(cl.keys) ||
		j == 0 && len(cl.local) != len(cl.keys) || j > 0 && len(cl.down) != len(cl.keys)) {
		d.err = fmt.Errorf("%w: a cluster of level %d whose fields disagree", ErrCorrupt, j+1)
	}
}
