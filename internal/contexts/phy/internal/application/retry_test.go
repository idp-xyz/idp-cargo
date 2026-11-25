package application

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"testing"
	"time"

	phy "go.idp.xyz/idp-cargo/internal/contexts/phy/internal/domain"
)

// stage is a stand-in for a database whose transaction keeps what it wrote only when it commits: the Outbox and the
// receipts write into the open transaction, and a transaction that fails drops them. It stands in for the rollback of
// PostgreSQL only here; the same rules are run on PostgreSQL by the integration tests.
type stage struct {
	events    []string
	receipts  []string
	open      struct{ events, receipts []string }
	opened    int
	acknowled int
	stored    bool // the command's receipt is found, as after a commit
}

func (s *stage) WithinTransaction(_ context.Context, fn func(context.Context) error) error {
	s.opened++
	s.open.events, s.open.receipts = nil, nil
	if err := fn(context.Background()); err != nil {
		return err
	}
	s.events, s.receipts = append(s.events, s.open.events...), append(s.receipts, s.open.receipts...)
	return nil
}

type stageReceipts struct{ s *stage }

func (r stageReceipts) Find(context.Context, phy.TenantID, string) (Receipt, bool, error) {
	return Receipt{CommandID: "cmd", RequestHash: "h", NewRevision: 3}, r.s.stored, nil
}

func (r stageReceipts) Save(_ context.Context, _ phy.TenantID, rc Receipt) error {
	r.s.open.receipts = append(r.s.open.receipts, rc.CommandID)
	return nil
}

type stageOutbox struct{ s *stage }

func (o stageOutbox) EnqueuePHYEvent(_ context.Context, e phy.Event) error {
	o.s.open.events = append(o.s.open.events, fmt.Sprint(e))
	return nil
}

// changed is an aggregate that has changed in memory: one pending event per change made to it.
type changed struct {
	s       *stage
	changes int
}

func (c *changed) Pending() []phy.Event { return make([]phy.Event, c.changes) }
func (c *changed) Acknowledge()         { c.s.acknowled++ }

func newStage() (Deps, *stage) {
	s := &stage{}
	return Deps{Tx: s, Receipts: stageReceipts{s}, Outbox: stageOutbox{s}, Attempts: 4, Backoff: time.Microsecond}, s
}

func busy() error {
	return fmt.Errorf("%w: load execution X-1 is busy with another command; try again", phy.ErrRetryable)
}

// TestARetryNeverWritesTwice: the first attempt changes its aggregate in memory (two events pending) and then finds a
// stuffing busy; the second succeeds. One event line for the one change, one receipt and one acknowledgement are
// kept: what the first attempt did lived in an aggregate it loaded itself and in a transaction that rolled back. By
// hand: two transactions opened, one change in the second, so one event, one receipt and one acknowledgement.
func TestARetryNeverWritesTwice(t *testing.T) {
	d, s := newStage()
	calls := 0
	r, err := d.write(context.Background(), Scope{TenantID: "T"}, "cmd-1", "hash", func(context.Context) (Receipt, aggregate, error) {
		calls++
		agg := &changed{s: s, changes: 2} // loaded in this attempt and changed in memory
		if calls == 1 {
			return Receipt{}, agg, busy()
		}
		agg.changes = 1 // the second attempt changes its own aggregate once
		return Receipt{NewRevision: 7}, agg, nil
	})
	if err != nil || r.NewRevision != 7 || r.CommandID != "cmd-1" {
		t.Fatalf("receipt %+v, error %v", r, err)
	}
	if calls != 2 || s.opened != 2 || len(s.events) != 1 || len(s.receipts) != 1 || s.acknowled != 1 {
		t.Errorf("%d attempts, %d transactions, %d events, %d receipts, %d acknowledged; want 2, 2, 1, 1, 1", calls, s.opened, len(s.events), len(s.receipts), s.acknowled)
	}
}

