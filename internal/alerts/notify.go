package alerts

import (
	"context"
	"errors"
	"sync"

	"github.com/Real-kia/payesh/internal/contracts"
)

const maxPendingNotifications = 128

type notificationJob struct {
	destination NotificationDestination
	event       contracts.AlertHistoryEvent
	completed   func(error)
}

// DeliveryFailure reports a terminal delivery failure after the notifier's
// bounded retry policy is exhausted. Destination details are deliberately not
// included because they may contain owner-configured credentials.
type DeliveryFailure struct {
	EventID  string
	AlertID  string
	ServerID contracts.ServerID
	Err      error
}

// DeliveryQueue is a small bounded asynchronous delivery adapter. A full
// queue fails fast so an unavailable webhook cannot consume unbounded hub
// memory or delay metric ingestion.
type DeliveryQueue struct {
	send      func(context.Context, NotificationDestination, contracts.AlertHistoryEvent) error
	jobs      chan notificationJob
	failures  chan DeliveryFailure
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	wg        sync.WaitGroup
}

func NewDeliveryQueue(notifier Notifier, workers int) *DeliveryQueue {
	if workers < 1 {
		workers = 1
	}
	if workers > 8 {
		workers = 8
	}
	queueCtx, cancel := context.WithCancel(context.Background())
	queue := &DeliveryQueue{send: notifier.SendWithRetry, jobs: make(chan notificationJob, maxPendingNotifications), failures: make(chan DeliveryFailure, maxPendingNotifications), done: make(chan struct{}), ctx: queueCtx, cancel: cancel}
	queue.wg.Add(workers)
	for index := 0; index < workers; index++ {
		go queue.worker()
	}
	return queue
}

func (q *DeliveryQueue) worker() {
	defer q.wg.Done()
	for job := range q.jobs {
		err := q.send(q.ctx, job.destination, job.event)
		if job.completed != nil {
			job.completed(err)
		}
		if err != nil {
			failure := DeliveryFailure{EventID: job.event.ID, AlertID: job.event.AlertID, ServerID: job.event.ServerID, Err: err}
			select {
			case q.failures <- failure:
			default:
				// The result callback already leaves alert cadence retryable.
				// Failure observation is bounded as well, so an unattended
				// diagnostics consumer cannot stall delivery workers.
			}
		}
	}
}

func (q *DeliveryQueue) Enqueue(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent) error {
	return q.enqueue(ctx, destination, event, nil)
}

func (q *DeliveryQueue) enqueue(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent, completed func(error)) error {
	if q == nil {
		return errors.New("notification queue is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return errors.New("notification queue is closed")
	}
	select {
	case q.jobs <- notificationJob{destination: destination, event: event, completed: completed}:
		q.mu.Unlock()
		return nil
	case <-ctx.Done():
		q.mu.Unlock()
		return ctx.Err()
	default:
		q.mu.Unlock()
		return errors.New("notification queue is full")
	}
}

// Failures exposes a bounded diagnostics stream for terminal failures. The
// records contain durable alert identities and errors, never destinations or
// notification secrets.
func (q *DeliveryQueue) Failures() <-chan DeliveryFailure {
	if q == nil {
		return nil
	}
	return q.failures
}

func (q *DeliveryQueue) Close() {
	if q == nil {
		return
	}
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		close(q.jobs)
		q.mu.Unlock()
		q.cancel()
		q.wg.Wait()
		close(q.failures)
		close(q.done)
	})
}
