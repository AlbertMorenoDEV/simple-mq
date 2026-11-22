package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlbertMorenoDEV/simple-mq/internal/model"
	"github.com/AlbertMorenoDEV/simple-mq/internal/storage"
)

// DeliveryMode controls how published messages are distributed to active subscribers.
type DeliveryMode string

const (
	// ModeHybrid fans out to all currently waiting subscribers, or enqueues FIFO
	// when no subscribers are waiting (backwards-compatible default).
	ModeHybrid DeliveryMode = "hybrid"
	// ModeQueue delivers each message to at most one consumer (competing consumers / round-robin).
	ModeQueue DeliveryMode = "queue"
	// ModeFanOut broadcasts to all active subscribers.
	ModeFanOut DeliveryMode = "fanout"
)

// ParseDeliveryMode parses a delivery mode string, defaulting to fallback when empty.
func ParseDeliveryMode(raw string, fallback DeliveryMode) (DeliveryMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return fallback, nil
	case string(ModeHybrid):
		return ModeHybrid, nil
	case string(ModeQueue):
		return ModeQueue, nil
	case string(ModeFanOut):
		return ModeFanOut, nil
	default:
		return "", fmt.Errorf("invalid delivery mode %q: must be hybrid, queue, or fanout", raw)
	}
}

var (
	ErrQueueFull        = errors.New("queue capacity reached")
	ErrMaxQueuesReached = errors.New("maximum number of queues reached")
	ErrReceiptNotFound  = errors.New("in-flight message receipt not found")
	ErrBrokerClosed     = errors.New("broker is closed")
	ErrTimeout          = errors.New("consume timed out")
)

// Options configures Broker limits and defaults.
type Options struct {
	DeliveryMode      DeliveryMode
	MaxQueueDepth     int
	MaxQueues         int
	VisibilityTimeout time.Duration
	MaxRetries        int
	ReapInterval      time.Duration
}

// DefaultOptions returns sensible defaults.
func DefaultOptions() Options {
	return Options{
		DeliveryMode:      ModeHybrid,
		MaxQueueDepth:     100_000,
		MaxQueues:         10_000,
		VisibilityTimeout: 30 * time.Second,
		MaxRetries:        3,
		ReapInterval:      250 * time.Millisecond,
	}
}

// ConsumeOptions customizes a single Consume, ConsumeBatch, or SubscribeStream call.
type ConsumeOptions struct {
	// Timeout limits how long to wait when the queue is empty.
	// A negative duration waits indefinitely until ctx is canceled.
	// Zero performs an immediate non-blocking poll (returns ErrTimeout if empty).
	Timeout           time.Duration
	AckRequired       bool
	VisibilityTimeout time.Duration
	Mode              DeliveryMode
}

// QueueInfo summarizes the runtime state of a single named queue.
type QueueInfo struct {
	Name              string `json:"name"`
	Depth             int    `json:"depth"`
	Delayed           int    `json:"delayed"`
	InFlight          int    `json:"in_flight"`
	ActiveSubscribers int    `json:"active_subscribers"`
}

// MetricsSnapshot holds point-in-time broker metrics.
type MetricsSnapshot struct {
	PublishedTotal    int64 `json:"published_total"`
	DeliveredTotal    int64 `json:"delivered_total"`
	AckedTotal        int64 `json:"acked_total"`
	NackedTotal       int64 `json:"nacked_total"`
	RedeliveredTotal  int64 `json:"redelivered_total"`
	DeadLetteredTotal int64 `json:"dead_lettered_total"`
	ExpiredTotal      int64 `json:"expired_total"`
	DroppedTotal      int64 `json:"dropped_total"`
	QueueCount        int   `json:"queue_count"`
	TotalQueued       int   `json:"total_queued"`
	TotalDelayed      int   `json:"total_delayed"`
	TotalInFlight     int   `json:"total_in_flight"`
	ActiveSubscribers int   `json:"active_subscribers"`
}

type delivery struct {
	msg  model.Message
	sole bool
}

type subscriber struct {
	id         uint64
	queue      string
	ch         chan delivery
	stream     bool
	ack        bool
	visibility time.Duration
	mode       DeliveryMode
}

type inflightItem struct {
	queue    string
	msg      model.Message
	deadline time.Time
}

// Broker is the core concurrency-safe message queue engine.
//
// Lock ordering invariant: ALWAYS acquire persistMu before mu when both are needed.
// Never acquire persistMu while holding mu.
type Broker struct {
	opts   Options
	store  storage.Store
	logger *slog.Logger

	persistMu sync.Mutex
	mu        sync.Mutex

	queues      map[string]*RingBuffer[model.Message]
	delayed     map[string][]model.Message
	subscribers map[string][]*subscriber
	inflight    map[string]*inflightItem
	rrIndex     map[string]uint64
	nextSubID   uint64

	closed   bool
	stopCh   chan struct{}
	reaperWg sync.WaitGroup

	// Atomic metrics counters
	publishedTotal    atomic.Int64
	deliveredTotal    atomic.Int64
	ackedTotal        atomic.Int64
	nackedTotal       atomic.Int64
	redeliveredTotal  atomic.Int64
	deadLetteredTotal atomic.Int64
	expiredTotal      atomic.Int64
	droppedTotal      atomic.Int64
}

