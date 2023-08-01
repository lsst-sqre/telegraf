//go:generate ../../../tools/readme_config_includer/generator
package kafka_consumer

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/common/kafka"
	"github.com/influxdata/telegraf/plugins/inputs"
)

//go:embed sample.conf
var sampleConfig string

const (
	defaultMaxUndeliveredMessages = 1000
	defaultMaxProcessingTime      = config.Duration(100 * time.Millisecond)
	defaultConsumerGroup          = "telegraf_metrics_consumers"
	reconnectDelay                = 5 * time.Second
)

type empty struct{}
type semaphore chan empty

type KafkaConsumer struct {
	Brokers                []string        `toml:"brokers"`
	ConsumerGroup          string          `toml:"consumer_group"`
	MaxMessageLen          int             `toml:"max_message_len"`
	MaxUndeliveredMessages int             `toml:"max_undelivered_messages"`
	MaxProcessingTime      config.Duration `toml:"max_processing_time"`
	Offset                 string          `toml:"offset"`
	BalanceStrategy        string          `toml:"balance_strategy"`
	Topics                 []string        `toml:"topics"`
	TopicRegexps           []string        `toml:"topic_regexps"`
	TopicRefreshInterval   config.Duration `toml:"topic_refresh_interval"`
	TopicTag               string          `toml:"topic_tag"`
	ConsumerFetchDefault   config.Size     `toml:"consumer_fetch_default"`
	ConnectionStrategy     string          `toml:"connection_strategy"`

	kafka.ReadConfig

	kafka.Logger

	Log telegraf.Logger `toml:"-"`

	ConsumerCreator ConsumerGroupCreator `toml:"-"`
	consumer        ConsumerGroup
	config          *sarama.Config

	topicClient     sarama.Client
	regexps         []regexp.Regexp
	allWantedTopics []string
	refreshContext  context.Context
	refreshCancel   context.CancelFunc
	refreshWg       sync.WaitGroup
	ticker          *time.Ticker
	stopTicker      bool
	fingerprint     string

	parser    telegraf.Parser
	topicLock sync.Mutex
	groupLock sync.Mutex
	wg        sync.WaitGroup
	cancel    context.CancelFunc
}

// FIXME
// AJT notes to self
// 1: race condition when switching consumers.  Maybe I just need to move
// the lock.  Maybe switching in the background is a bad idea.  Since Kafka
// knows where to pick up, I could try NOT doing that in the background.
// Remember `go test -race` exists.
// 2: Clean shutdown of timer ticker: looks like wrapping it in its own
// Context and Waitgroup should work.

type ConsumerGroup interface {
	Consume(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error
	Errors() <-chan error
	Close() error
}

type ConsumerGroupCreator interface {
	Create(brokers []string, group string, cfg *sarama.Config) (ConsumerGroup, error)
}

type SaramaCreator struct{}

func (*SaramaCreator) Create(brokers []string, group string, cfg *sarama.Config) (ConsumerGroup, error) {
	return sarama.NewConsumerGroup(brokers, group, cfg)
}

func (*KafkaConsumer) SampleConfig() string {
	return sampleConfig
}

func (k *KafkaConsumer) SetParser(parser telegraf.Parser) {
	k.parser = parser
}

func (k *KafkaConsumer) Init() error {
	k.SetLogger()

	if k.MaxUndeliveredMessages == 0 {
		k.MaxUndeliveredMessages = defaultMaxUndeliveredMessages
	}
	if time.Duration(k.MaxProcessingTime) == 0 {
		k.MaxProcessingTime = defaultMaxProcessingTime
	}
	if k.ConsumerGroup == "" {
		k.ConsumerGroup = defaultConsumerGroup
	}

	cfg := sarama.NewConfig()

	// Kafka version 0.10.2.0 is required for consumer groups.
	cfg.Version = sarama.V0_10_2_0

	if err := k.SetConfig(cfg, k.Log); err != nil {
		return fmt.Errorf("SetConfig: %w", err)
	}

	switch strings.ToLower(k.Offset) {
	case "oldest", "":
		cfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	case "newest":
		cfg.Consumer.Offsets.Initial = sarama.OffsetNewest
	default:
		return fmt.Errorf("invalid offset %q", k.Offset)
	}

	switch strings.ToLower(k.BalanceStrategy) {
	case "range", "":
		cfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.BalanceStrategyRange}
	case "roundrobin":
		cfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.BalanceStrategyRoundRobin}
	case "sticky":
		cfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.BalanceStrategySticky}
	default:
		return fmt.Errorf("invalid balance strategy %q", k.BalanceStrategy)
	}

	if k.ConsumerCreator == nil {
		k.ConsumerCreator = &SaramaCreator{}
	}

	cfg.Consumer.MaxProcessingTime = time.Duration(k.MaxProcessingTime)

	if k.ConsumerFetchDefault != 0 {
		cfg.Consumer.Fetch.Default = int32(k.ConsumerFetchDefault)
	}

	switch strings.ToLower(k.ConnectionStrategy) {
	default:
		return fmt.Errorf("invalid connection strategy %q", k.ConnectionStrategy)
	case "defer", "startup", "":
	}

	k.config = cfg

	if len(k.TopicRegexps) == 0 {
		k.allWantedTopics = k.Topics
	} else {
		if err := k.compileTopicRegexps(); err != nil {
			return err
		}
		// We have regexps, so we're going to need a client to ask
		// the broker for topics
		client, err := sarama.NewClient(k.Brokers, k.config)
		if err != nil {
			return err
		}
		k.topicClient = client
	}
	return nil
}

