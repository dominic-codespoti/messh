package jobs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Output is kept as the first headMax bytes (file <name>.log) followed by
// rolling segments <name>.log.<offset>, of which only the newest two survive.
// The start of a run (command banners, config echo) and its end (the failure)
// are the parts that matter, and disk use stays bounded at
// headMax + 2*segMax per stream. Offsets are absolute positions in the
// original stream, so pollers keep valid cursors even after old segments go.
const (
	defaultHeadBytes    = 4 << 20
	defaultSegmentBytes = 4 << 20
)

type logSink struct {
	dir, name string
	headMax   int64
	segMax    int64

	mu       sync.Mutex
	head     *os.File
	headN    int64
	cur      *os.File
	curN     int64
	prevPath string
	curPath  string
	abs      int64
}

func newLogSink(dir, name string, headMax, segMax int64) (*logSink, error) {
	headPath := filepath.Join(dir, name+".log")
	f, err := os.OpenFile(headPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() > headMax {
		if err := f.Truncate(headMax); err != nil {
			f.Close()
			return nil, err
		}
	}

	stream := openLog(dir, name)
	headN := min(st.Size(), headMax)
	if stream.total > headN && headN < headMax {
		// A tail segment means the head was already sealed. Do not append newer
		// bytes to the head merely because it is now shorter or missing.
		headN = headMax
	}
	var tail []logSeg
	for _, seg := range stream.segs {
		if filepath.Clean(seg.path) != filepath.Clean(headPath) {
			tail = append(tail, seg)
		}
	}
	if len(tail) > 2 {
		for _, seg := range tail[:len(tail)-2] {
			if err := os.Remove(seg.path); err != nil && !os.IsNotExist(err) {
				f.Close()
				return nil, err
			}
		}
		tail = tail[len(tail)-2:]
	}
	s := &logSink{dir: dir, name: name, headMax: headMax, segMax: segMax, head: f, headN: headN, abs: stream.total}
	if n := len(tail); n > 0 {
		last := tail[n-1]
		s.curPath = last.path
		if n > 1 {
			s.prevPath = tail[n-2].path
		}
		if last.size < segMax {
			s.cur, err = os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				f.Close()
				return nil, err
			}
			s.curN = last.size
		}
	}
	return s, nil
}

// Write never fails: a full disk must not wedge the job's pipe.
func (s *logSink) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(b)
	for len(b) > 0 {
		if s.headN < s.headMax {
			take := min(int64(len(b)), s.headMax-s.headN)
			s.head.Write(b[:take])
			s.headN += take
			s.abs += take
			b = b[take:]
			continue
		}
		if s.cur == nil || s.curN >= s.segMax {
			if !s.rotate() {
				s.abs += int64(len(b))
				break
			}
		}
		take := min(int64(len(b)), s.segMax-s.curN)
		s.cur.Write(b[:take])
		s.curN += take
		s.abs += take
		b = b[take:]
	}
	return n, nil
}

func (s *logSink) rotate() bool {
	p := filepath.Join(s.dir, s.name+".log."+strconv.FormatInt(s.abs, 10))
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	if s.cur != nil {
		s.cur.Close()
	}
	if s.prevPath != "" {
		os.Remove(s.prevPath)
	}
	s.prevPath, s.curPath = s.curPath, p
	s.cur, s.curN = f, 0
	return true
}

func (s *logSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.head != nil {
		s.head.Close()
		s.head = nil
	}
	if s.cur != nil {
		s.cur.Close()
		s.cur = nil
	}
}

// --- reading ---

type logSeg struct {
	start int64
	path  string
	size  int64
}

type logStream struct {
	segs  []logSeg
	total int64
}

func openLog(dir, name string) logStream {
	var l logStream
	entries, err := os.ReadDir(dir)
	if err != nil {
		return l
	}
	prefix := name + ".log"
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || e.IsDir() {
			continue
		}
		var start int64
		if n != prefix {
			v, err := strconv.ParseInt(strings.TrimPrefix(n, prefix+"."), 10, 64)
			if err != nil {
				continue
			}
			start = v
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		l.segs = append(l.segs, logSeg{start: start, path: filepath.Join(dir, n), size: st.Size()})
	}
	sort.Slice(l.segs, func(i, j int) bool { return l.segs[i].start < l.segs[j].start })
	if n := len(l.segs); n > 0 {
		l.total = l.segs[n-1].start + l.segs[n-1].size
	}
	return l
}