// New initializes a Broker, loading any persisted messages from store and
// starting the background reaper for delayed messages, TTL expiry, and ACK timeouts.
func New(opts Options, store storage.Store, logger *slog.Logger) (*Broker, error) {
	defaults := DefaultOptions()
	if opts.DeliveryMode == "" {
		opts.DeliveryMode = defaults.DeliveryMode
	}
	if opts.MaxQueueDepth <= 0 {
		opts.MaxQueueDepth = defaults.MaxQueueDepth
	}
	if opts.MaxQueues <= 0 {
		opts.MaxQueues = defaults.MaxQueues
	}
	if opts.VisibilityTimeout <= 0 {
		opts.VisibilityTimeout = defaults.VisibilityTimeout
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = defaults.MaxRetries
	}
	if opts.ReapInterval <= 0 {
		opts.ReapInterval = defaults.ReapInterval
	}
	if store == nil {
		store = storage.NewMemoryStore()
	}
	if logger == nil {
		logger = slog.Default()
	}

	b := &Broker{
		opts:        opts,
		store:       store,
		logger:      logger,
		queues:      make(map[string]*RingBuffer[model.Message]),
		delayed:     make(map[string][]model.Message),
		subscribers: make(map[string][]*subscriber),
		inflight:    make(map[string]*inflightItem),
		rrIndex:     make(map[string]uint64),
		stopCh:      make(chan struct{}),
	}

	if err := b.loadFromStore(); err != nil {
		return nil, fmt.Errorf("load persisted queues: %w", err)
	}

	b.reaperWg.Add(1)
	go b.reaperLoop()

	return b, nil
}

func (b *Broker) loadFromStore() error {
	snap, err := b.store.Load()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	loadedQueues := 0
	loadedMsgs := 0

	for qName, msgs := range snap.Queues {
		if len(msgs) == 0 {
			continue
		}
		rb := NewRingBuffer[model.Message]()
		for _, m := range msgs {
			if m.IsExpired(now) {
				b.expiredTotal.Add(1)
				continue
			}
			m.Receipt = ""
			rb.Push(m)
			loadedMsgs++
		}
		if rb.Len() > 0 {
			b.queues[qName] = rb
			loadedQueues++
		}
	}

	for qName, msgs := range snap.Delayed {
		for _, m := range msgs {
			if m.IsExpired(now) {
				b.expiredTotal.Add(1)
				continue
			}
			m.Receipt = ""
			if m.IsDelayed(now) {
				b.delayed[qName] = append(b.delayed[qName], m)
			} else {
				b.getOrCreateQueueLocked(qName).Push(m)
			}
			loadedMsgs++
		}
	}

	if loadedMsgs > 0 {
		b.logger.Info("loaded persisted state", "queues", loadedQueues, "messages", loadedMsgs)
	}
	return nil
}

// Close stops background workers, flushes a final compacted snapshot, and closes the store.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.stopCh)
	b.mu.Unlock()

	b.reaperWg.Wait()

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	snap := b.snapshotState()
	saveErr := b.store.SaveSnapshot(snap)
	closeErr := b.store.Close()
	if saveErr != nil {
		return saveErr
	}
	return closeErr
}

// Publish validates and publishes a single message to queueName.
func (b *Broker) Publish(queueName string, msg model.Message) (model.Message, error) {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return model.Message{}, err
	}
	now := time.Now().UTC()
	if err := msg.NormalizeAndValidate(now); err != nil {
		return model.Message{}, err
	}

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return model.Message{}, ErrBrokerClosed
	}
	if err := b.checkLimitsLocked(qName, 1); err != nil {
		b.mu.Unlock()
		return model.Message{}, err
	}

	needPersist, isDelayed := b.enqueueOrDispatchLocked(qName, msg, now)
	b.mu.Unlock()

	b.publishedTotal.Add(1)

	if needPersist {
		if err := b.persistPublishLocked(qName, msg, isDelayed); err != nil {
			b.logger.Error("failed to persist published message", "queue", qName, "id", msg.ID, "err", err)
			return msg, err
		}
	}

	return msg, nil
}

