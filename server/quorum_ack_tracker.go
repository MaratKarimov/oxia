// Copyright 2023 StreamNative, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/streamnative/oxia/common"
	"github.com/streamnative/oxia/server/util"
)

var (
	ErrTooManyCursors    = errors.New("too many cursors")
	ErrInvalidHeadOffset = errors.New("invalid head offset")
)

// QuorumAckTracker
// The QuorumAckTracker is responsible for keeping track of the head offset and commit offset of a shard
//   - Head offset: the last entry written in the local WAL of the leader
//   - Commit offset: the oldest entry that is considered "fully committed", as it has received the requested amount
//     of acks from the followers
//
// The quorum ack tracker is also used to block until the head offset or commit offset are advanced.
type QuorumAckTracker interface {
	io.Closer

	CommitOffset() int64

	// WaitForCommitOffset
	// Waits for the specific entry id to be fully committed.
	// After that, invokes the closure
	WaitForCommitOffset(ctx context.Context, offset int64, closure func(context.Context, error))

	// WaitForCommitOffsetAsync
	// Async waits for the specific entry id to be fully committed.
	// After that, invokes the closure by async callback goroutine
	WaitForCommitOffsetAsync(ctx context.Context, offset int64, closure func(context.Context, error))

	// NextOffset returns the offset for the next entry to write
	// Note this can go ahead of the head-offset as there can be multiple operations in flight.
	NextOffset() int64

	HeadOffset() int64

	AdvanceHeadOffset(headOffset int64)

	// WaitForHeadOffset
	// Waits until the specified entry is written on the wal
	WaitForHeadOffset(ctx context.Context, offset int64) error

	// NewCursorAcker creates a tracker for a new cursor
	// The `ackOffset` is the previous last-acked position for the cursor
	NewCursorAcker(ackOffset int64) (CursorAcker, error)
}

type quorumAckTracker struct {
	sync.Mutex
	waitingRequests   []waitingRequest
	waitForHeadOffset common.ConditionContext

	replicationFactor uint32
	requiredAcks      uint32

	nextOffset   atomic.Int64
	headOffset   atomic.Int64
	commitOffset atomic.Int64

	// Keep track of the number of acks that each entry has received
	// The bitset is used to handle duplicate acks from a single follower
	tracker            map[int64]*util.BitSet
	cursorIdxGenerator int
	closed             bool
}

type CursorAcker interface {
	Ack(offset int64)
}

type cursorAcker struct {
	quorumTracker *quorumAckTracker
	cursorIdx     int
}

type waitingRequest struct {
	minOffset int64
	closure   func(ctx context.Context, err error)
}

func NewQuorumAckTracker(replicationFactor uint32, headOffset int64, commitOffset int64) QuorumAckTracker {
	q := &quorumAckTracker{
		// Ack quorum is number of follower acks that are required to consider the entry fully committed
		// We are using RF/2 (and not RF/2 + 1) because the leader is already storing 1 copy locally
		requiredAcks:      replicationFactor / 2,
		replicationFactor: replicationFactor,
		tracker:           make(map[int64]*util.BitSet),
		waitingRequests:   make([]waitingRequest, 0),
	}

	q.nextOffset.Store(headOffset)
	q.headOffset.Store(headOffset)
	q.commitOffset.Store(commitOffset)

	// Add entries to track the entries we're not yet sure that are fully committed
	for offset := commitOffset + 1; offset <= headOffset; offset++ {
		q.tracker[offset] = &util.BitSet{}
	}

	q.waitForHeadOffset = common.NewConditionContext(q)
	return q
}

func (q *quorumAckTracker) AdvanceHeadOffset(headOffset int64) {
	q.Lock()
	defer q.Unlock()

	if headOffset <= q.headOffset.Load() {
		return
	}

	q.headOffset.Store(headOffset)
	q.waitForHeadOffset.Broadcast()

	if q.requiredAcks == 0 {
		q.notifyCommitOffsetAdvanced(headOffset)
	} else {
		q.tracker[headOffset] = &util.BitSet{}
	}
}

