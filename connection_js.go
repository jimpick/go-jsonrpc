// +build js

package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

type jsConn struct {
	rpcConnection
	sendResponse func(response string)
}

func (c *jsConn) handleJsConn(ctx context.Context) {
	c.incoming = make(chan io.Reader)
	c.inflight = map[int64]clientRequest{}
	c.handling = map[int64]context.CancelFunc{}
	c.chanHandlers = map[uint64]func(m []byte, ok bool){}

	c.registerCh = make(chan outChanReg)
	c.rpcConnection.nextWriter = func(cb func(io.Writer)) {
		c.nextWriter(cb)
	}
	c.rpcConnection.sendRequest = func(req request) error {
		return c.sendRequest(req)
	}

	defer close(c.exiting)

	// ////

	// on close, make sure to return from all pending calls, and cancel context
	//  on all calls we handle
	defer c.closeInFlight()
	defer c.closeChans()

	// wait for the first message
	// go c.nextMessage() // FIXME
	for {
		select {
		case r, ok := <-c.incoming:
			if !ok {
				return // remote closed
			}

			// debug util - dump all messages to stderr
			// r = io.TeeReader(r, os.Stderr)

			var frame frame
			if err := json.NewDecoder(r).Decode(&frame); err != nil {
				log.Error("handle me:", err)
				return
			}

			c.handleFrame(ctx, frame)
			// go c.nextMessage() // FIXME
		case req := <-c.requests:
			c.writeLk.Lock()
			if req.req.ID != nil {
				if c.incomingErr != nil { // No conn?, immediate fail
					req.ready <- clientResponse{
						Jsonrpc: "2.0",
						ID:      *req.req.ID,
						Error: &respError{
							Message: "handler: websocket connection closed",
							Code:    2,
						},
					}
					c.writeLk.Unlock()
					break
				}
				c.inflight[*req.req.ID] = req
			}
			c.writeLk.Unlock()
			if err := c.sendRequest(req.req); err != nil {
				log.Errorf("sendReqest failed (Handle me): %s", err)
			}
		}
	}
}

//                         //
// JS <-> Go Message utils //
//                         //

func (c *jsConn) Write(p []byte) (n int, err error) {
	c.sendResponse(string(p))
	return len(p), nil
}

// nextWriter waits for writeLk and invokes the cb callback with WS message
// writer when the lock is acquired
func (c *jsConn) nextWriter(cb func(io.Writer)) {
	fmt.Println("Jim nextWriter")
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	cb(c)
}

func (c *jsConn) sendRequest(req request) error {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	response, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.sendResponse(string(response))
	/*
		if err := c.conn.WriteJSON(req); err != nil {
			return err
		}
	*/
	return nil
}