// PublishBatch validates and publishes multiple messages atomically with respect to limits.
func (b *Broker) PublishBatch(queueName string, msgs []model.Message) ([]model.Message, error) {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return []model.Message{}, nil
	}

	now := time.Now().UTC()
	normalized := make([]model.Message, len(msgs))
	for i := range msgs {
		m := msgs[i]
		if err := m.NormalizeAndValidate(now); err != nil {
			return nil, fmt.Errorf("message[%d]: %w", i, err)
		}
		normalized[i] = m
	}

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBrokerClosed
	}
	if err := b.checkLimitsLocked(qName, len(normalized)); err != nil {
		b.mu.Unlock()
		return nil, err
	}

	type persistItem struct {
		msg     model.Message
		delayed bool
	}
	var toPersist []persistItem
	for _, m := range normalized {
		needPersist, isDelayed := b.enqueueOrDispatchLocked(qName, m, now)
		if needPersist {
			toPersist = append(toPersist, persistItem{msg: m, delayed: isDelayed})
		}
	}
	b.mu.Unlock()

	b.publishedTotal.Add(int64(len(normalized)))

	for _, item := range toPersist {
		if err := b.store.RecordPublish(qName, item.msg, item.delayed); err != nil {
			b.logger.Error("failed to persist batch message", "queue", qName, "id", item.msg.ID, "err", err)
			return normalized, err
		}
	}
	if len(toPersist) > 0 && b.store.NeedsCompaction() {
		if err := b.store.SaveSnapshot(b.snapshotState()); err != nil {
			return normalized, err
		}
	}

	return normalized, nil
}

// enqueueOrDispatchLocked either schedules the message as delayed, delivers it
// to active subscriber(s), or appends it to the queue ring buffer.
// Caller must hold b.mu (and b.persistMu).
func (b *Broker) enqueueOrDispatchLocked(qName string, msg model.Message, now time.Time) (needPersist bool, isDelayed bool) {
	if msg.IsDelayed(now) {
		b.delayed[qName] = append(b.delayed[qName], msg)
		return true, true
	}

	subs := b.subscribers[qName]
	if len(subs) == 0 {
		b.getOrCreateQueueLocked(qName).Push(msg)
		return true, false
	}

	// Determine whether to deliver to one subscriber (competing consumers) or fan-out.
	useSingleConsumer := b.opts.DeliveryMode == ModeQueue
	if !useSingleConsumer {
		// If all waiting subscribers explicitly requested queue mode, honor it.
		allQueue := true
		for _, s := range subs {
			if s.mode != ModeQueue {
				allQueue = false
				break
			}
		}
		useSingleConsumer = allQueue
	}

	if useSingleConsumer {
		idx := int(b.rrIndex[qName] % uint64(len(subs)))
		b.rrIndex[qName]++
		sub := subs[idx]
		out := b.prepareDeliveryLocked(qName, msg, sub, now)
		select {
		case sub.ch <- delivery{msg: out, sole: true}:
			b.deliveredTotal.Add(1)
			if !sub.stream {
				b.removeSubscriberAtLocked(qName, idx)
			}
			// Persist only if in ACK mode so a crash before ACK restores the message.
			return sub.ack, false
		default:
			// Subscriber buffer full; fall back to queueing so no message is lost.
			b.clearInflightLocked(out.Receipt)
			b.droppedTotal.Add(1)
			b.getOrCreateQueueLocked(qName).Push(msg)
			return true, false
		}
	}

	// Fan-out to all active subscribers.
	deliveredCount := 0
	anyAck := false
	remaining := subs[:0]
	sole := len(subs) == 1

	for _, sub := range subs {
		out := b.prepareDeliveryLocked(qName, msg.Clone(), sub, now)
		select {
		case sub.ch <- delivery{msg: out, sole: sole}:
			deliveredCount++
			b.deliveredTotal.Add(1)
			if sub.ack {
				anyAck = true
			}
			if sub.stream {
				remaining = append(remaining, sub)
			}
		default:
			b.clearInflightLocked(out.Receipt)
			b.droppedTotal.Add(1)
			b.logger.Warn("subscriber buffer full, dropping fan-out copy", "queue", qName, "sub_id", sub.id)
			if sub.stream {
				remaining = append(remaining, sub)
			}
		}
	}

	if len(remaining) == 0 {
		delete(b.subscribers, qName)
	} else {
		b.subscribers[qName] = remaining
	}

	if deliveredCount == 0 {
		b.getOrCreateQueueLocked(qName).Push(msg)
		return true, false
	}
	return anyAck, false
}

func (b *Broker) prepareDeliveryLocked(qName string, msg model.Message, sub *subscriber, now time.Time) model.Message {
	msg.DeliveryCount++
	if sub.ack {
		vis := sub.visibility
		if vis <= 0 {
			vis = b.opts.VisibilityTimeout
		}
		receipt := model.NewUUID()
		msg.Receipt = receipt
		b.inflight[receipt] = &inflightItem{
			queue:    qName,
			msg:      msg.Clone(),
			deadline: now.Add(vis),
		}
	} else {
		msg.Receipt = ""
	}
	return msg
}

