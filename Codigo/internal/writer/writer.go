// Package writer serializes every database mutation through a single goroutine.
//
// All writes in EventPOS are submitted as commands to one channel, consumed by
// one goroutine that owns the writer *sql.DB (which itself caps at one physical
// connection). This makes mutations globally serial in-process: two operators
// charging the same attendee, or selling the last unit of stock, are physically
// queued and can never interleave. It is the backbone of the no-double-discount
// / no-race / no-negative-inventory guarantees.
//
// Each command runs inside a single BEGIN IMMEDIATE transaction so it grabs the
// SQLite write lock up front (fail-fast instead of mid-transaction SQLITE_BUSY).
package writer

import (
	"context"
	"database/sql"
	"fmt"
)

// TxFn is a unit of work executed inside one BEGIN IMMEDIATE transaction.
// Whatever it returns is handed back to the caller of Do.
type TxFn func(tx *sql.Tx) (any, error)

type command struct {
	ctx   context.Context
	fn    TxFn
	reply chan result
}

type result struct {
	value any
	err   error
}

// Writer is the single-writer command bus.
type Writer struct {
	db    *sql.DB
	queue chan command
	done  chan struct{}
}

// New starts the writer goroutine. db must be the single-connection writer
// handle from package db. Call Close to stop it.
func New(db *sql.DB) *Writer {
	w := &Writer{
		db:    db,
		queue: make(chan command, 256),
		done:  make(chan struct{}),
	}
	go w.loop()
	return w
}

func (w *Writer) loop() {
	defer close(w.done)
	for cmd := range w.queue {
		val, err := w.runTx(cmd)
		cmd.reply <- result{value: val, err: err}
	}
}

// runTx executes one command inside a transaction. The writer handle is opened
// with _txlock=immediate, so BeginTx takes the SQLite write lock up front.
func (w *Writer) runTx(cmd command) (any, error) {
	tx, err := w.db.BeginTx(cmd.ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	val, err := cmd.fn(tx)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return val, nil
}

// Do submits fn to the writer and blocks until it commits or fails.
// It is safe to call from many goroutines concurrently; calls are serialized.
func (w *Writer) Do(ctx context.Context, fn TxFn) (any, error) {
	reply := make(chan result, 1)
	select {
	case w.queue <- command{ctx: ctx, fn: fn, reply: reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-reply:
		return r.value, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops accepting commands and waits for the goroutine to drain.
func (w *Writer) Close() {
	close(w.queue)
	<-w.done
}
