// Copyright 2015 The go-ethereum Authors
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
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/log"
)

const MetadataApi = "rpc"
const EngineApi = "engine"

// CodecOption specifies which type of messages a codec supports.
//
// Deprecated: this option is no longer honored by Server.
type CodecOption int

const (
	// OptionMethodInvocation is an indication that the codec supports RPC method calls
	OptionMethodInvocation CodecOption = 1 << iota

	// OptionSubscriptions is an indication that the codec supports RPC notifications
	OptionSubscriptions = 1 << iota // support pub sub
)

// Server is an RPC server.
// RPC Server 结构体
type Server struct {
	services serviceRegistry // 注册的服务，是个 map[string]service 结构，每个服务都需要有服务名
	idgen    func() ID       // 随机 ID 生成器

	mutex              sync.Mutex               // 锁
	codecs             map[ServerCodec]struct{} // 编解码器，读取和回复
	run                atomic.Bool              // 是否接受请求的开关，为 true 时才接受新的请求
	batchItemLimit     int
	batchResponseLimit int
	httpBodyLimit      int
	// 因为 json-rpc 是基于 http 请求的，json rpc 的内容放在请求体中，所以限制请求体大小就等于限制了 json-rpc 传输数据的大小
}

// NewServer creates a new server instance with no registered handlers.
// 创建一个新的 Server 实例，注册了一个默认的 handlers，并初始化了 id 生成器，httpBodyLimit 和编解码器
func NewServer() *Server {
	server := &Server{
		idgen:         randomIDGenerator(),
		codecs:        make(map[ServerCodec]struct{}),
		httpBodyLimit: defaultBodyLimit, // 5 * 1024 * 1024
	}
	server.run.Store(true)
	// Register the default service providing meta information about the RPC service such
	// as the services and methods it offers.
	rpcService := &RPCService{server}
	server.RegisterName(MetadataApi, rpcService)
	// 注册一个初始的服务，会注册一个返回 RPC 所有服务列表和版本的回调:map[name]version
	return server
}

// SetBatchLimits sets limits applied to batch requests. There are two limits: 'itemLimit'
// is the maximum number of items in a batch. 'maxResponseSize' is the maximum number of
// response bytes across all requests in a batch.
//
// This method should be called before processing any requests via ServeCodec, ServeHTTP,
// ServeListener etc.
func (s *Server) SetBatchLimits(itemLimit, maxResponseSize int) {
	s.batchItemLimit = itemLimit
	s.batchResponseLimit = maxResponseSize
}

// SetHTTPBodyLimit sets the size limit for HTTP requests.
//
// This method should be called before processing any requests via ServeHTTP.
func (s *Server) SetHTTPBodyLimit(limit int) {
	s.httpBodyLimit = limit
}

// RegisterName creates a service for the given receiver type under the given name. When no
// methods on the given receiver match the criteria to be either an RPC method or a
// subscription an error is returned. Otherwise a new service is created and added to the
// service collection this server provides to clients.
// 注册服务，其实就是在 services 这个 map 中添加一个（name，service）
func (s *Server) RegisterName(name string, receiver interface{}) error {
	return s.services.registerName(name, receiver)
}

// ServeCodec reads incoming requests from codec, calls the appropriate callback and writes
// the response back using the given codec. It will block until the codec is closed or the
// server is stopped. In either case the codec is closed.
//
// Note that codec options are no longer supported.
// 提供编解码抽象层服务，实际就是在 conn 上封装了一层，来解析 jsonrpc 的数据
// 如：之前用过的 jsonrpc 包：rpc.ServeCodec(jsonrpc.NewServerCodec(conn))
func (s *Server) ServeCodec(codec ServerCodec, options CodecOption) {
	defer codec.close()

	if !s.trackCodec(codec) {
		return
	} // 注册一个 ServerCodec
	defer s.untrackCodec(codec) // 移除

	cfg := &clientConfig{
		idgen:              s.idgen,
		batchItemLimit:     s.batchItemLimit,
		batchResponseLimit: s.batchResponseLimit,
	}
	c := initClient(codec, &s.services, cfg) // 初始化客户端，并进行等待交互，他这里会一直循环
	<-codec.closed()                         // 阻塞，等待 codec 或者 server 结束
	c.Close()
}