func (b *Broker) clearInflightLocked(receipt string) {
	if receipt != "" {
		delete(b.inflight, receipt)
	}
}

// Consume pops the next available message from queueName, or long-polls until
// one arrives, ctx is canceled, or opts.Timeout expires.
func (b *Broker) Consume(ctx context.Context, queueName string, opts ConsumeOptions) (*model.Message, error) {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = b.opts.DeliveryMode
	}

	b.persistMu.Lock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.persistMu.Unlock()
		return nil, ErrBrokerClosed
	}

	now := time.Now().UTC()
	if msg, ok := b.popReadyLocked(qName, now); ok {
		msg.DeliveryCount++
		if opts.AckRequired {
			vis := opts.VisibilityTimeout
			if vis <= 0 {
				vis = b.opts.VisibilityTimeout
			}
			receipt := model.NewUUID()
			msg.Receipt = receipt
			b.inflight[receipt] = &inflightItem{
				queue:    qName,
				msg:      msg.Clone(),
				deadline: now.Add(vis),
			}
			b.mu.Unlock()
			b.persistMu.Unlock()
			b.deliveredTotal.Add(1)
			return &msg, nil
		}

		msg.Receipt = ""
		b.mu.Unlock()
		b.deliveredTotal.Add(1)
		if err := b.persistConsumeLocked(qName, msg.ID); err != nil {
			b.logger.Error("failed to persist consume", "queue", qName, "id", msg.ID, "err", err)
		}
		b.persistMu.Unlock()
		return &msg, nil
	}

	// Non-blocking poll when Timeout == 0.
	if opts.Timeout == 0 {
		b.mu.Unlock()
		b.persistMu.Unlock()
		return nil, ErrTimeout
	}

	// Register one-shot subscriber atomically while still holding b.mu.
	b.nextSubID++
	sub := &subscriber{
		id:         b.nextSubID,
		queue:      qName,
		ch:         make(chan delivery, 1),
		stream:     false,
		ack:        opts.AckRequired,
		visibility: opts.VisibilityTimeout,
		mode:       opts.Mode,
	}
	b.subscribers[qName] = append(b.subscribers[qName], sub)
	b.mu.Unlock()
	b.persistMu.Unlock()

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if opts.Timeout > 0 {
		timer = time.NewTimer(opts.Timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case d := <-sub.ch:
		return &d.msg, nil
	case <-ctx.Done():
		b.cleanupCanceledSubscriber(sub)
		return nil, ctx.Err()
	case <-timeoutCh:
		b.cleanupCanceledSubscriber(sub)
		return nil, ErrTimeout
	case <-b.stopCh:
		b.cleanupCanceledSubscriber(sub)
		return nil, ErrBrokerClosed
	}
}

// ConsumeBatch pops up to max ready messages from queueName. If the queue is
// currently empty, it waits for the first message according to opts.Timeout,
// then drains any additional immediately-ready messages up to max.
func (b *Broker) ConsumeBatch(ctx context.Context, queueName string, max int, opts ConsumeOptions) ([]model.Message, error) {
	if max <= 1 {
		m, err := b.Consume(ctx, queueName, opts)
		if err != nil {
			return nil, err
		}
		return []model.Message{*m}, nil
	}

	first, err := b.Consume(ctx, queueName, opts)
	if err != nil {
		return nil, err
	}
	result := make([]model.Message, 1, max)
	result[0] = *first

	// Drain up to max-1 additional immediately ready messages without blocking.
	nonBlocking := opts
	nonBlocking.Timeout = 0
	for len(result) < max {
		m, err := b.Consume(ctx, queueName, nonBlocking)
		if err != nil {
			break
		}
		result = append(result, *m)
	}
	return result, nil
}