func (k *KafkaConsumer) compileTopicRegexps() error {
	// While we can add new topics matching extant regexps, we can't
	// update that list on the fly.  We compile them once at startup.
	// Changing them is a configuration change and requires us to cancel
	// and relaunch our ConsumerGroup.

	k.regexps = make([]regexp.Regexp, 0, len(k.TopicRegexps))
	for _, r := range k.TopicRegexps {
		re, err := regexp.Compile(r)
		if err != nil {
			return fmt.Errorf("regular expression %q did not compile: '%w", r, err)
		}
		k.regexps = append(k.regexps, *re)
	}
	return nil
}

func (k *KafkaConsumer) changedTopics() (bool, error) {
	// We have instantiated a generic Kafka client, so we can ask
	// it for all the topics it knows about.  Then we build
	// regexps from our strings, loop over those, loop over the
	// topics, and if we find a match, add that topic to
	// out topic set, which then we turn back into a list at the end.
	//
	// If our topics changed, we return true, to indicate that the
	// consumer will need a restart in order to pick up the new
	// topics.

	if len(k.regexps) == 0 {
		return false, nil
	}

	// Refresh metadata for all topics.
	err := k.topicClient.RefreshMetadata()
	if err != nil {
		return false, err
	}
	allDiscoveredTopics, err := k.topicClient.Topics()
	if err != nil {
		return false, err
	}
	sort.Strings(allDiscoveredTopics)
	k.Log.Debugf("discovered %d topics in total", len(allDiscoveredTopics))

	extantTopicSet := make(map[string]bool, len(allDiscoveredTopics))
	for _, t := range allDiscoveredTopics {
		extantTopicSet[t] = true
	}
	// Even if a topic specified by a literal string (that is, k.Topics)
	// does not appear in the topic list, we want to keep it around, in
	// case it pops back up--it is not guaranteed to be matched by any
	// of our regular expressions.  Therefore, we pretend that it's in
	// extantTopicSet, even if it isn't.
	//
	// Assuming that literally-specified topics are usually in the topics
	// present on the broker, this should not need a resizing (although if
	// you have many topics that you don't care about, it will be too big)
	wantedTopicSet := make(map[string]bool, len(allDiscoveredTopics))
	for _, t := range k.Topics {
		// Get our pre-specified topics
		wantedTopicSet[t] = true
	}
	for _, t := range allDiscoveredTopics {
		// Add topics that match regexps
		for _, r := range k.regexps {
			if r.MatchString(t) {
				wantedTopicSet[t] = true
				break
			}
		}
	}
	topicList := make([]string, 0, len(wantedTopicSet))
	for t := range wantedTopicSet {
		topicList = append(topicList, t)
	}
	sort.Strings(topicList)
	fingerprint := strings.Join(topicList, ";")
	k.Log.Debugf("Regular expression list %q matched %d topics", k.TopicRegexps, len(topicList))
	if fingerprint != k.fingerprint {
		k.Log.Infof("updating topics: replacing %q with %q", k.allWantedTopics, topicList)
		k.topicLock.Lock()
		k.fingerprint = fingerprint
		k.allWantedTopics = topicList
		k.topicLock.Unlock()
		return true, nil
	}
	k.Log.Debugf("topic list unchanged on refresh")
	return false, nil
}

