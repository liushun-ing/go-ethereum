// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

var (
	ErrBadResult                 = errors.New("bad result in JSON-RPC response")
	ErrClientQuit                = errors.New("client is closed")
	ErrNoResult                  = errors.New("JSON-RPC response has no result")
	ErrMissingBatchResponse      = errors.New("response batch did not contain a response to this call")
	ErrSubscriptionQueueOverflow = errors.New("subscription queue overflow")
	errClientReconnected         = errors.New("client reconnected")
	errDead                      = errors.New("connection lost")
)

// Timeouts
const (
	defaultDialTimeout = 10 * time.Second // used if context has no deadline
	subscribeTimeout   = 10 * time.Second // overall timeout eth_subscribe, rpc_modules calls
)

const (
	// Subscriptions are removed when the subscriber cannot keep up.
	//
	// This can be worked around by supplying a channel with sufficiently sized buffer,
	// but this can be inconvenient and hard to explain in the docs. Another issue with
	// buffered channels is that the buffer is static even though it might not be needed
	// most of the time.
	//
	// The approach taken here is to maintain a per-subscription linked list buffer
	// shrinks on demand. If the buffer reaches the size below, the subscription is
	// dropped.
	maxClientSubscriptionBuffer = 20000
)

// BatchElem is an element in a batch request.
// batch 请求的对象 ???
type BatchElem struct {
	Method string
	Args   []interface{}
	// The result is unmarshaled into this field. Result must be set to a
	// non-nil pointer value of the desired type, otherwise the response will be
	// discarded.
	Result interface{}
	// Error is set if the server returns an error for this request, or if
	// unmarshalling into Result fails. It is not set for I/O errors.
	Error error
}

// Client represents a connection to an RPC server.
// 客户对象，表示和 RPC server 的一个连接
type Client struct {
	idgen func() ID // for subscriptions // 订阅的 id，如 0xcd0c3e8af590364c09d0fa6a1210faf5
	// 这就决定了处理方式，如果是 http 直接处理返回即可，但是是 ws 和 ipc 的话，需要保持双向通道，一直监听，这一般用于订阅
	isHTTP   bool // connection type: http, ws or ipc
	services *serviceRegistry

	// 请求的唯一标识符，用于将请求和响应进行匹配
	idCounter atomic.Uint32

	// This function, if non-nil, is called when the connection is lost.
	reconnectFunc reconnectFunc

	// config fields
	batchItemLimit       int
	batchResponseMaxSize int

	// writeConn is used for writing to the connection on the caller's goroutine. It should
	// only be accessed outside of dispatch, with the write lock held. The write lock is
	// taken by sending on reqInit and released by sending on reqSent.
	writeConn jsonWriter

	// for dispatch
	close       chan struct{}
	closing     chan struct{}    // closed when client is quitting
	didClose    chan struct{}    // closed when client quits
	reconnected chan ServerCodec // where write/reconnect sends the new connection
	readOp      chan readOp      // read messages
	readErr     chan error       // errors from read
	reqInit     chan *requestOp  // register response IDs, takes write lock
	reqSent     chan error       // signals write completion, releases write lock
	reqTimeout  chan *requestOp  // removes response IDs when call timeout expires
}

type reconnectFunc func(context.Context) (ServerCodec, error)

type clientContextKey struct{}

// 客户端连接对象
type clientConn struct {
	codec   ServerCodec
	handler *handler
}

// 创建新的客户端连接对象
func (c *Client) newClientConn(conn ServerCodec) *clientConn {
	// 设置上下文，并往上下文中添加两个属性
	ctx := context.Background()
	ctx = context.WithValue(ctx, clientContextKey{}, c)
	ctx = context.WithValue(ctx, peerInfoContextKey{}, conn.peerInfo())
	// 生成处理器对象，这个处理器就包含了上下文
	handler := newHandler(ctx, conn, c.idgen, c.services, c.batchItemLimit, c.batchResponseMaxSize)
	return &clientConn{conn, handler}
}

// 关闭客户端连接
func (cc *clientConn) close(err error, inflightReq *requestOp) {
	cc.handler.close(err, inflightReq)
	cc.codec.close()
}

