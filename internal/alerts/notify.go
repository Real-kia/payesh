package alerts

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

const (
	maxPendingNotifications = 128
	// maxParkedPerDestination bounds how many notifications a rate-limited
	// destination holds while waiting to send one grouped message.
	maxParkedPerDestination = 200
	// maxDigestItems is how many folded notifications a digest lists.
	maxDigestItems = 20
	// digestHold lets a burst coalesce before the grouped message is sent.
	digestHold = 5 * time.Second
	// burstTokens is how many notifications a destination may send at once
	// before the per-minute rate applies.
	burstTokens = 10
)

type notificationJob struct {
	destination  NotificationDestination
	notification Notification
	completed    func(error)
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

type destinationLimiter struct {
	tokens  float64
	last    time.Time
	pending []notificationJob
	timer   *time.Timer
}

// DeliveryQueue is a small bounded asynchronous delivery adapter. A full
// queue fails fast so an unavailable webhook cannot consume unbounded hub
// memory or delay metric ingestion. Each destination is also rate limited:
// notifications over its per-minute budget are held briefly and sent as one
// grouped message, so a single outage cannot flood a chat channel.
type DeliveryQueue struct {
	send func(context.Context, NotificationDestination, Notification) error
	// Observer, when set before the first Enqueue, receives the outcome of
	// every delivery made by the default sender.
	Observer func(NotificationDestination, DeliveryResult)

	jobs      chan notificationJob
	failures  chan DeliveryFailure
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	wg        sync.WaitGroup

	limMu     sync.Mutex
	limiters  map[string]*destinationLimiter
	limClosed bool
	flushWG   sync.WaitGroup
	now       func() time.Time
	hold      time.Duration
	digestSeq int
}

func NewDeliveryQueue(notifier Notifier, workers int) *DeliveryQueue {
	if workers < 1 {
		workers = 1
	}
	if workers > 8 {
		workers = 8
	}
	queueCtx, cancel := context.WithCancel(context.Background())
	queue := &DeliveryQueue{jobs: make(chan notificationJob, maxPendingNotifications), failures: make(chan DeliveryFailure, maxPendingNotifications), done: make(chan struct{}), ctx: queueCtx, cancel: cancel,
		limiters: make(map[string]*destinationLimiter), now: time.Now, hold: digestHold}
	queue.send = func(ctx context.Context, destination NotificationDestination, notification Notification) error {
		result := notifier.Deliver(ctx, destination, notification)
		if queue.Observer != nil {
			queue.Observer(destination, result)
		}
		return result.Err()
	}
	queue.wg.Add(workers)
	for index := 0; index < workers; index++ {
		go queue.worker()
	}
	return queue
}

func (q *DeliveryQueue) worker() {
	defer q.wg.Done()
	for job := range q.jobs {
		if job.notification.Kind != NotifyTest && q.park(job) {
			continue
		}
		q.finish(job, q.send(q.ctx, job.destination, job.notification))
	}
}

func (q *DeliveryQueue) finish(job notificationJob, err error) {
	if job.completed != nil {
		job.completed(err)
	}
	if err != nil {
		q.reportFailure(job, err)
	}
}

func (q *DeliveryQueue) reportFailure(job notificationJob, err error) {
	failure := DeliveryFailure{EventID: job.notification.Event.ID, AlertID: job.notification.Event.AlertID, ServerID: job.notification.Event.ServerID, Err: err}
	select {
	case q.failures <- failure:
	default:
		// The result callback already leaves alert cadence retryable.
		// Failure observation is bounded as well, so an unattended
		// diagnostics consumer cannot stall delivery workers.
	}
}

// park applies the destination's rate limit. It returns false when the
// notification may be sent now (a token was spent), and true when it was held
// for the next grouped message.
func (q *DeliveryQueue) park(job notificationJob) bool {
	destination := job.destination.withDefaults()
	key := destination.limiterKey()
	q.limMu.Lock()
	defer q.limMu.Unlock()
	if q.limClosed {
		return false
	}
	limiter := q.limiters[key]
	if limiter == nil {
		limiter = &destinationLimiter{tokens: burstTokens, last: q.now()}
		q.limiters[key] = limiter
	}
	q.refill(limiter, destination)
	if limiter.tokens >= 1 && len(limiter.pending) == 0 {
		limiter.tokens--
		return false
	}
	if len(limiter.pending) >= maxParkedPerDestination {
		// Over the hard bound: fail this one so the engine retries it later
		// instead of growing memory.
		go q.finish(job, errors.New("notification rate limit exceeded"))
		return true
	}
	limiter.pending = append(limiter.pending, job)
	if limiter.timer == nil {
		q.scheduleFlushLocked(key, limiter, destination)
	}
	return true
}

func (q *DeliveryQueue) refill(limiter *destinationLimiter, destination NotificationDestination) {
	now := q.now()
	if elapsed := now.Sub(limiter.last); elapsed > 0 {
		limiter.tokens += elapsed.Seconds() * float64(destination.MaxPerMinute) / 60
		limiter.last = now
	}
	burst := float64(burstTokens)
	if float64(destination.MaxPerMinute) < burst {
		burst = float64(destination.MaxPerMinute)
	}
	if limiter.tokens > burst {
		limiter.tokens = burst
	}
}

func (q *DeliveryQueue) scheduleFlushLocked(key string, limiter *destinationLimiter, destination NotificationDestination) {
	delay := q.hold
	if limiter.tokens < 1 {
		need := time.Duration((1 - limiter.tokens) / float64(destination.MaxPerMinute) * 60 * float64(time.Second))
		if need > delay {
			delay = need
		}
	}
	limiter.timer = time.AfterFunc(delay, func() { q.flush(key) })
}

// flush sends one grouped message for everything a destination parked.
func (q *DeliveryQueue) flush(key string) {
	q.limMu.Lock()
	limiter := q.limiters[key]
	if limiter == nil || q.limClosed {
		q.limMu.Unlock()
		return
	}
	limiter.timer = nil
	if len(limiter.pending) == 0 {
		q.limMu.Unlock()
		return
	}
	destination := limiter.pending[len(limiter.pending)-1].destination.withDefaults()
	q.refill(limiter, destination)
	if limiter.tokens < 1 {
		q.scheduleFlushLocked(key, limiter, destination)
		q.limMu.Unlock()
		return
	}
	limiter.tokens--
	items := limiter.pending
	limiter.pending = nil
	q.digestSeq++
	sequence := q.digestSeq
	q.flushWG.Add(1)
	q.limMu.Unlock()
	defer q.flushWG.Done()

	digest := Notification{Kind: NotifyDigest, Event: items[len(items)-1].notification.Event}
	digest.Event.ID = "digest-" + strconv.Itoa(sequence) + "-" + digest.Event.ID
	for index, item := range items {
		if index < maxDigestItems {
			digest.Items = append(digest.Items, item.notification)
		} else {
			digest.Dropped++
		}
	}
	err := q.send(q.ctx, destination, digest)
	for _, item := range items {
		q.finish(item, err)
	}
}

func (q *DeliveryQueue) Enqueue(ctx context.Context, destination NotificationDestination, event contracts.AlertHistoryEvent) error {
	return q.enqueue(ctx, destination, Notification{Event: event, Kind: kindFromEvent(event)}, nil)
}

// EnqueueNotification queues a notification that carries rule and server
// context for templates and presets.
func (q *DeliveryQueue) EnqueueNotification(ctx context.Context, destination NotificationDestination, notification Notification) error {
	return q.enqueue(ctx, destination, notification, nil)
}

func (q *DeliveryQueue) enqueue(ctx context.Context, destination NotificationDestination, notification Notification, completed func(error)) error {
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
	case q.jobs <- notificationJob{destination: destination, notification: notification, completed: completed}:
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
		q.limMu.Lock()
		q.limClosed = true
		var parked []notificationJob
		for _, limiter := range q.limiters {
			if limiter.timer != nil {
				limiter.timer.Stop()
			}
			parked = append(parked, limiter.pending...)
			limiter.pending = nil
		}
		q.limMu.Unlock()
		q.flushWG.Wait()
		for _, job := range parked {
			if job.completed != nil {
				job.completed(errors.New("notification queue closed"))
			}
		}
		close(q.failures)
		close(q.done)
	})
}