func (k *KafkaConsumer) create() error {
	var err error
	k.consumer, err = k.ConsumerCreator.Create(
		k.Brokers,
		k.ConsumerGroup,
		k.config,
	)

	return err
}

func (k *KafkaConsumer) startErrorAdder(acc telegraf.Accumulator) {
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		for err := range k.consumer.Errors() {
			acc.AddError(fmt.Errorf("channel: %w", err))
		}
	}()
}
	

func (k *KafkaConsumer) getNewHandler(acc telegraf.Accumulator) *ConsumerGroupHandler {
	handler := NewConsumerGroupHandler(acc, k.MaxUndeliveredMessages, k.parser, k.Log)
	handler.MaxMessageLen = k.MaxMessageLen
	handler.TopicTag = k.TopicTag
	return handler
}

func (k *KafkaConsumer) handleTicker(acc telegraf.Accumulator) {
	dstr := time.Duration(k.TopicRefreshInterval).String()
	k.Log.Infof("starting refresh ticker: scanning topics every %s", dstr)
	for {
		// Grab a copy of k.stopTicker; if we miss it, we just do
		// one extra pass
		k.Log.Debug("about to get lock for stopticker in handleTicker()")
		k.topicLock.Lock()
		k.Log.Debug("got lock for stopticker in handleTicker()")
		st := k.stopTicker
		k.topicLock.Unlock()
		k.Log.Debug("released lock for stopticker in handleTicker()")
		if st {
			k.Log.Debug("ticker should stop")
			return
		}
		<-k.ticker.C
		k.Log.Debugf("received topic refresh request (every %s)", dstr)
		changed, err := k.changedTopics()
		if err != nil {
			acc.AddError(err)
			return
		}
		if changed {
			// We need the lock
			k.Log.Debug("about to get lock from handleTicker()")
			k.topicLock.Lock()
			k.Log.Debug("lock acquired: handleTicker()")
			err = k.restartConsumer(acc)
			if err != nil {
				acc.AddError(err)
				k.topicLock.Unlock()
				return
			}
			k.Log.Debug("releasing lock in handleTicker()")
			k.topicLock.Unlock()
		}
	}
}

func (k *KafkaConsumer) consumeTopics(ctx context.Context, acc telegraf.Accumulator) {
	k.wg.Add(1)
	defer k.wg.Done()
	go func() {
		for ctx.Err() == nil {
			handler := k.getNewHandler(acc)
			// We need to copy allWantedTopics; the Consume() is
			// long-running and we can easily deadlock if our
			// topic-update-checker fires.
			k.Log.Debug("about to get lock from consumeTopics()")
			k.topicLock.Lock()
			k.Log.Debug("acquired lock in consumeTopics()")
			topics := make([]string, len(k.allWantedTopics))
			copy(topics, k.allWantedTopics)
			k.topicLock.Unlock()
			k.Log.Debug("lock released in consumeTopics()")
			err := k.consumer.Consume(ctx, topics, handler)
			if err != nil {
				acc.AddError(fmt.Errorf("consume: %w", err))
				internal.SleepContext(ctx, reconnectDelay) //nolint:errcheck // ignore returned error as we cannot do anything about it anyway
			}
		}
		err := k.consumer.Close()
		if err != nil {
			acc.AddError(fmt.Errorf("close: %w", err))
		}
	}()
}