// 读取操作的实体
type readOp struct {
	//  应该 batch 操作就会有多个 jsonrpcMessage
	msgs  []*jsonrpcMessage
	batch bool
}

// requestOp represents a pending request. This is used for both batch and non-batch
// requests.
type requestOp struct {
	ids         []json.RawMessage
	err         error
	resp        chan []*jsonrpcMessage // the response goes here
	sub         *ClientSubscription    // set for Subscribe requests.
	hadResponse bool                   // true when the request was responded to
}

func (op *requestOp) wait(ctx context.Context, c *Client) ([]*jsonrpcMessage, error) {
	select {
	case <-ctx.Done(): // 上下文关闭
		// Send the timeout to dispatch so it can remove the request IDs.
		if !c.isHTTP {
			select {
			case c.reqTimeout <- op: // 尝试将 op 写入通道，如果在发送之前从 c.closing 通道接收到关闭信号，则放弃发送操作
			case <-c.closing: // 等待关闭信号
			}
		}
		return nil, ctx.Err()
	case resp := <-op.resp: // 或者读取到了返回值
		return resp, op.err
	}
}

// Dial creates a new client for the given URL.
//
// The currently supported URL schemes are "http", "https", "ws" and "wss". If rawurl is a
// file name with no URL scheme, a local socket connection is established using UNIX
// domain sockets on supported platforms and named pipes on Windows.
//
// If you want to further configure the transport, use DialOptions instead of this
// function.
//
// For websocket connections, the origin is set to the local host name.
//
// The client reconnects automatically when the connection is lost.
func Dial(rawurl string) (*Client, error) {
	return DialOptions(context.Background(), rawurl)
}

// DialContext creates a new RPC client, just like Dial.
//
// The context is used to cancel or time out the initial connection establishment. It does
// not affect subsequent interactions with the client.
func DialContext(ctx context.Context, rawurl string) (*Client, error) {
	return DialOptions(ctx, rawurl)
}

// DialOptions creates a new RPC client for the given URL. You can supply any of the
// pre-defined client options to configure the underlying transport.
//
// The context is used to cancel or time out the initial connection establishment. It does
// not affect subsequent interactions with the client.
//
// The client reconnects automatically when the connection is lost.
// 创建一个新的客户端，并根据不同的协议，生成不同的 serverCodec，也就是编码解码和接受返回器
func DialOptions(ctx context.Context, rawurl string, options ...ClientOption) (*Client, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}

	cfg := new(clientConfig)
	for _, opt := range options {
		opt.applyOption(cfg)
	}

	var reconnect reconnectFunc
	switch u.Scheme {
	case "http", "https":
		reconnect = newClientTransportHTTP(rawurl, cfg)
	case "ws", "wss":
		rc, err := newClientTransportWS(rawurl, cfg)
		if err != nil {
			return nil, err
		}
		reconnect = rc
	case "stdio":
		reconnect = newClientTransportIO(os.Stdin, os.Stdout)
	case "":
		reconnect = newClientTransportIPC(rawurl)
	default:
		return nil, fmt.Errorf("no known transport for URL scheme %q", u.Scheme)
	}

	return newClient(ctx, cfg, reconnect)
}

// ClientFromContext retrieves the client from the context, if any. This can be used to perform
// 'reverse calls' in a handler method.
// 从上下文中获取 client 对象
func ClientFromContext(ctx context.Context) (*Client, bool) {
	client, ok := ctx.Value(clientContextKey{}).(*Client)
	return client, ok
}

// 创建一个新的客户端，并初始化
func newClient(initctx context.Context, cfg *clientConfig, connect reconnectFunc) (*Client, error) {
	conn, err := connect(initctx)
	if err != nil {
		return nil, err
	}
	c := initClient(conn, new(serviceRegistry), cfg)
	c.reconnectFunc = connect
	return c, nil
}