// SubscribeStream creates a persistent subscription channel for queueName that
// first drains any existing queued messages and then streams newly published
// messages until ctx is canceled or the returned unsubscribe function is called.
func (b *Broker) SubscribeStream(ctx context.Context, queueName string, opts ConsumeOptions) (<-chan model.Message, func(), error) {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return nil, nil, err
	}
	if opts.Mode == "" {
		opts.Mode = b.opts.DeliveryMode
	}

	streamCtx, cancel := context.WithCancel(ctx)
	out := make(chan model.Message, 64)

	b.persistMu.Lock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.persistMu.Unlock()
		cancel()
		return nil, nil, ErrBrokerClosed
	}

	// Drain initial queued messages into a local slice while under lock?
	// Better: register the streaming subscriber immediately so no live messages
	// are missed, and collect any backlog to deliver first.
	now := time.Now().UTC()
	var backlog []model.Message
	var consumedIDs []string
	for {
		m, ok := b.popReadyLocked(qName, now)
		if !ok {
			break
		}
		m.DeliveryCount++
		if opts.AckRequired {
			vis := opts.VisibilityTimeout
			if vis <= 0 {
				vis = b.opts.VisibilityTimeout
			}
			receipt := model.NewUUID()
			m.Receipt = receipt
			b.inflight[receipt] = &inflightItem{
				queue:    qName,
				msg:      m.Clone(),
				deadline: now.Add(vis),
			}
		} else {
			m.Receipt = ""
			consumedIDs = append(consumedIDs, m.ID)
		}
		backlog = append(backlog, m)
	}

	b.nextSubID++
	sub := &subscriber{
		id:         b.nextSubID,
		queue:      qName,
		ch:         make(chan delivery, 256),
		stream:     true,
		ack:        opts.AckRequired,
		visibility: opts.VisibilityTimeout,
		mode:       opts.Mode,
	}
	b.subscribers[qName] = append(b.subscribers[qName], sub)
	b.mu.Unlock()

	if len(backlog) > 0 {
		b.deliveredTotal.Add(int64(len(backlog)))
	}
	for _, id := range consumedIDs {
		if err := b.store.RecordConsume(qName, id); err != nil {
			b.logger.Error("failed to persist stream backlog consume", "queue", qName, "id", id, "err", err)
		}
	}
	if len(consumedIDs) > 0 && b.store.NeedsCompaction() {
		_ = b.store.SaveSnapshot(b.snapshotState())
	}
	b.persistMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		defer b.cleanupStreamSubscriber(sub)

		// Deliver pre-existing backlog first in FIFO order.
		for i, m := range backlog {
			select {
			case out <- m:
			case <-streamCtx.Done():
				b.requeueUndelivered(qName, backlog[i:])
				return
			case <-b.stopCh:
				b.requeueUndelivered(qName, backlog[i:])
				return
			}
		}

		// Forward live deliveries.
		for {
			select {
			case d := <-sub.ch:
				select {
				case out <- d.msg:
				case <-streamCtx.Done():
					if d.sole {
						b.requeueUndelivered(qName, []model.Message{d.msg})
					} else {
						b.releaseInflight(d.msg.Receipt)
					}
					return
				case <-b.stopCh:
					if d.sole {
						b.requeueUndelivered(qName, []model.Message{d.msg})
					} else {
						b.releaseInflight(d.msg.Receipt)
					}
					return
				}
			case <-streamCtx.Done():
				return
			case <-b.stopCh:
				return
			}
		}
	}()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	return out, unsubscribe, nil
}

// cleanupCanceledSubscriber removes a one-shot subscriber and safely requeues
// any message that raced into sub.ch as the subscriber was canceling.
func (b *Broker) cleanupCanceledSubscriber(sub *subscriber) {
	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	removed := b.removeSubscriberByIDLocked(sub.queue, sub.id)
	if removed {
		b.mu.Unlock()
		return
	}

	// Not found: publisher already popped sub and sent a delivery into sub.ch.
	select {
	case d := <-sub.ch:
		b.clearInflightLocked(d.msg.Receipt)
		if d.sole {
			m := d.msg
			m.Receipt = ""
			if m.DeliveryCount > 0 {
				m.DeliveryCount--
			}
			b.getOrCreateQueueLocked(sub.queue).PushFront(m)
			b.mu.Unlock()
			_ = b.persistRequeueLocked(sub.queue, m)
			return
		}
	default:
	}
	b.mu.Unlock()
}

func (b *Broker) cleanupStreamSubscriber(sub *subscriber) {
	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	b.removeSubscriberByIDLocked(sub.queue, sub.id)
	var requeue []model.Message
	for {
		select {
		case d := <-sub.ch:
			b.clearInflightLocked(d.msg.Receipt)
			if d.sole {
				m := d.msg
				m.Receipt = ""
				if m.DeliveryCount > 0 {
					m.DeliveryCount--
				}
				requeue = append(requeue, m)
			}
		default:
			goto drained
		}
	}
drained:
	for i := len(requeue) - 1; i >= 0; i-- {
		b.getOrCreateQueueLocked(sub.queue).PushFront(requeue[i])
	}
	b.mu.Unlock()

	for _, m := range requeue {
		_ = b.persistRequeueLocked(sub.queue, m)
	}
}

func (b *Broker) requeueUndelivered(qName string, msgs []model.Message) {
	if len(msgs) == 0 {
		return
	}
	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		b.clearInflightLocked(m.Receipt)
		m.Receipt = ""
		if m.DeliveryCount > 0 {
			m.DeliveryCount--
		}
		b.getOrCreateQueueLocked(qName).PushFront(m)
	}
	b.mu.Unlock()

	for _, m := range msgs {
		m.Receipt = ""
		if m.DeliveryCount > 0 {
			m.DeliveryCount--
		}
		_ = b.persistRequeueLocked(qName, m)
	}
}

