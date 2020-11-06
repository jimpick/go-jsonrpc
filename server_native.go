// +build !js

package jsonrpc

import (
	"context"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// NewServer creates new RPCServer instance
func NewServer(opts ...ServerOption) *RPCServer {
	config := defaultServerConfig()
	for _, o := range opts {
		o(&config)
	}

	return &RPCServer{
		methods:       map[string]rpcHandler{},
		paramDecoders: config.paramDecoders,
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (s *RPCServer) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// TODO: allow setting
	// (note that we still are mostly covered by jwt tokens)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Header.Get("Sec-WebSocket-Protocol") != "" {
		w.Header().Set("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err)
		w.WriteHeader(500)
		return
	}

	(&wsConn{
		rpcConnection: rpcConnection{
			handler: s,
			exiting: make(chan struct{}),
		},
		conn: c,
	}).handleWsConn(ctx)

	if err := c.Close(); err != nil {
		log.Error(err)
		return
	}
}

// TODO: return errors to clients per spec
func (s *RPCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	h := strings.ToLower(r.Header.Get("Connection"))
	if strings.Contains(h, "upgrade") {
		s.handleWS(ctx, w, r)
		return
	}

	s.handleReader(ctx, r.Body, w, rpcError)
}
