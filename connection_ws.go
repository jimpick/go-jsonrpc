// +build !js

package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsConn struct {
	rpcConnection

	// outside params
	conn             *websocket.Conn
	connFactory      func() (*websocket.Conn, error)
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration
	pongs            chan struct{}
}

//                         //
// WebSocket Message utils //
//                         //

// nextMessage wait for one message and puts it to the incoming channel
func (c *wsConn) nextMessage() {
	if c.timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			log.Error("setting read deadline", err)
		}
	}
	msgType, r, err := c.conn.NextReader()
	if err != nil {
		c.incomingErr = err
		close(c.incoming)
		return
	}
	if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
		c.incomingErr = errors.New("unsupported message type")
		close(c.incoming)
		return
	}
	c.incoming <- r
}

// nextWriter waits for writeLk and invokes the cb callback with WS message
// writer when the lock is acquired
func (c *wsConn) nextWriter(cb func(io.Writer)) {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	wcl, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		log.Error("handle me:", err)
		return
	}

	cb(wcl)

	if err := wcl.Close(); err != nil {
		log.Error("handle me:", err)
		return
	}
}

func (c *wsConn) sendRequest(req request) error {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	if err := c.conn.WriteJSON(req); err != nil {
		return err
	}
	return nil
}

func (c *wsConn) setupPings() func() {
	if c.pingInterval == 0 {
		return func() {}
	}

	c.conn.SetPongHandler(func(appData string) error {
		select {
		case c.pongs <- struct{}{}:
		default:
		}
		return nil
	})

	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-time.After(c.pingInterval):
				c.writeLk.Lock()
				if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					log.Errorf("sending ping message: %+v", err)
				}
				c.writeLk.Unlock()
			case <-stop:
				return
			}
		}
	}()

	var o sync.Once
	return func() {
		o.Do(func() {
			close(stop)
		})
	}
}

func (c *wsConn) handleWsConn(ctx context.Context) {
	c.incoming = make(chan io.Reader)
	c.inflight = map[int64]clientRequest{}
	c.handling = map[int64]context.CancelFunc{}
	c.chanHandlers = map[uint64]func(m []byte, ok bool){}
	c.pongs = make(chan struct{}, 1)

	c.registerCh = make(chan outChanReg)
	defer close(c.exiting)

	// ////

	// on close, make sure to return from all pending calls, and cancel context
	//  on all calls we handle
	defer c.closeInFlight()
	defer c.closeChans()

	// setup pings

	stopPings := c.setupPings()
	defer stopPings()

	var timeoutTimer *time.Timer
	if c.timeout != 0 {
		timeoutTimer = time.NewTimer(c.timeout)
		defer timeoutTimer.Stop()
	}

	// wait for the first message
	go c.nextMessage()
	for {
		var timeoutCh <-chan time.Time
		if timeoutTimer != nil {
			if !timeoutTimer.Stop() {
				<-timeoutTimer.C
			}
			timeoutTimer.Reset(c.timeout)

			timeoutCh = timeoutTimer.C
		}

		select {
		case r, ok := <-c.incoming:
			if !ok {
				if c.incomingErr != nil {
					if !websocket.IsCloseError(c.incomingErr, websocket.CloseNormalClosure) {
						log.Debugw("websocket error", "error", c.incomingErr)
						// connection dropped unexpectedly, do our best to recover it
						c.closeInFlight()
						c.closeChans()
						c.incoming = make(chan io.Reader) // listen again for responses
						go func() {
							if c.connFactory == nil { // likely the server side, don't try to reconnect
								return
							}

							stopPings()

							attempts := 0
							var conn *websocket.Conn
							for conn == nil {
								time.Sleep(c.reconnectBackoff.next(attempts))
								var err error
								if conn, err = c.connFactory(); err != nil {
									log.Debugw("websocket connection retry failed", "error", err)
								}
								select {
								case <-ctx.Done():
									break
								default:
									continue
								}
								attempts++
							}

							c.writeLk.Lock()
							c.conn = conn
							c.incomingErr = nil

							stopPings = c.setupPings()

							c.writeLk.Unlock()

							go c.nextMessage()
						}()
						continue
					}
				}
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
			go c.nextMessage()
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
		case <-c.pongs:
			if c.timeout > 0 {
				if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
					log.Error("setting read deadline", err)
				}
			}
		case <-timeoutCh:
			if c.pingInterval == 0 {
				// pings not running, this is perfectly normal
				continue
			}

			c.writeLk.Lock()
			if err := c.conn.Close(); err != nil {
				log.Warnw("timed-out websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			log.Errorw("Connection timeout", "remote", c.conn.RemoteAddr())
			return
		case <-c.stop:
			c.writeLk.Lock()
			cmsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			if err := c.conn.WriteMessage(websocket.CloseMessage, cmsg); err != nil {
				log.Warn("failed to write close message: ", err)
			}
			if err := c.conn.Close(); err != nil {
				log.Warnw("websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			return
		}
	}
}