// TestOnlyARaceIsRetried: a stale revision (a plain conflict) and any other error end the command at the first attempt;
// a retryable conflict is tried as many times as Attempts says and then answered as the conflict it is; Attempts 1
// is no retry at all.
func TestOnlyARaceIsRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		attempts int
		err      error
		want     int
		is       []error
	}{
		"stale revision":      {4, phy.ErrConflict, 1, []error{phy.ErrConflict}},
		"stale and wrapped":   {4, fmt.Errorf("%w: lot L-1", phy.ErrConflict), 1, []error{phy.ErrConflict}},
		"other error":         {4, errors.New("boom"), 1, nil},
		"busy, four attempts": {4, busy(), 4, []error{phy.ErrConflict, phy.ErrRetryable}},
		"busy, one attempt":   {1, busy(), 1, []error{phy.ErrConflict, phy.ErrRetryable}},
		"busy, default six":   {0, busy(), 6, []error{phy.ErrConflict, phy.ErrRetryable}},
	} {
		t.Run(name, func(t *testing.T) {
			d, s := newStage()
			d.Attempts = tc.attempts
			calls := 0
			_, err := d.write(context.Background(), Scope{TenantID: "T"}, "cmd", "h", func(context.Context) (Receipt, aggregate, error) {
				calls++
				return Receipt{}, nil, tc.err
			})
			if calls != tc.want || err == nil {
				t.Fatalf("%d attempts, want %d: %v", calls, tc.want, err)
			}
			for _, want := range tc.is {
				if !errors.Is(err, want) {
					t.Errorf("error %v is not %v", err, want)
				}
			}
			if len(s.events) != 0 || len(s.receipts) != 0 {
				t.Errorf("a refused command kept %d events and %d receipts", len(s.events), len(s.receipts))
			}
			if errors.Is(err, phy.ErrRetryable) != errors.Is(tc.err, phy.ErrRetryable) {
				t.Errorf("the answer is %v, the cause %v", err, tc.err)
			}
		})
	}
}

// TestTheWaitBetweenAttemptsEndsWithTheContext: the first attempt finds a stuffing busy and the command waits a minute;
// a cancelled context, or one that expires, ends the wait at once with its own error, not the conflict, and no further
// attempt is made.
func TestTheWaitBetweenAttemptsEndsWithTheContext(t *testing.T) {
	for name, newContext := range map[string]func() (context.Context, context.CancelFunc){
		"cancelled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(20*time.Millisecond, cancel)
			return ctx, cancel
		},
		"expired": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 20*time.Millisecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := newStage()
			d.Backoff = time.Minute
			ctx, cancel := newContext()
			defer cancel()
			calls := 0
			start := time.Now()
			_, err := d.write(ctx, Scope{TenantID: "T"}, "cmd", "h", func(context.Context) (Receipt, aggregate, error) {
				calls++
				return Receipt{}, nil, busy()
			})
			if calls != 1 || time.Since(start) > 10*time.Second || !errors.Is(err, ctx.Err()) || errors.Is(err, phy.ErrConflict) {
				t.Fatalf("%d attempts after %v: %v", calls, time.Since(start), err)
			}
		})
	}
}

// TestAFailedAttemptAnswersTheContextsErrorWhenItEnded: the attempt ends the context itself, as a cancellation that
// reaches the driver while it writes does, and then fails with the driver's network error, which carries neither
// context.Canceled nor context.DeadlineExceeded. The command is answered the context's error, after one attempt, and
// not the network error. A failed attempt of a context that has not ended keeps its own error.
func TestAFailedAttemptAnswersTheContextsErrorWhenItEnded(t *testing.T) {
	networkErrors := map[string]error{
		"net.OpError": &net.OpError{Op: "write", Net: "tcp", Err: errors.New("i/o timeout")},
		"closed pipe": fmt.Errorf("write failed: %w", io.ErrClosedPipe),
	}
	for name, network := range networkErrors {
		for how, want := range map[string]error{"cancelled": context.Canceled, "expired": context.DeadlineExceeded} {
			t.Run(name+", "+how, func(t *testing.T) {
				d, _ := newStage()
				ctx, cancel := context.WithCancel(context.Background())
				if how == "expired" {
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				}
				defer cancel()
				calls := 0
				_, err := d.retrying(ctx, func() (Receipt, error) {
					calls++
					cancel()
					<-ctx.Done()
					return Receipt{}, network
				})
				if calls != 1 || !errors.Is(err, want) || errors.Is(err, network) {
					t.Fatalf("%d attempts: %v, want %v after 1 attempt", calls, err, want)
				}
			})
		}
	}
	t.Run("a live context keeps the attempt's error", func(t *testing.T) {
		d, _ := newStage()
		network := &net.OpError{Op: "write", Net: "tcp", Err: errors.New("i/o timeout")}
		_, err := d.retrying(context.Background(), func() (Receipt, error) { return Receipt{}, network })
		if !errors.Is(err, network) {
			t.Fatalf("%v, want the network error", err)
		}
	})
}