// 初始化一个 ServerCodec 的 client
func initClient(conn ServerCodec, services *serviceRegistry, cfg *clientConfig) *Client {
	_, isHTTP := conn.(*httpConn)
	c := &Client{
		isHTTP:               isHTTP,
		services:             services,
		idgen:                cfg.idgen,
		batchItemLimit:       cfg.batchItemLimit,
		batchResponseMaxSize: cfg.batchResponseLimit,
		writeConn:            conn,
		close:                make(chan struct{}),
		closing:              make(chan struct{}),
		didClose:             make(chan struct{}),
		reconnected:          make(chan ServerCodec),
		readOp:               make(chan readOp),
		readErr:              make(chan error),
		reqInit:              make(chan *requestOp),
		reqSent:              make(chan error, 1),
		reqTimeout:           make(chan *requestOp),
	}

	// Set defaults.
	if c.idgen == nil {
		c.idgen = randomIDGenerator()
	}

	// Launch the main loop.
	if !isHTTP { // 循环处理，但是单个 http 请求不需要一直循环监听，所以需要 !isHTTP
		go c.dispatch(conn)
	}
	return c
}

// RegisterName creates a service for the given receiver type under the given name. When no
// methods on the given receiver match the criteria to be either a RPC method or a
// subscription an error is returned. Otherwise a new service is created and added to the
// service collection this client provides to the server.
// 注册一个服务
func (c *Client) RegisterName(name string, receiver interface{}) error {
	return c.services.registerName(name, receiver)
}

// id 递增
func (c *Client) nextID() json.RawMessage {
	id := c.idCounter.Add(1)
	return strconv.AppendUint(nil, uint64(id), 10)
}

// SupportedModules calls the rpc_modules method, retrieving the list of
// APIs that are available on the server.
// 获取支持的服务模块
// 前面提到在服务启动的时候，注册了一个默认的服务，注册名字为 rpc，有一个 Modules 方法，
// 看来 rpc 里的实现，是 服务名_名字（格式化将第一个字母变成小写）
func (c *Client) SupportedModules() (map[string]string, error) {
	var result map[string]string
	ctx, cancel := context.WithTimeout(context.Background(), subscribeTimeout)
	defer cancel()
	err := c.CallContext(ctx, &result, "rpc_modules")
	return result, err
}

// Close closes the client, aborting any in-flight requests.
// 关闭客户端
func (c *Client) Close() {
	if c.isHTTP {
		return
	}
	// 好好体会这：尝试向 c.close 发送一个消息，并等待 c.didClose 的消息
	// 如果发送消息失败，还是会进行等待
	select {
	case c.close <- struct{}{}:
		<-c.didClose
	case <-c.didClose:
	}
}

// SetHeader adds a custom HTTP header to the client's requests.
// This method only works for clients using HTTP, it doesn't have
// any effect for clients using another transport.
// 设置 http 请求的请求头
func (c *Client) SetHeader(key, value string) {
	if !c.isHTTP {
		return
	}
	conn := c.writeConn.(*httpConn)
	conn.mu.Lock()
	conn.headers.Set(key, value)
	conn.mu.Unlock()
}

// Call performs a JSON-RPC call with the given arguments and unmarshals into
// result if no error occurred.
//
// The result must be a pointer so that package json can unmarshal into it. You
// can also pass nil, in which case the result is ignored.
// 执行一个 rpc 调用，会创建一个新 context, 然后调用 CallContext
func (c *Client) Call(result interface{}, method string, args ...interface{}) error {
	ctx := context.Background()
	return c.CallContext(ctx, result, method, args...)
}

