package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"sync"
	"sync/atomic"

	"golang.org/x/xerrors"
)

const wsCancel = "xrpc.cancel"
const chValue = "xrpc.ch.val"
const chClose = "xrpc.ch.close"

type frame struct {
	// common
	Jsonrpc string            `json:"jsonrpc"`
	ID      *int64            `json:"id,omitempty"`
	Meta    map[string]string `json:"meta,omitempty"`

	// request
	Method string  `json:"method,omitempty"`
	Params []param `json:"params,omitempty"`

	// response
	Result json.RawMessage `json:"result,omitempty"`
	Error  *respError      `json:"error,omitempty"`
}

type outChanReg struct {
	reqID int64

	chID uint64
	ch   reflect.Value
}

type rpcConnectionImpl interface {
}

type rpcConnection struct {
	rpcConnectionImpl

	// outside params
	handler  *RPCServer
	requests <-chan clientRequest
	stop     <-chan struct{}
	exiting  chan struct{}

	// incoming messages
	incoming    chan io.Reader
	incomingErr error

	// outgoing messages
	writeLk sync.Mutex

	// ////
	// Client related

	// inflight are requests we've sent to the remote
	inflight map[int64]clientRequest

	// chanHandlers is a map of client-side channel handlers
	chanHandlers map[uint64]func(m []byte, ok bool)

	// ////
	// Server related

	// handling are the calls we handle
	handling   map[int64]context.CancelFunc
	handlingLk sync.Mutex

	spawnOutChanHandlerOnce sync.Once

	// chanCtr is a counter used for identifying output channels on the server side
	chanCtr uint64

	registerCh chan outChanReg

	// Added by concrete implementation
	sendRequest func(req request) error
	nextWriter  func(cb func(io.Writer))
}

//                 //
// Output channels //
//                 //

// handleOutChans handles channel communication on the server side
// (forwards channel messages to client)
func (c *rpcConnection) handleOutChans() {
	regV := reflect.ValueOf(c.registerCh)
	exitV := reflect.ValueOf(c.exiting)

	cases := []reflect.SelectCase{
		{ // registration chan always 0
			Dir:  reflect.SelectRecv,
			Chan: regV,
		},
		{ // exit chan always 1
			Dir:  reflect.SelectRecv,
			Chan: exitV,
		},
	}
	internal := len(cases)
	var caseToID []uint64

	for {
		chosen, val, ok := reflect.Select(cases)

		switch chosen {
		case 0: // registration channel
			if !ok {
				// control channel closed - signals closed connection
				// This shouldn't happen, instead the exiting channel should get closed
				log.Warn("control channel closed")
				return
			}

			registration := val.Interface().(outChanReg)

			caseToID = append(caseToID, registration.chID)
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: registration.ch,
			})

			c.nextWriter(func(w io.Writer) {
				resp := &response{
					Jsonrpc: "2.0",
					ID:      registration.reqID,
					Result:  registration.chID,
				}

				if err := json.NewEncoder(w).Encode(resp); err != nil {
					log.Error(err)
					return
				}
			})

			continue
		case 1: // exiting channel
			if !ok {
				// exiting channel closed - signals closed connection
				//
				// We're not closing any channels as we're on receiving end.
				// Also, context cancellation below should take care of any running
				// requests
				return
			}
			log.Warn("exiting channel received a message")
			continue
		}

		if !ok {
			// Output channel closed, cleanup, and tell remote that this happened

			id := caseToID[chosen-internal]

			n := len(cases) - 1
			if n > 0 {
				cases[chosen] = cases[n]
				caseToID[chosen-internal] = caseToID[n-internal]
			}

			cases = cases[:n]
			caseToID = caseToID[:n-internal]

			if err := c.sendRequest(request{
				Jsonrpc: "2.0",
				ID:      nil, // notification
				Method:  chClose,
				Params:  []param{{v: reflect.ValueOf(id)}},
			}); err != nil {
				log.Warnf("closed out channel sendRequest failed: %s", err)
			}
			continue
		}

		// forward message
		if err := c.sendRequest(request{
			Jsonrpc: "2.0",
			ID:      nil, // notification
			Method:  chValue,
			Params:  []param{{v: reflect.ValueOf(caseToID[chosen-internal])}, {v: val}},
		}); err != nil {
			log.Warnf("sendRequest failed: %s", err)
			return
		}
	}
}

// handleChanOut registers output channel for forwarding to client
func (c *rpcConnection) handleChanOut(ch reflect.Value, req int64) error {
	c.spawnOutChanHandlerOnce.Do(func() {
		go c.handleOutChans()
	})
	id := atomic.AddUint64(&c.chanCtr, 1)

	select {
	case c.registerCh <- outChanReg{
		reqID: req,

		chID: id,
		ch:   ch,
	}:
		return nil
	case <-c.exiting:
		return xerrors.New("connection closing")
	}
}

//                          //
// Context.Done propagation //
//                          //

// handleCtxAsync handles context lifetimes for client
// TODO: this should be aware of events going through chanHandlers, and quit
//  when the related channel is closed.
//  This should also probably be a single goroutine,
//  Note that not doing this should be fine for now as long as we are using
//  contexts correctly (cancelling when async functions are no longer is use)
func (c *rpcConnection) handleCtxAsync(actx context.Context, id int64) {
	<-actx.Done()

	if err := c.sendRequest(request{
		Jsonrpc: "2.0",
		Method:  wsCancel,
		Params:  []param{{v: reflect.ValueOf(id)}},
	}); err != nil {
		log.Warnw("failed to send request", "method", wsCancel, "id", id, "error", err.Error())
	}
}

