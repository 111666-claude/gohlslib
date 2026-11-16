package gohlslib

import (
	"context"
	"sync"
	"time"
)

type segmentData struct {
	dateTime *time.Time
	payload  []byte
	err      error
	// catchUp is true when this segment starts a target-latency catch up:
	// the output clock must be re-anchored so that the segment and the
	// buffered media are delivered immediately instead of at real-time pace.
	catchUp bool
}

// clientSegmentQueue stores the segments downloaded by the stream downloader
// and feeds them to the stream processor. It supports discarding queued
// segments that have been superseded by a target-latency catch up, so that the
// downloader is never blocked by media that is no longer meant to be played.
type clientSegmentQueue struct {
	mutex   sync.Cond
	queue   []*segmentData
	pending int
}

func (q *clientSegmentQueue) initialize() {
	q.mutex.L = &sync.Mutex{}
}

func (q *clientSegmentQueue) push(seg *segmentData) {
	q.mutex.L.Lock()
	q.queue = append(q.queue, seg)
	q.pending++
	q.mutex.Broadcast()
	q.mutex.L.Unlock()
}

// discardQueued drops every segment still waiting to be processed and returns
// the number of segments that were dropped. It is used after a catch up, when
// the queued media has become stale.
func (q *clientSegmentQueue) discardQueued() int {
	q.mutex.L.Lock()
	dropped := len(q.queue)
	q.queue = nil
	q.pending -= dropped
	q.mutex.Broadcast()
	q.mutex.L.Unlock()
	return dropped
}

// waitUntilPendingIsBelow blocks while the number of downloaded but not yet
// fully processed segments is above n.
func (q *clientSegmentQueue) waitUntilPendingIsBelow(errGroupCtx context.Context, n int) bool {
	stop := context.AfterFunc(errGroupCtx, func() {
		q.mutex.L.Lock()
		q.mutex.Broadcast()
		q.mutex.L.Unlock()
	})
	defer stop()

	q.mutex.L.Lock()
	for q.pending > n && errGroupCtx.Err() == nil {
		q.mutex.Wait()
	}
	q.mutex.L.Unlock()

	return errGroupCtx.Err() == nil
}

func (q *clientSegmentQueue) pull(errGroupCtx context.Context) (*segmentData, bool) {
	// abort the wait when the context is canceled
	stop := context.AfterFunc(errGroupCtx, func() {
		q.mutex.L.Lock()
		q.mutex.Broadcast()
		q.mutex.L.Unlock()
	})
	defer stop()

	q.mutex.L.Lock()
	for len(q.queue) == 0 && errGroupCtx.Err() == nil {
		q.mutex.Wait()
	}

	if errGroupCtx.Err() != nil {
		q.mutex.L.Unlock()
		return nil, false
	}

	var seg *segmentData
	seg, q.queue = q.queue[0], q.queue[1:]
	q.mutex.L.Unlock()

	return seg, true
}

// markProcessed decrements the number of segments that have been downloaded
// but not fully processed yet.
func (q *clientSegmentQueue) markProcessed() {
	q.mutex.L.Lock()
	q.pending--
	q.mutex.Broadcast()
	q.mutex.L.Unlock()
}
