package recording

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultSegmentSize is when the recorder moves on to the next segment.
	DefaultSegmentSize = 64 << 20
	// DefaultKeep is how many segments a recording keeps; older ones are
	// deleted, about 512 MB per session at the default size.
	DefaultKeep = 8

	blockSize = 32 << 10
)

// Options tune the recorder; zero values mean the defaults.
type Options struct {
	SegmentSize int64
	Keep        int
}

// Record is the recorder: it copies in (the pane output tmux pipes to it)
// into dir's segments until EOF, then writes ended_at. It holds
// recorder.lock the whole time and is the only writer of the segments and
// meta.json while it runs. It never holds more than one read block and writes
// each block before reading the next.
func Record(dir string, in io.Reader, opts Options) error {
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if opts.Keep <= 0 {
		opts.Keep = DefaultKeep
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return fmt.Errorf("open recorder lock: %w", err)
	}
	defer lock.Close()
	// Retry instead of blocking: a status check holds the lock for a moment
	// while it probes, but a second recorder must not wait forever.
	if err := lockWithin(lock, 2*time.Second); err != nil {
		return err
	}

	r := &recorder{dir: dir, opts: opts, last: time.Now()}
	existing, err := segments(dir)
	if err != nil {
		return err
	}
	next := 1
	if n := len(existing); n > 0 {
		next = existing[n-1] + 1
	}
	if err := r.open(next); err != nil {
		return err
	}
	defer r.unlinks.Wait()

	buf := make([]byte, blockSize)
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			if err := r.write(buf[:n]); err != nil {
				r.close()
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			r.close()
			return readErr
		}
	}
	if err := r.close(); err != nil {
		return err
	}
	return Finalize(dir, time.Now())
}

type recorder struct {
	dir     string
	opts    Options
	segment int
	size    int64
	full    bool // the segment ended at a newline before reaching size
	log     *os.File
	timing  *os.File
	last    time.Time      // time of the previous write, for timing delays
	started map[int]string // segment -> when this recorder opened it
	unlinks sync.WaitGroup
}

// write appends one block. When the block would take the segment past its
// size, the segment ends after the block's last newline that fits, else after
// its first newline, else exactly at the size; the rest goes to the next
// segment. So a line rarely straddles two segments, and a segment is at most
// one block over the size. Rotation waits for the next byte, so a recording
// never ends with an empty segment.
func (r *recorder) write(block []byte) error {
	for len(block) > 0 {
		if r.full || r.size >= r.opts.SegmentSize {
			if err := r.rotate(); err != nil {
				return err
			}
		}
		room := int(r.opts.SegmentSize - r.size)
		if len(block) <= room {
			return r.append(block)
		}
		split := bytes.LastIndexByte(block[:room], '\n') + 1
		if split == 0 {
			if i := bytes.IndexByte(block[room:], '\n'); i >= 0 {
				split = room + i + 1
			} else {
				split = room
			}
		}
		if err := r.append(block[:split]); err != nil {
			return err
		}
		r.full = true
		block = block[split:]
	}
	return nil
}

// append writes data to the current segment and its timing line.
func (r *recorder) append(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	now := time.Now()
	delay := now.Sub(r.last).Seconds()
	r.last = now
	if _, err := r.log.Write(data); err != nil {
		return fmt.Errorf("write segment: %w", err)
	}
	r.size += int64(len(data))
	if _, err := fmt.Fprintf(r.timing, "%.6f %d\n", delay, len(data)); err != nil {
		return fmt.Errorf("write timing: %w", err)
	}
	return nil
}

func (r *recorder) open(n int) error {
	log, err := os.OpenFile(segmentPath(r.dir, n, ".log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("open segment: %w", err)
	}
	timing, err := os.OpenFile(segmentPath(r.dir, n, ".timing"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
	if err != nil {
		log.Close()
		return fmt.Errorf("open timing: %w", err)
	}
	r.log, r.timing, r.segment, r.size, r.full = log, timing, n, 0, false
	if r.started == nil {
		r.started = map[int]string{}
	}
	r.started[n] = formatTime(time.Now())
	return nil
}

func (r *recorder) close() error {
	return errors.Join(r.log.Close(), r.timing.Close())
}

// rotate closes the current segment and opens the next. When more than Keep
// segments exist, the oldest go (unlinked in the background, so capture never
// stalls) and meta.json's first_segment moves forward, which is how a reader
// learns that older output was rotated away.
func (r *recorder) rotate() error {
	if err := r.close(); err != nil {
		return err
	}
	if err := r.open(r.segment + 1); err != nil {
		return err
	}
	_ = LogEvent(r.dir, "rotated", Fields{"segment": r.segment})

	meta, err := ReadMeta(r.dir)
	if err != nil {
		return nil // the log is what matters; meta is best effort
	}
	first := max(meta.FirstSegment, 1)
	if r.segment-first+1 <= r.opts.Keep {
		return nil
	}
	var gone []int
	for ; r.segment-first+1 > r.opts.Keep; first++ {
		gone = append(gone, first)
	}
	meta.FirstSegment = first
	if at, ok := r.started[first]; ok {
		meta.FirstSegmentAt = at
	} else if info, err := os.Stat(segmentPath(r.dir, first-1, ".log")); err == nil {
		// Opened before this recorder: the previous segment's last write
		// is when it began.
		meta.FirstSegmentAt = formatTime(info.ModTime())
	}
	_ = WriteMeta(r.dir, meta)
	r.unlinks.Add(1)
	go func() {
		defer r.unlinks.Done()
		for _, n := range gone {
			_ = os.Remove(segmentPath(r.dir, n, ".log"))
			_ = os.Remove(segmentPath(r.dir, n, ".timing"))
		}
	}()
	return nil
}

func lockWithin(f *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock recorder: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("another recorder holds " + f.Name())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