// cancelCtx is a built-in rpc which handles context cancellation over rpc
func (c *rpcConnection) cancelCtx(req frame) {
	if req.ID != nil {
		log.Warnf("%s call with ID set, won't respond", wsCancel)
	}

	var id int64
	if err := json.Unmarshal(req.Params[0].data, &id); err != nil {
		log.Error("handle me:", err)
		return
	}

	c.handlingLk.Lock()
	defer c.handlingLk.Unlock()

	cf, ok := c.handling[id]
	if ok {
		cf()
	}
}

//                     //
// Main Handling logic //
//                     //

func (c *rpcConnection) handleChanMessage(frame frame) {
	var chid uint64
	if err := json.Unmarshal(frame.Params[0].data, &chid); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	hnd, ok := c.chanHandlers[chid]
	if !ok {
		log.Errorf("xrpc.ch.val: handler %d not found", chid)
		return
	}

	hnd(frame.Params[1].data, true)
}

func (c *rpcConnection) handleChanClose(frame frame) {
	var chid uint64
	if err := json.Unmarshal(frame.Params[0].data, &chid); err != nil {
		log.Error("failed to unmarshal channel id in xrpc.ch.val: %s", err)
		return
	}

	hnd, ok := c.chanHandlers[chid]
	if !ok {
		log.Errorf("xrpc.ch.val: handler %d not found", chid)
		return
	}

	delete(c.chanHandlers, chid)

	hnd(nil, false)
}

func (c *rpcConnection) handleResponse(frame frame) {
	req, ok := c.inflight[*frame.ID]
	if !ok {
		log.Error("client got unknown ID in response")
		return
	}

	if req.retCh != nil && frame.Result != nil {
		// output is channel
		var chid uint64
		if err := json.Unmarshal(frame.Result, &chid); err != nil {
			log.Errorf("failed to unmarshal channel id response: %s, data '%s'", err, string(frame.Result))
			return
		}

		var chanCtx context.Context
		chanCtx, c.chanHandlers[chid] = req.retCh()
		go c.handleCtxAsync(chanCtx, *frame.ID)
	}

	req.ready <- clientResponse{
		Jsonrpc: frame.Jsonrpc,
		Result:  frame.Result,
		ID:      *frame.ID,
		Error:   frame.Error,
	}
	delete(c.inflight, *frame.ID)
}

func (c *rpcConnection) handleCall(ctx context.Context, frame frame) {
	fmt.Printf("Jim handleCall %v\n", frame)
	if c.handler == nil {
		log.Error("handleCall on client")
		return
	}

	req := request{
		Jsonrpc: frame.Jsonrpc,
		ID:      frame.ID,
		Meta:    frame.Meta,
		Method:  frame.Method,
		Params:  frame.Params,
	}

	ctx, cancel := context.WithCancel(ctx)

	nextWriter := func(cb func(io.Writer)) {
		cb(ioutil.Discard)
	}
	done := func(keepCtx bool) {
		if !keepCtx {
			cancel()
		}
	}
	if frame.ID != nil {
		fmt.Println("Jim x1")
		nextWriter = c.nextWriter
		fmt.Println("Jim x2", nextWriter)

		c.handlingLk.Lock()
		c.handling[*frame.ID] = cancel
		c.handlingLk.Unlock()

		done = func(keepctx bool) {
			c.handlingLk.Lock()
			defer c.handlingLk.Unlock()

			if !keepctx {
				cancel()
				delete(c.handling, *frame.ID)
			}
		}
	}

	go c.handler.handle(ctx, req, nextWriter, rpcError, done, c.handleChanOut)
}

// handleFrame handles all incoming messages (calls and responses)
func (c *rpcConnection) handleFrame(ctx context.Context, frame frame) {
	// Get message type by method name:
	// "" - response
	// "xrpc.*" - builtin
	// anything else - incoming remote call
	fmt.Printf("Jim handleFrame %v\n", frame)
	switch frame.Method {
	case "": // Response to our call
		c.handleResponse(frame)
	case wsCancel:
		c.cancelCtx(frame)
	case chValue:
		c.handleChanMessage(frame)
	case chClose:
		c.handleChanClose(frame)
	default: // Remote call
		c.handleCall(ctx, frame)
	}
}

func (c *rpcConnection) closeInFlight() {
	for id, req := range c.inflight {
		req.ready <- clientResponse{
			Jsonrpc: "2.0",
			ID:      id,
			Error: &respError{
				Message: "handler: websocket connection closed",
				Code:    2,
			},
		}
	}

	c.handlingLk.Lock()
	for _, cancel := range c.handling {
		cancel()
	}
	c.handlingLk.Unlock()

	c.inflight = map[int64]clientRequest{}
	c.handling = map[int64]context.CancelFunc{}
}

func (c *rpcConnection) closeChans() {
	for chid := range c.chanHandlers {
		hnd := c.chanHandlers[chid]
		delete(c.chanHandlers, chid)
		hnd(nil, false)
	}
}