// TestTheRetryWaitsLongerEachTime: the waits double, each with up to its own length added: from a first wait of 1 ms
// the first wait lies in [1, 2) ms, the second in [2, 4) ms and the third in [4, 8) ms. The time between the attempts
// can only be bounded from below, by those least waits.
func TestTheRetryWaitsLongerEachTime(t *testing.T) {
	d, _ := newStage()
	d.Backoff = time.Millisecond
	var at []time.Time
	_, err := d.write(context.Background(), Scope{TenantID: "T"}, "cmd", "h", func(context.Context) (Receipt, aggregate, error) {
		at = append(at, time.Now())
		return Receipt{}, nil, busy()
	})
	if !errors.Is(err, phy.ErrConflict) || len(at) != 4 {
		t.Fatalf("%d attempts: %v", len(at), err)
	}
	for i, least := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond} {
		if got := at[i+1].Sub(at[i]); got < least {
			t.Errorf("wait %d was %v, at least %v", i+1, got, least)
		}
	}
}

// TestAReplayAcknowledgesNothing: a command written once and then asked again with the same id and payload answers its
// stored receipt, runs nothing and acknowledges no aggregate: what one command committed does not reach the next
// through the wrapper. By hand: one acknowledgement for the first command, none for the replay, one mutate call.
func TestAReplayAcknowledgesNothing(t *testing.T) {
	d, s := newStage()
	calls := 0
	mutate := func(context.Context) (Receipt, aggregate, error) {
		calls++
		return Receipt{NewRevision: 3}, &changed{s: s, changes: 1}, nil
	}
	if _, err := d.write(context.Background(), Scope{TenantID: "T"}, "cmd", "h", mutate); err != nil {
		t.Fatal(err)
	}
	s.stored = true
	r, err := d.write(context.Background(), Scope{TenantID: "T"}, "cmd", "h", mutate)
	if err != nil || r.NewRevision != 3 || calls != 1 || s.acknowled != 1 || len(s.events) != 1 {
		t.Errorf("replay: %+v %v, %d mutate calls, %d acknowledged, %d events; want the stored receipt, 1, 1, 1", r, err, calls, s.acknowled, len(s.events))
	}
}

// waitsOf runs a command that is refused every time, with the sleep and the random part injected, and returns the waits
// it asked for. The random part is the largest the source can give (n - 1 ns) or none.
func waitsOf(t *testing.T, d Deps, jitter func(time.Duration) time.Duration) []time.Duration {
	t.Helper()
	var waits []time.Duration
	d.sleep = func(_ context.Context, w time.Duration) error { waits = append(waits, w); return nil }
	d.jitter = jitter
	_, err := d.write(context.Background(), Scope{TenantID: "T"}, "cmd", "h", func(context.Context) (Receipt, aggregate, error) {
		return Receipt{}, nil, busy()
	})
	if !errors.Is(err, phy.ErrRetryable) {
		t.Fatalf("answered %v", err)
	}
	return waits
}

func none(time.Duration) time.Duration   { return 0 }
func most(n time.Duration) time.Duration { return n - 1 }

// TestTheDefaultWaitsAndTheirJitter, by hand, with the source of the random part replaced (no timing): the default is 6
// attempts and a first wait of 5 ms, doubled each time, so five waits of 5, 10, 20, 40 and 80 ms (155 ms in all) when
// the random part is none, and each wait plus less than itself, 5 ms + 4.999999 ms and so on (just under 310 ms in
// all), when it is the most the source gives. A wait is therefore always between its base and twice its base.
func TestTheDefaultWaitsAndTheirJitter(t *testing.T) {
	d, _ := newStage()
	d.Attempts, d.Backoff = 0, 0
	ms := time.Millisecond
	if got, want := waitsOf(t, d, none), []time.Duration{5 * ms, 10 * ms, 20 * ms, 40 * ms, 80 * ms}; !slices.Equal(got, want) {
		t.Errorf("waits without jitter %v, want %v", got, want)
	}
	loud := waitsOf(t, d, most)
	for i, base := range []time.Duration{5 * ms, 10 * ms, 20 * ms, 40 * ms, 80 * ms} {
		if loud[i] != 2*base-1 {
			t.Errorf("wait %d with the most jitter is %v, want %v: below twice its base", i+1, loud[i], 2*base-1)
		}
	}
}