func (b *Broker) releaseInflight(receipt string) {
	if receipt == "" {
		return
	}
	b.mu.Lock()
	delete(b.inflight, receipt)
	b.mu.Unlock()
}

// Ack confirms processing of an in-flight message, permanently removing it.
func (b *Broker) Ack(queueName, receipt string) error {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return err
	}

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	item, ok := b.inflight[receipt]
	if !ok || item.queue != qName {
		b.mu.Unlock()
		return ErrReceiptNotFound
	}
	delete(b.inflight, receipt)
	msgID := item.msg.ID
	b.mu.Unlock()

	b.ackedTotal.Add(1)
	return b.persistConsumeLocked(qName, msgID)
}

// Nack negatively acknowledges an in-flight message, immediately requeuing it
// or routing it to `<queue>.dlq` if MaxRetries has been reached.
func (b *Broker) Nack(queueName, receipt string) error {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return err
	}

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	item, ok := b.inflight[receipt]
	if !ok || item.queue != qName {
		b.mu.Unlock()
		return ErrReceiptNotFound
	}
	delete(b.inflight, receipt)
	msg := item.msg
	msg.Receipt = ""

	dlqName := qName + ".dlq"
	toDLQ := msg.DeliveryCount >= b.opts.MaxRetries
	if toDLQ {
		b.getOrCreateQueueLocked(dlqName).Push(msg)
	} else {
		// Try delivering to a waiting subscriber or push to front of queue.
		now := time.Now().UTC()
		if len(b.subscribers[qName]) > 0 {
			b.enqueueOrDispatchLocked(qName, msg, now)
		} else {
			b.getOrCreateQueueLocked(qName).PushFront(msg)
		}
	}
	b.mu.Unlock()

	b.nackedTotal.Add(1)
	if toDLQ {
		b.deadLetteredTotal.Add(1)
		_ = b.store.RecordConsume(qName, msg.ID)
		return b.persistPublishLocked(dlqName, msg, false)
	}
	b.redeliveredTotal.Add(1)
	return b.persistRequeueLocked(qName, msg)
}

