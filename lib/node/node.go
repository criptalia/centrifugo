// Package node is a real-time core for Centrifugo server.
package node

import (
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/centrifugal/centrifugo/lib/channel"
	"github.com/centrifugal/centrifugo/lib/engine"
	"github.com/centrifugal/centrifugo/lib/logger"
	"github.com/centrifugal/centrifugo/lib/metrics"
	"github.com/centrifugal/centrifugo/lib/proto"
	"github.com/centrifugal/centrifugo/lib/proto/api"
	"github.com/centrifugal/centrifugo/lib/proto/control"
	"github.com/centrifugal/centrifugo/lib/rpc"
	"github.com/nats-io/nuid"

	"github.com/satori/go.uuid"
)

// Node is a heart of Centrifugo – it internally keeps and manages client
// connections, maintains information about other Centrifugo nodes, keeps
// some useful references to things like engine, metrics etc.
type Node struct {
	mu sync.RWMutex

	// version of Centrifugo node.
	version string

	// unique id for this node.
	uid string

	// startedAt is unix time of node start.
	startedAt int64

	// hub to manage client connections.
	hub Hub

	// config for node.
	config *Config

	// engine - in memory or redis.
	engine engine.Engine

	// nodes contains registry of known nodes.
	nodes *nodeRegistry

	// shutdown is a flag which is only true when node is going to shut down.
	shutdown bool

	// shutdownCh is a channel which is closed when node shutdown initiated.
	shutdownCh chan struct{}

	// save metrics snapshot until next metrics interval.
	metricsSnapshot map[string]int64

	// metricsOnce helps to share metrics with other nodes only once after
	// snapshot updated thus significantly reducing node control message size.
	metricsOnce sync.Once

	// protect access to metrics snapshot.
	metricsMu sync.RWMutex

	// messageEncoder is encoder to encode messages for engine.
	messageEncoder proto.MessageEncoder

	// messageEncoder is decoder to decode messages coming from engine.
	messageDecoder proto.MessageDecoder

	// controlEncoder is encoder to encode control messages for engine.
	controlEncoder control.Encoder

	// controlDecoder is decoder to decode control messages coming from engine.
	controlDecoder control.Decoder

	rpcHandler rpc.Handler
}

// global metrics registry pointing to the same Registry plugin package uses.
var metricsRegistry *metrics.Registry

func init() {
	metricsRegistry = metrics.DefaultRegistry

	metricsRegistry.RegisterCounter("node_num_publication_sent", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_join_sent", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_leave_sent", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_admin_msg_sent", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_control_sent", metrics.NewCounter())

	metricsRegistry.RegisterCounter("node_num_publication_received", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_join_received", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_leave_received", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_admin_msg_received", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_control_received", metrics.NewCounter())

	metricsRegistry.RegisterCounter("node_num_add_client_conn", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_remove_client_conn", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_add_client_sub", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_remove_client_sub", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_presence", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_add_presence", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_remove_presence", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_history", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_remove_history", metrics.NewCounter())
	metricsRegistry.RegisterCounter("node_num_last_message_id", metrics.NewCounter())

	metricsRegistry.RegisterGauge("node_memory_sys", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_memory_heap_sys", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_memory_heap_alloc", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_memory_stack_inuse", metrics.NewGauge())

	metricsRegistry.RegisterGauge("node_cpu_usage", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_num_goroutine", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_num_clients", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_num_unique_clients", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_num_channels", metrics.NewGauge())
	metricsRegistry.RegisterGauge("node_uptime_seconds", metrics.NewGauge())
}

// VERSION of Centrifugo server node. Set on build stage.
var VERSION string

// New creates Node, the only required argument is config.
func New(c *Config) *Node {
	uid := uuid.NewV4().String()

	n := &Node{
		version:         VERSION,
		uid:             uid,
		nodes:           newNodeRegistry(uid),
		config:          c,
		hub:             NewHub(),
		startedAt:       time.Now().Unix(),
		metricsSnapshot: make(map[string]int64),
		shutdownCh:      make(chan struct{}),
		messageEncoder:  proto.NewProtobufMessageEncoder(),
		messageDecoder:  proto.NewProtobufMessageDecoder(),
		controlEncoder:  control.NewProtobufEncoder(),
		controlDecoder:  control.NewProtobufDecoder(),
	}

	// Create initial snapshot with empty metric values.
	n.metricsMu.Lock()
	n.metricsSnapshot = n.getSnapshotMetrics()
	n.metricsMu.Unlock()

	return n
}

// Config returns a copy of node Config.
func (n *Node) Config() Config {
	n.mu.RLock()
	c := *n.config
	n.mu.RUnlock()
	return c
}

// SetConfig binds config to node.
func (n *Node) SetConfig(c *Config) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.config = c
}