// CallContext performs a JSON-RPC call with the given arguments. If the context is
// canceled before the call has successfully returned, CallContext returns immediately.
//
// The result must be a pointer so that package json can unmarshal into it. You
// can also pass nil, in which case the result is ignored.
// 真正调用一个带上下文的 json-rpc 调用
func (c *Client) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	if result != nil && reflect.TypeOf(result).Kind() != reflect.Ptr {
		return fmt.Errorf("call result parameter must be pointer or nil interface: %v", result)
	}
	// 构建一个 rpc 的调用消息
	msg, err := c.newMessage(method, args...)
	if err != nil {
		return err
	}
	// 不是一个 batch，所以resp 大小为 1
	op := &requestOp{
		ids:  []json.RawMessage{msg.ID},
		resp: make(chan []*jsonrpcMessage, 1),
	}

	// 发送请求
	if c.isHTTP {
		// 借用 http 库发送 http 请求，最终的结果将会写入 op.resp 管道中
		err = c.sendHTTP(ctx, op, msg)
	} else {
		err = c.send(ctx, op, msg)
	}
	if err != nil {
		return err
	}

	// dispatch has accepted the request and will close the channel when it quits.
	// 等待上下文结束或者读取到了返回值
	batchresp, err := op.wait(ctx, c)
	if err != nil {
		return err
	}
	resp := batchresp[0] // 实际上没有 batch，就读取第一个就行
	switch {
	case resp.Error != nil:
		return resp.Error
	case len(resp.Result) == 0:
		return ErrNoResult
	default:
		if result == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, result) // 反序列化到 result 后返回
	}
}

// BatchCall sends all given requests as a single batch and waits for the server
// to return a response for all of them.
//
// In contrast to Call, BatchCall only returns I/O errors. Any error specific to
// a request is reported through the Error field of the corresponding BatchElem.
//
// Note that batch calls may not be executed atomically on the server side.
func (c *Client) BatchCall(b []BatchElem) error {
	ctx := context.Background()
	return c.BatchCallContext(ctx, b)
}

// BatchCallContext sends all given requests as a single batch and waits for the server
// to return a response for all of them. The wait duration is bounded by the
// context's deadline.
//
// In contrast to CallContext, BatchCallContext only returns errors that have occurred
// while sending the request. Any error specific to a request is reported through the
// Error field of the corresponding BatchElem.
//
// Note that batch calls may not be executed atomically on the server side.
func (c *Client) BatchCallContext(ctx context.Context, b []BatchElem) error {
	var (
		msgs = make([]*jsonrpcMessage, len(b))
		byID = make(map[string]int, len(b))
	)
	op := &requestOp{
		ids:  make([]json.RawMessage, len(b)),
		resp: make(chan []*jsonrpcMessage, 1), // 注意这里的大小也只有 1，说明只能顺序处理
	}
	// 发送批次的请求，实际上是将 message 组织成一个数组，然后发送一个请求
	for i, elem := range b {
		msg, err := c.newMessage(elem.Method, elem.Args...)
		if err != nil {
			return err
		}
		msgs[i] = msg
		op.ids[i] = msg.ID
		byID[string(msg.ID)] = i
	}

	var err error
	if c.isHTTP {
		err = c.sendBatchHTTP(ctx, op, msgs)
	} else {
		err = c.send(ctx, op, msgs)
	}
	if err != nil {
		return err
	}

	batchresp, err := op.wait(ctx, c)
	if err != nil {
		return err
	}

	// Wait for all responses to come back.
	// 循环处理所有结果
	for n := 0; n < len(batchresp); n++ {
		resp := batchresp[n]
		if resp == nil {
			// Ignore null responses. These can happen for batches sent via HTTP.
			continue
		}

		// Find the element corresponding to this response.
		index, ok := byID[string(resp.ID)]
		if !ok {
			continue
		}
		delete(byID, string(resp.ID))

		// Assign result and error.
		elem := &b[index]
		switch {
		case resp.Error != nil:
			elem.Error = resp.Error
		case resp.Result == nil:
			elem.Error = ErrNoResult
		default:
			elem.Error = json.Unmarshal(resp.Result, elem.Result)
		}
	}

	// Check that all expected responses have been received.
	for _, index := range byID {
		elem := &b[index]
		elem.Error = ErrMissingBatchResponse
	}

	return err
}

// Notify sends a notification, i.e. a method call that doesn't expect a response.
// 只需要发送 json-rpc 请求，不需要有返回
func (c *Client) Notify(ctx context.Context, method string, args ...interface{}) error {
	op := new(requestOp)
	msg, err := c.newMessage(method, args...)
	if err != nil {
		return err
	}
	msg.ID = nil

	if c.isHTTP {
		return c.sendHTTP(ctx, op, msg)
	}
	return c.send(ctx, op, msg)
}