func (k *KafkaConsumer) restartConsumer(acc telegraf.Accumulator) error {
	// Up to 100Hz, at least, we do not lose messages on consumer group
	// restart.  Since the group name is the same, it seems very likely
	// that we just pick up at the offset we left off at: that is the
	// producer does not see this as a new consumer, but as an old one
	// that's just missed a couple beats.
	if k.consumer == nil {
		// Fast exit if the consumer isn't running
		return nil
	}
	// Do the switcheroo.
	k.Log.Debug("replacing consumer group")
	k.Log.Debug("closing old consumer group")
	err := k.consumer.Close()
	if err != nil {
		acc.AddError(err)
		return err
	}
	k.Log.Debug("destroying old consumer goroutines")
	k.cancel()
	k.wg.Wait()
	k.Log.Debug("creating new consumer group")
	newConsumer, err := k.ConsumerCreator.Create(
		k.Brokers,
		k.ConsumerGroup,
		k.config,
	)
	if err != nil {
		acc.AddError(err)
		return err
	}
	ctx, ccl := context.WithCancel(context.Background())
	k.Log.Debug("about to acquire groupLock in restartConsumer()")
	k.groupLock.Lock()
	k.Log.Debug("acquired groupLock in restartConsumer()")
	k.Log.Debug("replacing k.consumer and k.cancel")
	k.consumer = newConsumer
	k.cancel = ccl
	k.groupLock.Unlock()
	k.Log.Debug("released groupLock in restartConsumer()")
	k.Log.Debug("starting new consumer group")
	k.consumeTopics(ctx, acc)
	k.Log.Info("restarted consumer group")
	return nil
}

func (k *KafkaConsumer) Start(acc telegraf.Accumulator) error {
	var err error

	k.Log.Debugf("TopicRegexps: %v", k.TopicRegexps)
	k.Log.Debugf("TopicRefreshInterval: %v", k.TopicRefreshInterval)

	// If TopicRegexps is set, add matches to Topics
	if len(k.TopicRegexps) > 0 {
		if _, err = k.changedTopics(); err != nil {
			// We're starting, so we expect the list to change;
			// all we care about is whether we got an error
			// acquiring our topics.
			return err
		}
		// If refresh interval is specified, start a goroutine
		// to refresh topics periodically.  This only makes sense if
		// TopicRegexps is set.
		if k.TopicRefreshInterval > 0 {
			k.refreshContext, k.refreshCancel = context.WithCancel(context.Background())
			k.ticker = time.NewTicker(time.Duration(k.TopicRefreshInterval))
			// Note that this uses a separate waitgroup.
			// That's because handleTicker cares whether
			// topics have changed, and if they have, it
			// will invoke restartConsumer(), which tells
			// the current consumer to stop.  That stop
			// cancels goroutines in its waitgroup, and
			// therefore the restart goroutine must not be
			// in that waitgroup.
			go k.handleTicker(acc)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	k.cancel = cancel

	if k.ConnectionStrategy != "defer" {
		err = k.create()
		if err != nil {
			return fmt.Errorf("create consumer: %w", err)
		}
		k.startErrorAdder(acc)
	}

	if k.consumer == nil {
		err = k.create()
		if err != nil {
			acc.AddError(fmt.Errorf("create consumer async: %w", err))
			return err
		}
	}

	k.startErrorAdder(acc)
	k.consumeTopics(ctx, acc) // Starts goroutine internally

	return nil
}

func (k *KafkaConsumer) Gather(_ telegraf.Accumulator) error {
	return nil
}

func (k *KafkaConsumer) Stop() {
	k.Log.Debug("Stop() called")
	if k.ticker != nil {
		k.Log.Debug("stopping ticker")
		k.ticker.Stop()
		k.Log.Debug("stopped ticker, setting k.StopTicker")
		k.stopTicker = true
		k.Log.Debug("calling refreshCancel()")
		k.refreshCancel()
		k.Log.Debug("waiting on refreshWg")
		k.refreshWg.Wait()
		k.Log.Debug("refreshWg awaited")
	}
	// Lock so that a topic refresh cannot start while we are stopping.
	k.topicLock.Lock()
	defer k.topicLock.Unlock()
	if k.topicClient != nil {
		k.topicClient.Close()
	}
	k.cancel()
	k.wg.Wait()
}

// Message is an aggregate type binding the Kafka message and the session so
// that offsets can be updated.
type Message struct {
	message *sarama.ConsumerMessage
	session sarama.ConsumerGroupSession
}

func NewConsumerGroupHandler(acc telegraf.Accumulator, maxUndelivered int, parser telegraf.Parser, log telegraf.Logger) *ConsumerGroupHandler {
	handler := &ConsumerGroupHandler{
		acc:         acc.WithTracking(maxUndelivered),
		sem:         make(chan empty, maxUndelivered),
		undelivered: make(map[telegraf.TrackingID]Message, maxUndelivered),
		parser:      parser,
		log:         log,
	}
	return handler
}

// ConsumerGroupHandler is a sarama.ConsumerGroupHandler implementation.
type ConsumerGroupHandler struct {
	MaxMessageLen int
	TopicTag      string

	acc    telegraf.TrackingAccumulator
	sem    semaphore
	parser telegraf.Parser
	wg     sync.WaitGroup
	cancel context.CancelFunc

	mu          sync.Mutex
	undelivered map[telegraf.TrackingID]Message

	log telegraf.Logger
}

// Setup is called once when a new session is opened.  It sets up the handler
// and begins processing delivered messages.
func (h *ConsumerGroupHandler) Setup(sarama.ConsumerGroupSession) error {
	h.undelivered = make(map[telegraf.TrackingID]Message)

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.run(ctx)
	}()
	return nil
}

// Run processes any delivered metrics during the lifetime of the session.
func (h *ConsumerGroupHandler) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case track := <-h.acc.Delivered():
			h.onDelivery(track)
		}
	}
}