// SetRPCHandler binds config to node.
func (n *Node) SetRPCHandler(h rpc.Handler) {
	n.rpcHandler = h
}

// RPCHandler binds config to node.
func (n *Node) RPCHandler() rpc.Handler {
	return n.rpcHandler
}

// Version returns version of node.
func (n *Node) Version() string {
	return n.version
}

// Reload node.
func (n *Node) Reload(c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	n.SetConfig(c)
	return nil
}

// Engine returns node's Engine.
func (n *Node) Engine() engine.Engine {
	return n.engine
}

// Hub returns node's client hub.
func (n *Node) Hub() Hub {
	return n.hub
}

// MessageEncoder ...
func (n *Node) MessageEncoder() proto.MessageEncoder {
	return n.messageEncoder
}

// MessageDecoder ...
func (n *Node) MessageDecoder() proto.MessageDecoder {
	return n.messageDecoder
}

// ControlEncoder ...
func (n *Node) ControlEncoder() control.Encoder {
	return n.controlEncoder
}

// ControlDecoder ...
func (n *Node) ControlDecoder() control.Decoder {
	return n.controlDecoder
}

// NotifyShutdown returns a channel which will be closed on node shutdown.
func (n *Node) NotifyShutdown() chan struct{} {
	return n.shutdownCh
}

// Run performs all startup actions. At moment must be called once on start
// after engine and structure set.
func (n *Node) Run(e engine.Engine) error {
	n.mu.Lock()
	n.engine = e
	n.mu.Unlock()

	if err := n.engine.Run(); err != nil {
		return err
	}

	err := n.pubNode()
	if err != nil {
		logger.CRITICAL.Println(err)
	}
	go n.sendNodePingMsg()
	go n.cleanNodeInfo()
	go n.updateMetrics()

	return nil
}

// Shutdown sets shutdown flag and does various clean ups.
func (n *Node) Shutdown() error {
	n.mu.Lock()
	if n.shutdown {
		n.mu.Unlock()
		return nil
	}
	n.shutdown = true
	close(n.shutdownCh)
	n.mu.Unlock()
	return n.hub.Shutdown()
}

func (n *Node) updateMetricsOnce() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	metricsRegistry.Gauges.Set("node_memory_sys", int64(mem.Sys))
	metricsRegistry.Gauges.Set("node_memory_heap_sys", int64(mem.HeapSys))
	metricsRegistry.Gauges.Set("node_memory_heap_alloc", int64(mem.HeapAlloc))
	metricsRegistry.Gauges.Set("node_memory_stack_inuse", int64(mem.StackInuse))
	if usage, err := cpuUsage(); err == nil {
		metricsRegistry.Gauges.Set("node_cpu_usage", int64(usage))
	}
	n.metricsMu.Lock()
	metricsRegistry.Counters.UpdateDelta()
	n.metricsSnapshot = n.getSnapshotMetrics()
	n.metricsOnce = sync.Once{} // let metrics to be sent again.
	metricsRegistry.HDRHistograms.Rotate()
	n.metricsMu.Unlock()
}

func (n *Node) updateMetrics() {
	for {
		n.mu.RLock()
		interval := n.config.NodeMetricsInterval
		n.mu.RUnlock()
		select {
		case <-n.shutdownCh:
			return
		case <-time.After(interval):
			n.updateMetricsOnce()
		}
	}
}

func (n *Node) sendNodePingMsg() {
	for {
		n.mu.RLock()
		interval := n.config.NodePingInterval
		n.mu.RUnlock()
		select {
		case <-n.shutdownCh:
			return
		case <-time.After(interval):
			err := n.pubNode()
			if err != nil {
				logger.CRITICAL.Println(err)
			}
		}
	}
}

func (n *Node) cleanNodeInfo() {
	for {
		n.mu.RLock()
		interval := n.config.NodeInfoCleanInterval
		n.mu.RUnlock()
		select {
		case <-n.shutdownCh:
			return
		case <-time.After(interval):
			n.mu.RLock()
			delay := n.config.NodeInfoMaxDelay
			n.mu.RUnlock()
			n.nodes.clean(delay)
		}
	}
}

