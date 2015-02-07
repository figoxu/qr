package qr

// In process queue with disk based overflow. Element order is not strictly
// preserved.
//
// When everything is fine elements flow over Qr.q. This is a simple channel
// directly connecting the producer(s) and the consumer(s).
// If that channel is full elements are written to the Qr.planb channel.
// swapout() will write all elements from Qr.planb to disk. That file is closed
// after `timeout`. At the same time swapin() will try to process old files: if
// there is at least a single completed file, swapin() will open that file and
// try to write the elements to Qr.q.
//
//   ---> Enqueue()    ---------   .q  --------->     Dequeue() --->
//             \                                           ^
//            .planb                                     .q
//               \--> swapout() --> fs() --> swapin() --/
//
//
// Usage:
//    q := New("/mnt/queues/", "demo", OptionBuffer(100))
//    defer q.Close()
//    go func() {
//        for e := range q.Dequeue() {
//           fmt.Printf("We got: %v\n", e)
//        }
//    }()
//
//    // elsewhere:
//    q.Enqueue("aap")
//    q.Enqueue("noot")

import (
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultTimeout can be changed with OptionTimeout.
	DefaultTimeout = 10 * time.Second
	// DefaultBuffer can be changed with OptionBuffer.
	DefaultBuffer = 1000

	fileExtension = ".qr"
)

// Qr is a disk-based queue.
type Qr struct {
	q          chan interface{} // the main channel.
	planb      chan interface{} // to disk, used when q is full.
	dir        string
	prefix     string
	timeout    time.Duration
	bufferSize int
	log        func(string, ...interface{}) // Printf() style
}

// Option is an option to New(), which can change some settings.
type Option func(qr *Qr)

// OptionTimeout is an option for New(). It specifies the time after which a queue
// file is closed. Smaller means more files.
func OptionTimeout(t time.Duration) Option {
	return func(qr *Qr) {
		qr.timeout = t
	}
}

// OptionBuffer is an option for New(). It specifies the in-memory size of the
// queue. Smaller means the disk will be used sooner, larger means more memory.
func OptionBuffer(n int) Option {
	return func(qr *Qr) {
		qr.bufferSize = n
	}
}

// OptionLogger is an option for New(). Is sets the logger, the default is
// log.Printf, but glog.Errorf would also work.
func OptionLogger(l func(string, ...interface{})) Option {
	return func(qr *Qr) {
		qr.log = l
	}
}

// New starts a Queue which stores files in <dir>/<prefix>-.<timestamp>.qr
func New(dir, prefix string, options ...Option) *Qr {
	qr := Qr{
		planb:      make(chan interface{}),
		dir:        dir,
		prefix:     prefix,
		timeout:    DefaultTimeout,
		bufferSize: DefaultBuffer,
		log:        log.Printf,
	}
	for _, cb := range options {
		cb(&qr)
	}
	qr.q = make(chan interface{}, qr.bufferSize)

	var (
		filesToDisk   = make(chan string)
		filesFromDisk = make(chan string)
	)
	go qr.swapout(filesToDisk)
	go qr.fs(filesToDisk, filesFromDisk)
	go qr.swapin(filesFromDisk)
	for _, f := range qr.findOld() {
		filesFromDisk <- f
	}
	return &qr
}

// Enqueue adds something in the queue. This never blocks, and is safe to be
// called by different goroutines.
func (qr *Qr) Enqueue(e interface{}) {
	select {
	case qr.q <- e:
	default:
		qr.planb <- e
	}
}

// Dequeue is the channel where elements come out the queue. It'll be closed
// on Close().
func (qr *Qr) Dequeue() <-chan interface{} {
	return qr.q
}

// Close shuts down all Go routines and closes the Dequeue() channel. It'll
// write all in-flight entries to disk. Calling Enqueue() after Close will
// panic.
func (qr *Qr) Close() {
	// Closing planb triggers a cascade closing of all go-s and channels.
	close(qr.planb)

	// Store the in-flight entries for next time.
	filename := qr.batchFilename(time.Time{}) // special filename
	fh, err := os.Create(filename)
	if err != nil {
		qr.log("create err: %v", err)
		return
	}
	enc := gob.NewEncoder(fh)
	count := 0
	for e := range qr.q {
		count++
		if err = enc.Encode(&e); err != nil {
			qr.log("Encode error: %v", err)
		}
	}
	fh.Close()
	if count == 0 {
		// there was nothing to queue
		os.Remove(filename)
	}
}

func (qr *Qr) swapout(files chan<- string) {
	var (
		enc      *gob.Encoder
		filename string
		fh       io.WriteCloser
		tc       <-chan time.Time
		t        = time.NewTimer(0)
		err      error
	)
	defer func() {
		if enc != nil {
			fh.Close()
			files <- filename
		}
		close(files)
	}()
	for {
		select {
		case e, ok := <-qr.planb:
			if !ok {
				return
			}
			if enc == nil {
				filename = qr.batchFilename(time.Now().UTC())
				fh, err = os.Create(filename)
				if err != nil {
					// TODO: sure we return?
					qr.log("create err: %v\n", err)
					return
				}
				enc = gob.NewEncoder(fh)
				t.Reset(qr.timeout)
				tc = t.C
			}
			if err = enc.Encode(&e); err != nil {
				qr.log("Encode error: %v\n", err)
			}
		case <-tc:
			fh.Close()
			files <- filename
			enc = nil
			tc = nil
		}
	}
}

func (qr *Qr) swapin(files <-chan string) {
	defer close(qr.q)
	for filename := range files {
		fh, err := os.Open(filename)
		if err != nil {
			qr.log("open err: %v\n", err)
			continue
		}
		os.Remove(filename)
		dec := gob.NewDecoder(fh)
		for {
			var next interface{}
			if err = dec.Decode(&next); err != nil {
				if err != io.EOF {
					qr.log("decode err: %v\n", err)
				}
				fh.Close()
				fh = nil
				break
			}
			qr.q <- next
		}
	}
}

func (qr *Qr) fs(in <-chan string, out chan<- string) {
	defer close(out)
	var (
		filenames []string
		checkOut  chan<- string
		next      string
	)
	for {
		select {
		case f, ok := <-in:
			if !ok {
				return
			}
			if checkOut == nil {
				checkOut = out
				next = f
			} else {
				filenames = append(filenames, f)
			}
		case checkOut <- next:
			if len(filenames) > 0 {
				next, filenames = filenames[0], filenames[1:]
			} else {
				// case disabled since there is no file
				checkOut = nil
			}
		}
	}
}

func (qr *Qr) batchFilename(t time.Time) string {
	format := "20060102T150405.999999999"
	return fmt.Sprintf("%s/%s-%s%s",
		qr.dir,
		qr.prefix,
		t.Format(format),
		fileExtension,
	)
}

// findOld finds .qr files from a previous run.
func (qr *Qr) findOld() []string {
	f, err := os.Open(qr.dir)
	if err != nil {
		return nil
	}
	defer f.Close()

	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil
	}

	var existing []string
	for _, n := range names {
		if !strings.HasPrefix(n, qr.prefix+"-") || !strings.HasSuffix(n, fileExtension) {
			continue
		}
		existing = append(existing, filepath.Join(qr.dir, n))
	}

	sort.Strings(existing)

	return existing
}
