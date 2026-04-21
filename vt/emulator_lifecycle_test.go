package vt

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Lifecycle tests for Emulator.Close / Read / Write, covering the atomic.Bool
// guard on the `closed` field. These exist to lock in:
//   - Close() is idempotent (CompareAndSwap) even under concurrent callers
//   - Read() returns io.EOF after Close
//   - Write() returns io.ErrClosedPipe after Close
//   - Read() blocked inside the pipe is unblocked by Close
//   - No data race between a drain goroutine running Read() in a loop and the
//     owner calling Close() — the original symptom that motivated the fix.

func TestEmulatorClose_FirstCallReturnsNil(t *testing.T) {
	em := NewEmulator(20, 5)

	if err := em.Close(); err != nil {
		t.Fatalf("first Close: got %v, want nil", err)
	}
}

func TestEmulatorClose_SecondCallReturnsNil(t *testing.T) {
	em := NewEmulator(20, 5)
	if err := em.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	if err := em.Close(); err != nil {
		t.Fatalf("second Close: got %v, want nil", err)
	}
}

func TestEmulatorClose_ConcurrentCallersAllReturnNil(t *testing.T) {
	em := NewEmulator(20, 5)

	const N = 64
	var wg sync.WaitGroup
	errs := make(chan error, N)
	wg.Add(N)

	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			errs <- em.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close returned %v, want nil", err)
		}
	}
}

func TestEmulatorRead_AfterCloseReturnsEOF(t *testing.T) {
	em := NewEmulator(20, 5)
	if err := em.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	buf := make([]byte, 16)
	n, err := em.Read(buf)
	if n != 0 {
		t.Errorf("Read n: got %d, want 0", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("Read err: got %v, want io.EOF", err)
	}
}

func TestEmulatorWrite_AfterCloseReturnsErrClosedPipe(t *testing.T) {
	em := NewEmulator(20, 5)
	if err := em.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	n, err := em.Write([]byte("hello"))
	if n != 0 {
		t.Errorf("Write n: got %d, want 0", n)
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Write err: got %v, want io.ErrClosedPipe", err)
	}
}

func TestEmulatorWrite_BeforeCloseSucceeds(t *testing.T) {
	em := NewEmulator(20, 5)
	defer func() { _ = em.Close() }()

	n, err := em.Write([]byte("hi"))
	if err != nil {
		t.Fatalf("Write err: %v", err)
	}
	if n != 2 {
		t.Fatalf("Write n: got %d, want 2", n)
	}
}

// When Read is blocked on the internal pipe, Close must unblock it by closing
// the pipe writer side; the Read goroutine must observe a non-nil error and
// exit promptly.
func TestEmulatorRead_InFlightUnblockedByClose(t *testing.T) {
	em := NewEmulator(20, 5)

	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := em.Read(buf)
		readDone <- err
	}()

	// Give the reader a beat to block inside pr.Read.
	time.Sleep(20 * time.Millisecond)

	if err := em.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-readDone:
		if err == nil {
			t.Fatalf("Read returned nil error after Close; want io.EOF or ErrClosedPipe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close; pipe not unblocked")
	}
}

// Regression: xterm_divergence_test.go in watchtower runs a drain goroutine
// calling Read() in a loop while the owner calls Close() in defer. With a
// plain bool field for `closed`, -race flagged write/read races. The fix
// promoted `closed` to atomic.Bool; this test runs the same shape repeatedly
// and must be race-free under -race.
func TestEmulatorReadCloseNoRace(t *testing.T) {
	const iterations = 32

	for i := 0; i < iterations; i++ {
		em := NewEmulator(20, 5)

		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 256)
			for {
				if _, err := em.Read(buf); err != nil {
					return
				}
			}
		}()

		// Yield to the scheduler so the reader reaches pr.Read.
		time.Sleep(time.Microsecond)

		if err := em.Close(); err != nil {
			t.Fatalf("iter %d: Close: %v", i, err)
		}

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("iter %d: drain goroutine did not exit", i)
		}
	}
}

// Under concurrent Close + Read callers, exactly one Close must "win" the
// CompareAndSwap and return nil; the rest must also return nil (idempotent)
// and none may panic. Additionally, no Read call may return a nil error
// *after* Close has been observed — i.e., once a reader sees EOF/closed, it
// stays closed.
func TestEmulatorClose_WinnerExactlyOnce(t *testing.T) {
	em := NewEmulator(20, 5)

	const closers = 16
	var firstCloseWinners atomic.Int32

	// Intercept which caller actually did the underlying state transition by
	// reading the `closed` flag before/after. We cannot peek the flag from the
	// test package without exposing internals, so instead verify behavioral
	// invariants: (a) all Close calls return nil, (b) any Read after any
	// Close returns io.EOF.
	var wg sync.WaitGroup
	wg.Add(closers)
	for i := 0; i < closers; i++ {
		go func() {
			defer wg.Done()
			if err := em.Close(); err == nil {
				firstCloseWinners.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := firstCloseWinners.Load(); got != closers {
		t.Fatalf("Close nil-returns: got %d, want %d (all must be idempotent)", got, closers)
	}

	buf := make([]byte, 4)
	if _, err := em.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after mass-Close: got %v, want io.EOF", err)
	}
}