// Channels returns list of all engines clients subscribed on all Centrifugo nodes.
func (n *Node) Channels() ([]string, error) {
	return n.engine.Channels()
}

// Info returns aggregated stats from all Centrifugo nodes.
func (n *Node) Info() (*api.InfoResult, error) {
	nodes := n.nodes.list()
	nodeResults := make([]*api.NodeResult, len(nodes))
	for i, nd := range nodes {
		nodeResults[i] = &api.NodeResult{
			UID:                   nd.UID,
			Name:                  nd.Name,
			StartedAt:             nd.StartedAt,
			MetricsUpdateInterval: nd.MetricsUpdateInterval,
			Metrics:               nd.Metrics,
		}
	}

	return &api.InfoResult{
		Engine: n.Engine().Name(),
		Nodes:  nodeResults,
	}, nil
}

// Node returns raw information only from current node.
func (n *Node) Node() (*control.Node, error) {
	info := n.nodes.get(n.uid)
	info.Metrics = n.getRawMetrics()
	return &info, nil
}

func (n *Node) getRawMetrics() map[string]int64 {
	m := make(map[string]int64)
	for name, val := range metricsRegistry.Counters.LoadValues() {
		m[name] = val
	}
	for name, val := range metricsRegistry.HDRHistograms.LoadValues() {
		m[name] = val
	}
	for name, val := range metricsRegistry.Gauges.LoadValues() {
		m[name] = val
	}
	return m
}

func (n *Node) getSnapshotMetrics() map[string]int64 {
	m := make(map[string]int64)
	for name, val := range metricsRegistry.Counters.LoadIntervalValues() {
		m[name] = val
	}
	for name, val := range metricsRegistry.HDRHistograms.LoadValues() {
		m[name] = val
	}
	for name, val := range metricsRegistry.Gauges.LoadValues() {
		m[name] = val
	}
	return m
}

// HandleControl handles messages from control channel - control messages used for internal
// communication between nodes to share state or proto.
func (n *Node) HandleControl(cmd *control.Command) error {
	metricsRegistry.Counters.Inc("node_num_control_received")

	if cmd.UID == n.uid {
		// Sent by this node.
		return nil
	}

	method := cmd.Method
	params := cmd.Params

	switch method {
	case "node":
		cmd, err := n.ControlDecoder().DecodeNode(params)
		if err != nil {
			logger.ERROR.Printf("error decoding node control params: %v", err)
			return proto.ErrBadRequest
		}
		return n.nodeCmd(cmd)
	case "unsubscribe":
		cmd, err := n.ControlDecoder().DecodeUnsubscribe(params)
		if err != nil {
			logger.ERROR.Printf("error decoding unsubscribe control params: %v", err)
			return proto.ErrBadRequest
		}
		return n.unsubscribeUser(cmd.User, cmd.Channel)
	case "disconnect":
		cmd, err := n.ControlDecoder().DecodeDisconnect(params)
		if err != nil {
			logger.ERROR.Printf("error decoding disconnect control params: %v", err)
			return proto.ErrBadRequest
		}
		return n.disconnectUser(cmd.User, false)
	default:
		logger.ERROR.Printf("unknown control message method: %s", method)
		return proto.ErrBadRequest
	}
}

// HandleClientMessage ...
func (n *Node) HandleClientMessage(message *proto.Message) error {
	switch message.Type {
	case proto.MessageTypePublication:
		publication, err := n.messageDecoder.DecodePublication(message.Data)
		if err != nil {
			return err
		}
		n.HandlePublication(message.Channel, publication)
	case proto.MessageTypeJoin:
		join, err := n.messageDecoder.DecodeJoin(message.Data)
		if err != nil {
			return err
		}
		n.HandleJoin(message.Channel, join)
	case proto.MessageTypeLeave:
		leave, err := n.messageDecoder.DecodeLeave(message.Data)
		if err != nil {
			return err
		}
		n.HandleLeave(message.Channel, leave)
	default:
	}
	return nil
}

// HandlePublication handles messages published by web application or client into channel.
// The goal of this method to deliver this message to all clients on this node subscribed
// on channel.
func (n *Node) HandlePublication(ch string, publication *proto.Publication) error {
	metricsRegistry.Counters.Inc("node_num_publication_received")
	numSubscribers := n.hub.NumSubscribers(ch)
	hasCurrentSubscribers := numSubscribers > 0
	if !hasCurrentSubscribers {
		return nil
	}
	return n.hub.BroadcastPublication(ch, publication)
}