// TestAGivenBackoffIsTheFirstWaitAndNoWaitPassesASecond: a first wait of 400 ms doubles to 800 ms and is then held at
// one second (the doubling would give 1.6 s, 3.2 s, ...); a first wait of more than a second, and the largest duration
// there is, are held at one second as well, and a negative one is the default of 5 ms. With the random part none these
// are the waits, and with the most it gives none is twice a second or more; nothing overflows.
func TestAGivenBackoffIsTheFirstWaitAndNoWaitPassesASecond(t *testing.T) {
	ms, second := time.Millisecond, time.Second
	for name, tc := range map[string]struct {
		backoff time.Duration
		want    []time.Duration
	}{
		"400 ms":         {400 * ms, []time.Duration{400 * ms, 800 * ms, second, second, second}},
		"an hour":        {time.Hour, []time.Duration{second, second, second, second, second}},
		"the largest":    {time.Duration(math.MaxInt64), []time.Duration{second, second, second, second, second}},
		"negative":       {-time.Hour, []time.Duration{5 * ms, 10 * ms, 20 * ms, 40 * ms, 80 * ms}},
		"exactly second": {second, []time.Duration{second, second, second, second, second}},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := newStage()
			d.Attempts, d.Backoff = 6, tc.backoff
			if got := waitsOf(t, d, none); !slices.Equal(got, tc.want) {
				t.Errorf("waits %v, want %v", got, tc.want)
			}
			for _, w := range waitsOf(t, d, most) {
				if w <= 0 || w >= 2*second {
					t.Errorf("a wait of %v with the most jitter", w)
				}
			}
		})
	}
	d, _ := newStage()
	d.Attempts, d.Backoff = 50, 400*ms
	if waits := waitsOf(t, d, none); len(waits) != 49 || waits[len(waits)-1] != second {
		t.Errorf("fifty attempts: %d waits, the last %v, want 49 and one second: the doubling is held", len(waits), waits[len(waits)-1])
	}
}

// TestTheProductionJitterIsBetweenTheBaseAndTwiceIt runs the random source that runs in production (Deps.jitter is left
// nil, only the sleep is injected, so nothing is waited for): 400 commands that are refused every time ask for five
// waits each, 2,000 waits with bases of 5, 10, 20, 40 and 80 ms. Every wait lies from its base up to, not including,
// twice its base; for each base some wait is above one and a half times it and some below (a source that is stuck, or
// sits in one half, is not a jitter), and not all are the base itself. A correct build fails the last three checks only
// if every one of 400 draws for a base falls in one half of its range, or is exactly zero in 5 to 80 million: each case
// has a probability below 10^-100, so below one in a billion by far. A jitter removed fails the lower checks at once,
// and one tripled breaks the upper bound with the first draw in the upper third.
func TestTheProductionJitterIsBetweenTheBaseAndTwiceIt(t *testing.T) {
	ms := time.Millisecond
	bases := []time.Duration{5 * ms, 10 * ms, 20 * ms, 40 * ms, 80 * ms}
	high, low, atBase := make([]int, len(bases)), make([]int, len(bases)), make([]int, len(bases))
	for run := 0; run < 400; run++ {
		d, _ := newStage()
		d.Attempts, d.Backoff = 0, 0
		waits := waitsOf(t, d, nil)
		if len(waits) != len(bases) {
			t.Fatalf("%d waits, want %d", len(waits), len(bases))
		}
		for i, w := range waits {
			if w < bases[i] || w >= 2*bases[i] {
				t.Fatalf("wait %d is %v, want from %v up to, not including, %v", i+1, w, bases[i], 2*bases[i])
			}
			switch {
			case w == bases[i]:
				atBase[i]++
			case w-bases[i] > bases[i]/2:
				high[i]++
			default:
				low[i]++
			}
		}
	}
	for i, base := range bases {
		if high[i] == 0 || low[i]+atBase[i] == 0 || atBase[i] == 400 {
			t.Errorf("base %v: %d waits above one and a half times it, %d below, %d at the base itself, of 400", base, high[i], low[i], atBase[i])
		}
	}
}