func (h *ConsumerGroupHandler) onDelivery(track telegraf.DeliveryInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()

	msg, ok := h.undelivered[track.ID()]
	if !ok {
		h.log.Errorf("Could not mark message delivered: %d", track.ID())
		return
	}

	if track.Delivered() {
		msg.session.MarkMessage(msg.message, "")
	}

	delete(h.undelivered, track.ID())
	<-h.sem
}

// Reserve blocks until there is an available slot for a new message.
func (h *ConsumerGroupHandler) Reserve(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case h.sem <- empty{}:
		return nil
	}
}

func (h *ConsumerGroupHandler) release() {
	<-h.sem
}

// Handle processes a message and if successful saves it to be acknowledged
// after delivery.
func (h *ConsumerGroupHandler) Handle(session sarama.ConsumerGroupSession, msg *sarama.ConsumerMessage) error {
	if h.MaxMessageLen != 0 && len(msg.Value) > h.MaxMessageLen {
		session.MarkMessage(msg, "")
		h.release()
		return fmt.Errorf("message exceeds max_message_len (actual %d, max %d)",
			len(msg.Value), h.MaxMessageLen)
	}

	metrics, err := h.parser.Parse(msg.Value)
	if err != nil {
		h.release()
		return err
	}

	if len(h.TopicTag) > 0 {
		for _, metric := range metrics {
			metric.AddTag(h.TopicTag, msg.Topic)
		}
	}

	h.mu.Lock()
	id := h.acc.AddTrackingMetricGroup(metrics)
	h.undelivered[id] = Message{session: session, message: msg}
	h.mu.Unlock()
	return nil
}

// ConsumeClaim is called once each claim in a goroutine and must be
// thread-safe.  Should run until the claim is closed.
func (h *ConsumerGroupHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	ctx := session.Context()

	for {
		err := h.Reserve(ctx)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-claim.Messages():
			if !ok {
				return nil
			}
			err := h.Handle(session, msg)
			if err != nil {
				h.acc.AddError(err)
			}
		}
	}
}

// Cleanup stops the internal goroutine and is called after all ConsumeClaim
// functions have completed.
func (h *ConsumerGroupHandler) Cleanup(sarama.ConsumerGroupSession) error {
	h.cancel()
	h.wg.Wait()
	return nil
}

func init() {
	inputs.Add("kafka_consumer", func() telegraf.Input {
		return &KafkaConsumer{}
	})
}