// HandleJoin handles join messages.
func (n *Node) HandleJoin(ch string, join *proto.Join) error {
	metricsRegistry.Counters.Inc("node_num_join_received")
	hasCurrentSubscribers := n.hub.NumSubscribers(ch) > 0
	if !hasCurrentSubscribers {
		return nil
	}
	return n.hub.BroadcastJoin(ch, join)
}

// HandleLeave handles leave messages.
func (n *Node) HandleLeave(ch string, leave *proto.Leave) error {
	metricsRegistry.Counters.Inc("node_num_leave_received")
	hasCurrentSubscribers := n.hub.NumSubscribers(ch) > 0
	if !hasCurrentSubscribers {
		return nil
	}
	return n.hub.BroadcastLeave(ch, leave)
}

func makeErrChan(err error) <-chan error {
	ret := make(chan error, 1)
	ret <- err
	return ret
}

// Publish sends a message to all clients subscribed on channel. All running nodes
// will receive it and will send it to all clients on node subscribed on channel.
func (n *Node) Publish(ch string, pub *proto.Publication, opts *channel.Options) <-chan error {
	if opts == nil {
		chOpts, ok := n.ChannelOpts(ch)
		if !ok {
			return makeErrChan(proto.ErrNamespaceNotFound)
		}
		opts = &chOpts
	}

	metricsRegistry.Counters.Inc("node_num_publication_sent")

	if pub.UID == "" {
		pub.UID = nuid.Next()
	}

	return n.engine.Publish(ch, pub, opts)
}

// PublishJoin allows to publish join message into channel when someone subscribes on it
// or leave message when someone unsubscribes from channel.
func (n *Node) PublishJoin(ch string, join *proto.Join, opts *channel.Options) <-chan error {
	if opts == nil {
		chOpts, ok := n.ChannelOpts(ch)
		if !ok {
			return makeErrChan(proto.ErrNamespaceNotFound)
		}
		opts = &chOpts
	}
	metricsRegistry.Counters.Inc("node_num_join_sent")
	return n.engine.PublishJoin(ch, join, opts)
}

// PublishLeave allows to publish join message into channel when someone subscribes on it
// or leave message when someone unsubscribes from channel.
func (n *Node) PublishLeave(ch string, leave *proto.Leave, opts *channel.Options) <-chan error {
	if opts == nil {
		chOpts, ok := n.ChannelOpts(ch)
		if !ok {
			return makeErrChan(proto.ErrNamespaceNotFound)
		}
		opts = &chOpts
	}
	metricsRegistry.Counters.Inc("node_num_leave_sent")
	return n.engine.PublishLeave(ch, leave, opts)
}

// publishControl publishes message into control channel so all running
// nodes will receive and handle it.
func (n *Node) publishControl(msg *control.Command) <-chan error {
	metricsRegistry.Counters.Inc("node_num_control_sent")
	return n.engine.PublishControl(msg)
}

// pubNode sends control message to all nodes - this message
// contains information about current node.
func (n *Node) pubNode() error {
	n.mu.RLock()

	node := &control.Node{
		UID:                   n.uid,
		Name:                  n.config.Name,
		Version:               n.version,
		StartedAt:             n.startedAt,
		MetricsUpdateInterval: uint64(n.config.NodeMetricsInterval.Seconds()),
	}

	n.metricsMu.RLock()
	n.metricsOnce.Do(func() {
		metricsRegistry.Gauges.Set("node_num_clients", int64(n.hub.NumClients()))
		metricsRegistry.Gauges.Set("node_num_unique_clients", int64(n.hub.NumUniqueClients()))
		metricsRegistry.Gauges.Set("node_num_channels", int64(n.hub.NumChannels()))
		metricsRegistry.Gauges.Set("node_num_goroutine", int64(runtime.NumGoroutine()))
		metricsRegistry.Gauges.Set("node_uptime_seconds", time.Now().Unix()-n.startedAt)

		metricsSnapshot := make(map[string]int64)
		for k, v := range n.metricsSnapshot {
			metricsSnapshot[k] = v
		}
		node.Metrics = metricsSnapshot
	})
	n.metricsMu.RUnlock()

	n.mu.RUnlock()

	params, _ := n.ControlEncoder().EncodeNode(node)

	cmd := &control.Command{
		UID:    n.uid,
		Method: "node",
		Params: params,
	}

	err := n.nodeCmd(node)
	if err != nil {
		logger.ERROR.Println(err)
	}

	return <-n.publishControl(cmd)
}