func (q *quorumAckTracker) NextOffset() int64 {
	return q.nextOffset.Add(1)
}

func (q *quorumAckTracker) CommitOffset() int64 {
	return q.commitOffset.Load()
}

func (q *quorumAckTracker) HeadOffset() int64 {
	return q.headOffset.Load()
}

func (q *quorumAckTracker) WaitForHeadOffset(ctx context.Context, offset int64) error {
	q.Lock()
	defer q.Unlock()

	for !q.closed && q.headOffset.Load() < offset {
		if err := q.waitForHeadOffset.Wait(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (q *quorumAckTracker) WaitForCommitOffset(ctx context.Context, offset int64, closure func(context.Context, error)) {
	ch := make(chan any, 1)
	q.WaitForCommitOffsetAsync(ctx, offset, func(innerCtx context.Context, err error) {
		closure(innerCtx, err)
		ch <- nil
	})

	select {
	case <-ch:
		return
	case <-ctx.Done():
		if err := ctx.Err(); err != nil {
			// the api ensure the ctx.Err will never be nil
			closure(ctx, ctx.Err())
			return
		}
		// add a protection logic here
		slog.Info("unexpected context implementation. it might be a bug!")
		closure(ctx, context.Canceled)
		return
	}
}

func (q *quorumAckTracker) WaitForCommitOffsetAsync(ctx context.Context, offset int64, closure func(context.Context, error)) {
	q.Lock()
	if q.closed {
		q.Unlock()
		closure(ctx, common.ErrorAlreadyClosed)
		return
	}
	if q.requiredAcks == 0 || q.commitOffset.Load() >= offset {
		q.Unlock()
		closure(ctx, nil)
		return
	}
	q.waitingRequests = append(q.waitingRequests, waitingRequest{offset, closure})
	q.Unlock()
}

func (q *quorumAckTracker) notifyCommitOffsetAdvanced(commitOffset int64) {
	q.commitOffset.Store(commitOffset)

	for _, r := range q.waitingRequests {
		if r.minOffset > commitOffset {
			return
		}

		q.waitingRequests = q.waitingRequests[1:]
		r.closure(context.Background(), nil)
	}
}

func (q *quorumAckTracker) Close() error {
	q.Lock()
	defer q.Unlock()

	q.closed = true
	q.waitForHeadOffset.Broadcast()
	return nil
}

func (q *quorumAckTracker) NewCursorAcker(ackOffset int64) (CursorAcker, error) {
	q.Lock()
	defer q.Unlock()

	if uint32(q.cursorIdxGenerator) >= q.replicationFactor-1 {
		return nil, ErrTooManyCursors
	}

	if ackOffset > q.headOffset.Load() {
		return nil, ErrInvalidHeadOffset
	}

	qa := &cursorAcker{
		quorumTracker: q,
		cursorIdx:     q.cursorIdxGenerator,
	}

	// If the new cursor is already past the current quorum commit offset, we have
	// to mark these entries as acked (by that cursor).
	for offset := q.commitOffset.Load() + 1; offset <= ackOffset; offset++ {
		qa.ack(offset)
	}

	q.cursorIdxGenerator++
	return qa, nil
}

func (c *cursorAcker) Ack(offset int64) {
	c.quorumTracker.Lock()
	defer c.quorumTracker.Unlock()

	c.ack(offset)
}

func (c *cursorAcker) ack(offset int64) {
	q := c.quorumTracker

	e, found := q.tracker[offset]
	if !found {
		// The entry has already previously reached the quorum.
		// There's nothing more left to do here.
		return
	}

	// Mark that this follower has acked the entry
	e.Set(c.cursorIdx)
	if uint32(e.Count()) == q.requiredAcks {
		delete(q.tracker, offset)

		// Advance the commit offset
		q.notifyCommitOffsetAdvanced(offset)
	}
}