// 设置 ServerCodec
func (s *Server) trackCodec(codec ServerCodec) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if !s.run.Load() {
		return false // Don't serve if server is stopped.
	}
	s.codecs[codec] = struct{}{}
	return true
}

// 取消 ServerCodec
func (s *Server) untrackCodec(codec ServerCodec) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	delete(s.codecs, codec)
}

// serveSingleRequest reads and processes a single RPC request from the given codec. This
// is used to serve HTTP connections. Subscriptions and reverse calls are not allowed in
// this mode.
// 服务单个基于 http 的 RPC 请求，直接读取请求数据，然后解析处理之后返回，就不允许订阅之类的功能
func (s *Server) serveSingleRequest(ctx context.Context, codec ServerCodec) {
	// Don't serve if server is stopped.
	if !s.run.Load() {
		return
	}

	h := newHandler(ctx, codec, s.idgen, &s.services, s.batchItemLimit, s.batchResponseLimit)
	h.allowSubscribe = false
	defer h.close(io.EOF, nil)

	reqs, batch, err := codec.readBatch()
	if err != nil {
		if msg := messageForReadError(err); msg != "" {
			resp := errorMessage(&invalidMessageError{msg})
			codec.writeJSON(ctx, resp, true)
		}
		return
	}
	if batch {
		h.handleBatch(reqs)
	} else {
		h.handleMsg(reqs[0])
	}
}

func messageForReadError(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "read timeout"
		} else {
			return "read error"
		}
	} else if err != io.EOF {
		return "parse error"
	}
	return ""
}

// Stop stops reading new requests, waits for stopPendingRequestTimeout to allow pending
// requests to finish, then closes all codecs which will cancel pending requests and
// subscriptions.
// 停止服务，他会停止接收新的请求，但是会设置一个超时时间等待已有的请求完成，超时后，会关闭所有的请求和订阅
func (s *Server) Stop() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// cas 操作，保证只会执行一次
	if s.run.CompareAndSwap(true, false) {
		log.Debug("RPC server shutting down")
		for codec := range s.codecs {
			codec.close()
		}
	}
}

// RPCService gives meta information about the server.
// e.g. gives information about the loaded modules.
type RPCService struct {
	server *Server
}

// Modules returns the list of RPC services with their version number
func (s *RPCService) Modules() map[string]string {
	s.server.services.mu.Lock()
	defer s.server.services.mu.Unlock()

	modules := make(map[string]string)
	for name := range s.server.services.services {
		modules[name] = "1.0"
	}
	return modules
}

// PeerInfo contains information about the remote end of the network connection.
//
// This is available within RPC method handlers through the context. Call
// PeerInfoFromContext to get information about the client connection related to
// the current method call.
// 远程链接的客户端的信息
type PeerInfo struct {
	// Transport is name of the protocol used by the client.
	// This can be "http", "ws" or "ipc".
	Transport string

	// Address of client. This will usually contain the IP address and port.
	RemoteAddr string

	// Additional information for HTTP and WebSocket connections.
	HTTP struct {
		// Protocol version, i.e. "HTTP/1.1". This is not set for WebSocket.
		Version string
		// Header values sent by the client.
		UserAgent string
		Origin    string
		Host      string
	}
}

type peerInfoContextKey struct{}

// PeerInfoFromContext returns information about the client's network connection.
// Use this with the context passed to RPC method handler functions.
//
// The zero value is returned if no connection info is present in ctx.
func PeerInfoFromContext(ctx context.Context) PeerInfo {
	info, _ := ctx.Value(peerInfoContextKey{}).(PeerInfo)
	return info
}