// pubUnsubscribe publishes unsubscribe control message to all nodes – so all
// nodes could unsubscribe user from channel.
func (n *Node) pubUnsubscribe(user string, ch string) error {

	// TODO
	unsubscribe := &control.Unsubscribe{
		User: user,
	}

	params, _ := n.ControlEncoder().EncodeUnsubscribe(unsubscribe)

	cmd := &control.Command{
		UID:    n.uid,
		Method: "unsubscribe",
		Params: params,
	}

	return <-n.publishControl(cmd)
}

// pubDisconnect publishes disconnect control message to all nodes – so all
// nodes could disconnect user from Centrifugo.
func (n *Node) pubDisconnect(user string, reconnect bool) error {

	disconnect := &control.Disconnect{
		User: user,
	}

	params, _ := n.ControlEncoder().EncodeDisconnect(disconnect)

	cmd := &control.Command{
		UID:    n.uid,
		Method: "unsubscribe",
		Params: params,
	}

	return <-n.publishControl(cmd)
}

// AddClient registers authenticated connection in clientConnectionHub
// this allows to make operations with user connection on demand.
func (n *Node) AddClient(c Client) error {
	metricsRegistry.Counters.Inc("node_num_add_client_conn")
	return n.hub.Add(c)
}

// RemoveClient removes client connection from connection registry.
func (n *Node) RemoveClient(c Client) error {
	metricsRegistry.Counters.Inc("node_num_remove_client_conn")
	return n.hub.Remove(c)
}

// AddSubscription registers subscription of connection on channel in both
// engine and clientSubscriptionHub.
func (n *Node) AddSubscription(ch string, c Client) error {
	metricsRegistry.Counters.Inc("node_num_add_client_sub")
	first, err := n.hub.AddSub(ch, c)
	if err != nil {
		return err
	}
	if first {
		return n.engine.Subscribe(ch)
	}
	return nil
}

// RemoveSubscription removes subscription of connection on channel
// from both engine and clientSubscriptionHub.
func (n *Node) RemoveSubscription(ch string, c Client) error {
	metricsRegistry.Counters.Inc("node_num_remove_client_sub")
	empty, err := n.hub.RemoveSub(ch, c)
	if err != nil {
		return err
	}
	if empty {
		return n.engine.Unsubscribe(ch)
	}
	return nil
}

// nodeCmd handles ping control command i.e. updates information about known nodes.
func (n *Node) nodeCmd(node *control.Node) error {
	n.nodes.add(node)
	return nil
}

// Unsubscribe unsubscribes user from channel, if channel is equal to empty
// string then user will be unsubscribed from all channels.
func (n *Node) Unsubscribe(user string, ch string) error {

	if string(user) == "" {
		return proto.ErrBadRequest
	}

	if string(ch) != "" {
		_, ok := n.ChannelOpts(ch)
		if !ok {
			return proto.ErrNamespaceNotFound
		}
	}

	// First unsubscribe on this node.
	err := n.unsubscribeUser(user, ch)
	if err != nil {
		return proto.ErrInternalServerError
	}
	// Second send unsubscribe control message to other nodes.
	err = n.pubUnsubscribe(user, ch)
	if err != nil {
		return proto.ErrInternalServerError
	}
	return nil
}