// EthSubscribe registers a subscription under the "eth" namespace.
func (c *Client) EthSubscribe(ctx context.Context, channel interface{}, args ...interface{}) (*ClientSubscription, error) {
	return c.Subscribe(ctx, "eth", channel, args...)
}

// ShhSubscribe registers a subscription under the "shh" namespace.
// Deprecated: use Subscribe(ctx, "shh", ...).
func (c *Client) ShhSubscribe(ctx context.Context, channel interface{}, args ...interface{}) (*ClientSubscription, error) {
	return c.Subscribe(ctx, "shh", channel, args...)
}

// Subscribe calls the "<namespace>_subscribe" method with the given arguments,
// registering a subscription. Server notifications for the subscription are
// sent to the given channel. The element type of the channel must match the
// expected type of content returned by the subscription.
//
// The context argument cancels the RPC request that sets up the subscription but has no
// effect on the subscription after Subscribe has returned.
//
// Slow subscribers will be dropped eventually. Client buffers up to 20000 notifications
// before considering the subscriber dead. The subscription Err channel will receive
// ErrSubscriptionQueueOverflow. Use a sufficiently large buffer on the channel or ensure
// that the channel usually has at least one reader to prevent this issue.
// 订阅，执行 <namespace>_subscribe
func (c *Client) Subscribe(ctx context.Context, namespace string, channel interface{}, args ...interface{}) (*ClientSubscription, error) {
	// Check type of channel first. 需要是一个可写的 channel
	chanVal := reflect.ValueOf(channel)
	if chanVal.Kind() != reflect.Chan || chanVal.Type().ChanDir()&reflect.SendDir == 0 {
		panic(fmt.Sprintf("channel argument of Subscribe has type %T, need writable channel", channel))
	}
	if chanVal.IsNil() {
		panic("channel given to Subscribe must not be nil")
	}
	if c.isHTTP {
		return nil, ErrNotificationsUnsupported
	} // http 不支持订阅

	msg, err := c.newMessage(namespace+subscribeMethodSuffix, args...) // 创建一个订阅消息
	if err != nil {
		return nil, err
	}
	// 新建一个操作对象
	op := &requestOp{
		ids:  []json.RawMessage{msg.ID},
		resp: make(chan []*jsonrpcMessage, 1),
		sub:  newClientSubscription(c, namespace, chanVal),
	}

	// Send the subscription request.
	// The arrival and validity of the response is signaled on sub.quit.
	if err := c.send(ctx, op, msg); err != nil {
		return nil, err
	}
	if _, err := op.wait(ctx, c); err != nil {
		return nil, err
	} // 发送并等待
	return op.sub, nil
}

// SupportsSubscriptions reports whether subscriptions are supported by the client
// transport. When this returns false, Subscribe and related methods will return
// ErrNotificationsUnsupported.
func (c *Client) SupportsSubscriptions() bool {
	return !c.isHTTP
}