// read returns up to max bytes starting at off. If off falls in a stretch
// that was rolled away, reading starts at the next surviving byte; the
// returned start says where.
func (l logStream) read(off int64, max int) (data []byte, start int64) {
	if max <= 0 {
		return nil, off
	}
	start = off
	var buf bytes.Buffer
	for _, s := range l.segs {
		end := s.start + s.size
		if end <= off || s.size == 0 {
			continue
		}
		if buf.Len() == 0 {
			if s.start > off {
				start = s.start
			}
		} else if s.start != off {
			break // gap: stop at the contiguous part
		}
		from := max64(off, s.start)
		f, err := os.Open(s.path)
		if err != nil {
			break
		}
		f.Seek(from-s.start, io.SeekStart)
		n, _ := io.CopyN(&buf, f, min(int64(max-buf.Len()), end-from))
		f.Close()
		off = from + n
		if buf.Len() >= max || n == 0 {
			break
		}
	}
	return buf.Bytes(), start
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

const tailScanBytes = 256 << 10

// lastRunStart is where the newest unbroken stretch of output begins; reads
// that must end at the latest byte start no earlier than this.
func (l logStream) lastRunStart() int64 {
	i := len(l.segs) - 1
	if i < 0 {
		return 0
	}
	for i > 0 && l.segs[i-1].start+l.segs[i-1].size == l.segs[i].start {
		i--
	}
	return l.segs[i].start
}

// tailText returns the raw text of the last n lines, the stream offset it
// starts at, and whether earlier output exists that is not included.
func (l logStream) tailText(n int) (text string, off int64, more bool) {
	if n <= 0 || l.total == 0 {
		return "", l.total, l.total > 0
	}
	from := max64(max64(0, l.total-tailScanBytes), l.lastRunStart())
	b, start := l.read(from, tailScanBytes)
	if start > 0 { // the first line is probably cut; drop it
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b, start = b[i+1:], start+int64(i)+1
		}
	}
	end := len(b)
	for end > 0 && (b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	i, seen := end, 0
	for i > 0 {
		if b[i-1] == '\n' {
			if seen++; seen == n {
				break
			}
		}
		i--
	}
	return strings.ToValidUTF8(string(b[i:]), "\uFFFD"), start + int64(i), start > 0 || i > 0
}

// tailLines is tailText split into lines without their terminators.
func (l logStream) tailLines(n int) (lines []string, more bool) {
	text, _, more := l.tailText(n)
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return nil, more
	}
	lines = strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines, more
}

// logChunk is one stream's slice in a job_logs result.
type logChunk struct {
	Text       string `json:"text"`
	Offset     int64  `json:"offset"`      // first byte of text in the stream
	NextOffset int64  `json:"next_offset"` // pass as offset to continue
	TotalBytes int64  `json:"total_bytes"` // bytes written so far
	Skipped    int64  `json:"skipped_bytes,omitempty"`
	More       bool   `json:"earlier_output_omitted,omitempty"`
}

func (l logStream) chunkTail(n int) logChunk {
	text, off, more := l.tailText(n)
	return logChunk{Text: text, Offset: off, NextOffset: l.total, TotalBytes: l.total, More: more}
}

func (l logStream) chunkFrom(off int64, max int) logChunk {
	data, start := l.read(off, max)
	c := logChunk{
		Text:       strings.ToValidUTF8(string(data), "\uFFFD"),
		Offset:     start,
		NextOffset: start + int64(len(data)),
		TotalBytes: l.total,
	}
	if start > off {
		c.Skipped = start - off
	}
	if len(data) == 0 && off > l.total {
		c.NextOffset = l.total
	}
	return c
}

func validStream(s string) error {
	switch s {
	case "", "stdout", "stderr", "both":
		return nil
	}
	return fmt.Errorf("stream must be stdout, stderr or both, not %q", s)
}
