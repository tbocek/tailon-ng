//go:build linux

package main

import (
	"context"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// On Linux, followed files are watched with inotify (via the standard library's
// syscall package — no extra dependency), so appended lines reach the browser
// instantly instead of on the next poll. The event is only a wake-up hint: the
// read loop in tailer.go stays the source of truth, and a fallback timer
// catches whatever inotify cannot see — rotation by rename, network
// filesystems, or watch-limit exhaustion. Where inotify is unavailable the
// tailer degrades to plain pollInterval polling, same as non-Linux platforms.
const watchFallback = 2 * time.Second

// ino is the process-wide inotify instance shared by every followed file, or
// nil when inotify could not be initialized (then everything polls).
var ino = newInotify()

type inotify struct {
	fd   int
	mu   sync.Mutex
	subs map[int32][]chan struct{} // watch descriptor -> subscriber wake channels
	wds  map[string]int32          // path -> watch descriptor
	refs map[int32]int             // watch descriptor -> subscriber count
}

func newInotify() *inotify {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return nil
	}
	in := &inotify{
		fd:   fd,
		subs: map[int32][]chan struct{}{},
		wds:  map[string]int32{},
		refs: map[int32]int{},
	}
	go in.run()
	return in
}

// run reads inotify events forever and pokes every subscriber of the affected
// watch. Sends are non-blocking: the channels hold one pending wake-up, which
// is all a poll-style loop needs.
func (in *inotify) run() {
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(in.fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil || n < syscall.SizeofInotifyEvent {
			return
		}
		in.mu.Lock()
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			for _, ch := range in.subs[ev.Wd] {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
			off += syscall.SizeofInotifyEvent + int(ev.Len)
		}
		in.mu.Unlock()
	}
}

func (in *inotify) subscribe(path string, ch chan struct{}) (int32, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	wd, ok := in.wds[path]
	if !ok {
		w, err := syscall.InotifyAddWatch(in.fd, path,
			syscall.IN_MODIFY|syscall.IN_ATTRIB|syscall.IN_MOVE_SELF|syscall.IN_DELETE_SELF)
		if err != nil {
			return 0, false // missing file, watch limit, unsupported fs — poll instead
		}
		wd = int32(w)
		in.wds[path] = wd
	}
	in.subs[wd] = append(in.subs[wd], ch)
	in.refs[wd]++
	return wd, true
}

func (in *inotify) unsubscribe(wd int32, ch chan struct{}) {
	in.mu.Lock()
	defer in.mu.Unlock()
	list := in.subs[wd]
	for i, c := range list {
		if c == ch {
			in.subs[wd] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if in.refs[wd]--; in.refs[wd] > 0 {
		return
	}
	syscall.InotifyRmWatch(in.fd, uint32(wd))
	delete(in.subs, wd)
	delete(in.refs, wd)
	for p, w := range in.wds {
		if w == wd {
			delete(in.wds, p)
		}
	}
}

// fileWatch is one followed file's subscription. It survives rotation: the
// tailer calls rewatch after reopening, which re-attaches by path (inotify
// watches follow the inode, not the name).
type fileWatch struct {
	path string
	ch   chan struct{}
	wd   int32
	sub  bool
}

func watchFile(path string) *fileWatch {
	w := &fileWatch{path: path, ch: make(chan struct{}, 1)}
	w.attach()
	return w
}

func (w *fileWatch) attach() {
	if ino == nil || w.sub {
		return
	}
	if wd, ok := ino.subscribe(w.path, w.ch); ok {
		w.wd, w.sub = wd, true
	}
}

func (w *fileWatch) detach() {
	if w.sub {
		ino.unsubscribe(w.wd, w.ch)
		w.sub = false
	}
}

func (w *fileWatch) rewatch() { w.detach(); w.attach() }
func (w *fileWatch) close()   { w.detach() }

// wait blocks until the file changes, the fallback timer fires, or ctx is
// done; it returns false only for ctx. Unwatched files (not created yet,
// watch limit reached, …) fall back to the plain polling interval.
func (w *fileWatch) wait(ctx context.Context) bool {
	w.attach() // the file may not have existed on earlier attempts
	if !w.sub {
		return sleep(ctx, pollInterval)
	}
	select {
	case <-ctx.Done():
		return false
	case <-w.ch:
		return true
	case <-time.After(watchFallback):
		return true
	}
}
