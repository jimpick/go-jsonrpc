// +build js

package jsonrpc

import (
	"context"
	"fmt"
	"strings"
	"syscall/js"
)

// NewJSServer creates new RPCServer instance
func NewJSServer(globalJSName string, opts ...ServerOption) *RPCServer {
	ctx := context.Background()

	config := defaultServerConfig()
	for _, o := range opts {
		o(&config)
	}

	s := &RPCServer{
		methods:       map[string]rpcHandler{},
		paramDecoders: config.paramDecoders,
	}

	connectFunc := func(this js.Value, param []js.Value) interface{} {
		responseHandler := param[0]
		environment := param[1]
		fmt.Printf("go-jsonrpc connect\n")
		if config.connectCallback != nil {
			config.connectCallback(environment)
		}

		c := jsConn{
			rpcConnection: rpcConnection{
				handler: s,
				exiting: make(chan struct{}),
			},
			sendResponse: func(response string) {
				fmt.Printf("go-jsonrpc sendResponse %v\n", response)
				responseHandler.Invoke(js.ValueOf(response))
			},
		}
		go c.handleJsConn(ctx)

		receiveFunc := func(this js.Value, param []js.Value) interface{} {
			request := param[0].String()
			fmt.Printf("go-jsonrpc receive: %v\n", request)
			go func() {
				// time.Sleep(1 * time.Second)
				c.incoming <- strings.NewReader(request)
			}()
			return nil
		}

		return js.FuncOf(receiveFunc)
	}

	js.Global().Set(globalJSName, js.FuncOf(connectFunc))

	return s
}

// TODO: return errors to clients per spec
/*
func (s *RPCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	h := strings.ToLower(r.Header.Get("Connection"))
	if strings.Contains(h, "upgrade") {
		s.handleWS(ctx, w, r)
		return
	}

	s.handleReader(ctx, r.Body, w, rpcError)
}
*/

func WithConnectCallback(cb func(environment js.Value)) ServerOption {
	return func(c *ServerConfig) {
		c.connectCallback = cb
	}
}