// ListQueues returns stats for every active queue (queued, delayed, in-flight, or subscribed).
func (b *Broker) ListQueues() []QueueInfo {
	b.mu.Lock()
	defer b.mu.Unlock()

	names := make(map[string]struct{})
	for k, rb := range b.queues {
		if rb.Len() > 0 {
			names[k] = struct{}{}
		}
	}
	for k, d := range b.delayed {
		if len(d) > 0 {
			names[k] = struct{}{}
		}
	}
	for k, s := range b.subscribers {
		if len(s) > 0 {
			names[k] = struct{}{}
		}
	}
	inflightCounts := make(map[string]int)
	for _, item := range b.inflight {
		names[item.queue] = struct{}{}
		inflightCounts[item.queue]++
	}

	out := make([]QueueInfo, 0, len(names))
	for name := range names {
		depth := 0
		if rb, ok := b.queues[name]; ok {
			depth = rb.Len()
		}
		out = append(out, QueueInfo{
			Name:              name,
			Depth:             depth,
			Delayed:           len(b.delayed[name]),
			InFlight:          inflightCounts[name],
			ActiveSubscribers: len(b.subscribers[name]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// QueueStats returns stats for a single queue.
func (b *Broker) QueueStats(queueName string) (QueueInfo, error) {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return QueueInfo{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	depth := 0
	if rb, ok := b.queues[qName]; ok {
		depth = rb.Len()
	}
	inFlight := 0
	for _, item := range b.inflight {
		if item.queue == qName {
			inFlight++
		}
	}
	return QueueInfo{
		Name:              qName,
		Depth:             depth,
		Delayed:           len(b.delayed[qName]),
		InFlight:          inFlight,
		ActiveSubscribers: len(b.subscribers[qName]),
	}, nil
}

// Peek returns up to limit queued messages without removing them.
func (b *Broker) Peek(queueName string, limit int) ([]model.Message, error) {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	rb, ok := b.queues[qName]
	if !ok || rb.Len() == 0 {
		return []model.Message{}, nil
	}
	raw := rb.Peek(limit)
	now := time.Now().UTC()
	out := make([]model.Message, 0, len(raw))
	for _, m := range raw {
		if !m.IsExpired(now) {
			out = append(out, m.Clone())
		}
	}
	return out, nil
}

// PurgeQueue removes all queued, delayed, and in-flight messages for queueName.
func (b *Broker) PurgeQueue(queueName string) error {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return err
	}

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	if rb, ok := b.queues[qName]; ok {
		rb.Clear()
		delete(b.queues, qName)
	}
	delete(b.delayed, qName)
	for receipt, item := range b.inflight {
		if item.queue == qName {
			delete(b.inflight, receipt)
		}
	}
	b.mu.Unlock()

	if err := b.store.RecordPurge(qName); err != nil {
		return err
	}
	if b.store.NeedsCompaction() {
		return b.store.SaveSnapshot(b.snapshotState())
	}
	return nil
}

// Metrics returns a snapshot of broker counters and gauges.
func (b *Broker) Metrics() MetricsSnapshot {
	b.mu.Lock()
	totalQueued := 0
	queueCount := 0
	for _, rb := range b.queues {
		if rb.Len() > 0 {
			totalQueued += rb.Len()
			queueCount++
		}
	}
	totalDelayed := 0
	for _, d := range b.delayed {
		totalDelayed += len(d)
	}
	activeSubs := 0
	for _, s := range b.subscribers {
		activeSubs += len(s)
	}
	totalInFlight := len(b.inflight)
	b.mu.Unlock()

	return MetricsSnapshot{
		PublishedTotal:    b.publishedTotal.Load(),
		DeliveredTotal:    b.deliveredTotal.Load(),
		AckedTotal:        b.ackedTotal.Load(),
		NackedTotal:       b.nackedTotal.Load(),
		RedeliveredTotal:  b.redeliveredTotal.Load(),
		DeadLetteredTotal: b.deadLetteredTotal.Load(),
		ExpiredTotal:      b.expiredTotal.Load(),
		DroppedTotal:      b.droppedTotal.Load(),
		QueueCount:        queueCount,
		TotalQueued:       totalQueued,
		TotalDelayed:      totalDelayed,
		TotalInFlight:     totalInFlight,
		ActiveSubscribers: activeSubs,
	}
}

// ActiveSubscriberCount returns the number of currently registered subscribers on queueName.
func (b *Broker) ActiveSubscriberCount(queueName string) int {
	qName, err := model.ValidateQueueName(queueName)
	if err != nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers[qName])
}

func (b *Broker) reaperLoop() {
	defer b.reaperWg.Done()
	ticker := time.NewTicker(b.opts.ReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.reapOnce(time.Now().UTC())
		case <-b.stopCh:
			return
		}
	}
}

func (b *Broker) reapOnce(now time.Time) {
	// Quick check without persistMu to avoid contention when idle.
	b.mu.Lock()
	hasWork := len(b.delayed) > 0 || len(b.inflight) > 0
	b.mu.Unlock()
	if !hasWork {
		return
	}

	b.persistMu.Lock()
	defer b.persistMu.Unlock()

	b.mu.Lock()
	var (
		promoted      []struct{ q string; m model.Message }
		requeued      []struct{ q string; m model.Message }
		deadLettered  []struct{ q, dlq string; m model.Message }
		expiredIDs    []struct{ q, id string }
	)

	// 1. Promote due delayed messages.
	for qName, list := range b.delayed {
		remaining := list[:0]
		for _, m := range list {
			if m.IsExpired(now) {
				b.expiredTotal.Add(1)
				expiredIDs = append(expiredIDs, struct{ q, id string }{qName, m.ID})
				continue
			}
			if !m.IsDelayed(now) {
				m.DeliverAt = nil
				needPersist, _ := b.enqueueOrDispatchLocked(qName, m, now)
				if needPersist {
					promoted = append(promoted, struct{ q string; m model.Message }{qName, m})
				} else {
					// Delivered live directly; remove from delayed persistence.
					expiredIDs = append(expiredIDs, struct{ q, id string }{qName, m.ID})
				}
				continue
			}
			remaining = append(remaining, m)
		}
		if len(remaining) == 0 {
			delete(b.delayed, qName)
		} else {
			b.delayed[qName] = remaining
		}
	}

	// 2. Reap expired visibility leases on in-flight messages.
	for receipt, item := range b.inflight {
		if now.Before(item.deadline) {
			continue
		}
		delete(b.inflight, receipt)
		msg := item.msg
		msg.Receipt = ""

		if msg.IsExpired(now) {
			b.expiredTotal.Add(1)
			expiredIDs = append(expiredIDs, struct{ q, id string }{item.queue, msg.ID})
			continue
		}

		if msg.DeliveryCount >= b.opts.MaxRetries {
			dlqName := item.queue + ".dlq"
			b.getOrCreateQueueLocked(dlqName).Push(msg)
			b.deadLetteredTotal.Add(1)
			deadLettered = append(deadLettered, struct{ q, dlq string; m model.Message }{item.queue, dlqName, msg})
		} else {
			if len(b.subscribers[item.queue]) > 0 {
				b.enqueueOrDispatchLocked(item.queue, msg, now)
			} else {
				b.getOrCreateQueueLocked(item.queue).PushFront(msg)
			}
			b.redeliveredTotal.Add(1)
			requeued = append(requeued, struct{ q string; m model.Message }{item.queue, msg})
		}
	}
	b.mu.Unlock()

	dirty := false
	for _, e := range expiredIDs {
		_ = b.store.RecordConsume(e.q, e.id)
		dirty = true
	}
	for _, p := range promoted {
		_ = b.store.RecordConsume(p.q, p.m.ID)
		_ = b.store.RecordPublish(p.q, p.m, false)
		dirty = true
	}
	for _, r := range requeued {
		_ = b.store.RecordRequeue(r.q, r.m)
		dirty = true
	}
	for _, d := range deadLettered {
		_ = b.store.RecordConsume(d.q, d.m.ID)
		_ = b.store.RecordPublish(d.dlq, d.m, false)
		dirty = true
	}
	if dirty && b.store.NeedsCompaction() {
		_ = b.store.SaveSnapshot(b.snapshotState())
	}
}

func (b *Broker) popReadyLocked(qName string, now time.Time) (model.Message, bool) {
	rb, ok := b.queues[qName]
	if !ok {
		return model.Message{}, false
	}
	for {
		msg, ok := rb.Pop()
		if !ok {
			delete(b.queues, qName)
			return model.Message{}, false
		}
		if rb.Len() == 0 {
			delete(b.queues, qName)
		}
		if msg.IsExpired(now) {
			b.expiredTotal.Add(1)
			continue
		}
		return msg, true
	}
}

func (b *Broker) getOrCreateQueueLocked(qName string) *RingBuffer[model.Message] {
	rb, ok := b.queues[qName]
	if !ok {
		rb = NewRingBuffer[model.Message]()
		b.queues[qName] = rb
	}
	return rb
}

func (b *Broker) checkLimitsLocked(qName string, incoming int) error {
	rb, exists := b.queues[qName]
	if !exists {
		if _, hasDelayed := b.delayed[qName]; !hasDelayed {
			if len(b.queues)+len(b.delayed) >= b.opts.MaxQueues {
				return ErrMaxQueuesReached
			}
		}
	}
	curDepth := len(b.delayed[qName])
	if rb != nil {
		curDepth += rb.Len()
	}
	if curDepth+incoming > b.opts.MaxQueueDepth {
		return ErrQueueFull
	}
	return nil
}

func (b *Broker) removeSubscriberAtLocked(qName string, idx int) {
	subs := b.subscribers[qName]
	if idx < 0 || idx >= len(subs) {
		return
	}
	copy(subs[idx:], subs[idx+1:])
	subs[len(subs)-1] = nil
	subs = subs[:len(subs)-1]
	if len(subs) == 0 {
		delete(b.subscribers, qName)
	} else {
		b.subscribers[qName] = subs
	}
}

func (b *Broker) removeSubscriberByIDLocked(qName string, id uint64) bool {
	subs := b.subscribers[qName]
	for i, s := range subs {
		if s.id == id {
			b.removeSubscriberAtLocked(qName, i)
			return true
		}
	}
	return false
}

// snapshotState creates a consistent deep snapshot under b.mu.
// In-flight messages are included at the front of their queue so a restart
// redelivers unacknowledged messages.
func (b *Broker) snapshotState() *model.Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	snap := &model.Snapshot{
		Queues:  make(map[string][]model.Message, len(b.queues)),
		Delayed: make(map[string][]model.Message, len(b.delayed)),
	}

	// Prepend in-flight messages so crash recovery redelivers them.
	for _, item := range b.inflight {
		m := item.msg.Clone()
		m.Receipt = ""
		snap.Queues[item.queue] = append(snap.Queues[item.queue], m)
	}

	for qName, rb := range b.queues {
		if rb.Len() == 0 {
			continue
		}
		msgs := rb.ToSlice()
		for _, m := range msgs {
			snap.Queues[qName] = append(snap.Queues[qName], m.Clone())
		}
	}

	for qName, list := range b.delayed {
		if len(list) == 0 {
			continue
		}
		cp := make([]model.Message, len(list))
		for i, m := range list {
			cp[i] = m.Clone()
		}
		snap.Delayed[qName] = cp
	}

	return snap
}

func (b *Broker) persistPublishLocked(qName string, msg model.Message, delayed bool) error {
	if err := b.store.RecordPublish(qName, msg, delayed); err != nil {
		return err
	}
	if b.store.NeedsCompaction() {
		return b.store.SaveSnapshot(b.snapshotState())
	}
	return nil
}

func (b *Broker) persistConsumeLocked(qName, msgID string) error {
	if err := b.store.RecordConsume(qName, msgID); err != nil {
		return err
	}
	if b.store.NeedsCompaction() {
		return b.store.SaveSnapshot(b.snapshotState())
	}
	return nil
}

func (b *Broker) persistRequeueLocked(qName string, msg model.Message) error {
	if err := b.store.RecordRequeue(qName, msg); err != nil {
		return err
	}
	if b.store.NeedsCompaction() {
		return b.store.SaveSnapshot(b.snapshotState())
	}
	return nil
}