// 构建一个新消息
func (c *Client) newMessage(method string, paramsIn ...interface{}) (*jsonrpcMessage, error) {
	msg := &jsonrpcMessage{Version: vsn, ID: c.nextID(), Method: method}
	if paramsIn != nil { // prevent sending "params":null
		var err error
		if msg.Params, err = json.Marshal(paramsIn); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

// send registers op with the dispatch loop, then sends msg on the connection.
// if sending fails, op is deregistered.
func (c *Client) send(ctx context.Context, op *requestOp, msg interface{}) error {
	select {
	case c.reqInit <- op: // 将当前 op 发送到通道，然后开始写
		err := c.write(ctx, msg, false) // 写成功了 err 就为 nil
		c.reqSent <- err
		return err
	case <-ctx.Done():
		// This can happen if the client is overloaded or unable to keep up with
		// subscription notifications.
		return ctx.Err()
	case <-c.closing:
		return ErrClientQuit
	}
}

// 发送消息 c.writeConn.writeJSON(ctx, msg, false)
func (c *Client) write(ctx context.Context, msg interface{}, retry bool) error {
	if c.writeConn == nil {
		// The previous write failed. Try to establish a new connection.
		if err := c.reconnect(ctx); err != nil {
			return err
		}
	}
	err := c.writeConn.writeJSON(ctx, msg, false)
	if err != nil { // 写入失败就重试
		c.writeConn = nil
		if !retry {
			return c.write(ctx, msg, true)
		}
	}
	return err
}

// 重新连接，如果 writeConn 为 nil，就重新创建一个
func (c *Client) reconnect(ctx context.Context) error {
	if c.reconnectFunc == nil {
		return errDead
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, defaultDialTimeout)
		defer cancel()
	}
	newconn, err := c.reconnectFunc(ctx)
	if err != nil {
		log.Trace("RPC client reconnect failed", "err", err)
		return err
	}
	select {
	case c.reconnected <- newconn:
		c.writeConn = newconn
		return nil
	case <-c.didClose:
		newconn.close()
		return ErrClientQuit
	}
}

// dispatch is the main loop of the client.
// It sends read messages to waiting calls to Call and BatchCall
// and subscription notifications to registered subscriptions.
// 循环监听各种事件，读取各个通道的消息并处理，调用各种方法的时候，会产生各种消息
// 他会在客户端创建的时候就使用 goroutine 开启。
func (c *Client) dispatch(codec ServerCodec) {
	var (
		lastOp      *requestOp  // tracks last send operation
		reqInitLock = c.reqInit // nil while the send lock is held
		conn        = c.newClientConn(codec)
		reading     = true
	)
	// 关闭连接
	defer func() {
		close(c.closing)
		if reading {
			conn.close(ErrClientQuit, nil)
			c.drainRead()
		}
		close(c.didClose)
	}()

	// Spawn the initial read loop.
	go c.read(codec)

	for {
		select {
		case <-c.close:
			return

		// Read path:
		case op := <-c.readOp:
			if op.batch {
				conn.handler.handleBatch(op.msgs)
			} else {
				conn.handler.handleMsg(op.msgs[0])
			}

		case err := <-c.readErr:
			conn.handler.log.Debug("RPC connection read error", "err", err)
			conn.close(err, lastOp)
			reading = false

		// Reconnect:
		// 读出重新连接，关闭当前的，然后重新 go read()
		case newcodec := <-c.reconnected:
			log.Debug("RPC client reconnected", "reading", reading, "conn", newcodec.remoteAddr())
			if reading {
				// Wait for the previous read loop to exit. This is a rare case which
				// happens if this loop isn't notified in time after the connection breaks.
				// In those cases the caller will notice first and reconnect. Closing the
				// handler terminates all waiting requests (closing op.resp) except for
				// lastOp, which will be transferred to the new handler.
				conn.close(errClientReconnected, lastOp)
				c.drainRead() // 读取完之前的消息
			}
			go c.read(newcodec)
			reading = true
			conn = c.newClientConn(newcodec)
			// Re-register the in-flight request on the new handler
			// because that's where it will be sent.
			conn.handler.addRequestOp(lastOp)

		// Send path:
		case op := <-reqInitLock:
			// Stop listening for further requests until the current one has been sent.
			reqInitLock = nil
			lastOp = op
			conn.handler.addRequestOp(op)

		case err := <-c.reqSent:
			if err != nil {
				// Remove response handlers for the last send. When the read loop
				// goes down, it will signal all other current operations.
				conn.handler.removeRequestOp(lastOp)
			}
			// Let the next request in.
			reqInitLock = c.reqInit
			lastOp = nil

		case op := <-c.reqTimeout:
			conn.handler.removeRequestOp(op)
		}
	}
}

// drainRead drops read messages until an error occurs.
// 不断读数据，然后丢弃，直到没有消息了
func (c *Client) drainRead() {
	for {
		select {
		case <-c.readOp:
		case <-c.readErr:
			return
		}
	}
}

// read decodes RPC messages from a codec, feeding them into dispatch.
func (c *Client) read(codec ServerCodec) {
	for {
		msgs, batch, err := codec.readBatch()
		if _, ok := err.(*json.SyntaxError); ok {
			msg := errorMessage(&parseError{err.Error()})
			codec.writeJSON(context.Background(), msg, true)
		}
		if err != nil {
			c.readErr <- err
			return
		}
		c.readOp <- readOp{msgs, batch}
	}
}