// unsubscribeUser unsubscribes user from channel on this node. If channel
// is an empty string then user will be unsubscribed from all channels.
func (n *Node) unsubscribeUser(user string, ch string) error {
	userConnections := n.hub.UserConnections(user)
	for _, c := range userConnections {
		var channels []string
		if string(ch) == "" {
			// unsubscribe from all channels
			channels = c.Channels()
		} else {
			channels = []string{ch}
		}

		for _, channel := range channels {
			err := c.Unsubscribe(channel)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Disconnect allows to close all user connections to Centrifugo.
func (n *Node) Disconnect(user string, reconnect bool) error {

	if string(user) == "" {
		return proto.ErrBadRequest
	}

	// first disconnect user from this node
	err := n.disconnectUser(user, reconnect)
	if err != nil {
		return proto.ErrInternalServerError
	}
	// second send disconnect control message to other nodes
	err = n.pubDisconnect(user, reconnect)
	if err != nil {
		return proto.ErrInternalServerError
	}
	return nil
}

// disconnectUser closes client connections of user on current node.
func (n *Node) disconnectUser(user string, reconnect bool) error {
	userConnections := n.hub.UserConnections(user)
	advice := &proto.Disconnect{Reason: "disconnect", Reconnect: reconnect}
	for _, c := range userConnections {
		go func(cc Client) {
			cc.Close(advice)
		}(c)
	}
	return nil
}

// namespaceName returns namespace name from channel if exists.
func (n *Node) namespaceName(ch string) string {
	cTrim := strings.TrimPrefix(ch, n.config.PrivateChannelPrefix)
	if strings.Contains(cTrim, n.config.NamespaceChannelBoundary) {
		parts := strings.SplitN(cTrim, n.config.NamespaceChannelBoundary, 2)
		return parts[0]
	}
	return ""
}

// ChannelOpts returns channel options for channel using current channel config.
func (n *Node) ChannelOpts(ch string) (channel.Options, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.config.channelOpts(n.namespaceName(ch))
}

// AddPresence proxies presence adding to engine.
func (n *Node) AddPresence(ch string, uid string, info *proto.ClientInfo) error {
	n.mu.RLock()
	expire := int(n.config.PresenceExpireInterval.Seconds())
	n.mu.RUnlock()
	metricsRegistry.Counters.Inc("node_num_add_presence")
	return n.engine.AddPresence(ch, uid, info, expire)
}

// RemovePresence proxies presence removing to engine.
func (n *Node) RemovePresence(ch string, uid string) error {
	metricsRegistry.Counters.Inc("node_num_remove_presence")
	return n.engine.RemovePresence(ch, uid)
}

// Presence returns a map with information about active clients in channel.
func (n *Node) Presence(ch string) (map[string]*proto.ClientInfo, error) {

	metricsRegistry.Counters.Inc("node_num_presence")

	presence, err := n.engine.Presence(ch)
	if err != nil {
		logger.ERROR.Printf("error getting presence: %v", err)
		return nil, proto.ErrInternalServerError
	}
	return presence, nil
}

// History returns a slice of last messages published into project channel.
func (n *Node) History(ch string) ([]*proto.Publication, error) {
	metricsRegistry.Counters.Inc("node_num_history")

	publications, err := n.engine.History(ch, 0)
	if err != nil {
		return nil, err
	}
	return publications, nil
}

// RemoveHistory removes channel history.
func (n *Node) RemoveHistory(ch string) error {
	metricsRegistry.Counters.Inc("node_num_remove_history")
	return n.engine.RemoveHistory(ch)
}

// LastMessageID return last message id for channel.
func (n *Node) LastMessageID(ch string) (string, error) {
	metricsRegistry.Counters.Inc("node_num_last_message_id")
	publications, err := n.engine.History(ch, 1)
	if err != nil {
		return "", err
	}
	if len(publications) == 0 {
		return "", nil
	}
	return publications[0].UID, nil
}

// PrivateChannel checks if channel private and therefore subscription
// request on it must be properly signed on web application backend.
func (n *Node) PrivateChannel(ch string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return strings.HasPrefix(string(ch), n.config.PrivateChannelPrefix)
}

// UserAllowed checks if user can subscribe on channel - as channel
// can contain special part in the end to indicate which users allowed
// to subscribe on it.
func (n *Node) UserAllowed(ch string, user string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !strings.Contains(ch, n.config.UserChannelBoundary) {
		return true
	}
	parts := strings.Split(ch, n.config.UserChannelBoundary)
	allowedUsers := strings.Split(parts[len(parts)-1], n.config.UserChannelSeparator)
	for _, allowedUser := range allowedUsers {
		if string(user) == allowedUser {
			return true
		}
	}
	return false
}

// ClientAllowed checks if client can subscribe on channel - as channel
// can contain special part in the end to indicate which client allowed
// to subscribe on it.
func (n *Node) ClientAllowed(ch string, client string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !strings.Contains(ch, n.config.ClientChannelBoundary) {
		return true
	}
	parts := strings.Split(ch, n.config.ClientChannelBoundary)
	allowedClient := parts[len(parts)-1]
	if string(client) == allowedClient {
		return true
	}
	return false
}
