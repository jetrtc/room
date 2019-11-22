package room

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jetrtc/log"
	"golang.org/x/net/websocket"
)

const (
	heartbeatCheck   = 30 * time.Second
	heartbeatTimeout = 60 * time.Second
	bufferLevel      = 64
)

type roomCallback interface {
	OnAdd(addr, rid, uid string)
	OnDel(addr, rid, uid string)
}

type room struct {
	log.Sugar
	id       string
	callback roomCallback
	users    map[string]*roomUser
	regCh    chan *regUser
	getCh    chan *getUser
	addCh    chan *addUserConn
	delCh    chan *delUserConn
	msgCh    chan *sendMsg
	doneCh   chan bool
}

type regUser struct {
	uid  string
	user *roomUser
}

type getUser struct {
	uid   string
	resCh chan *roomUser
}

type addUserConn struct {
	uid  string
	conn *userConn
}

type delUserConn struct {
	uid  string
	conn *userConn
}

type sendMsg struct {
	from, to string
	body     interface{}
}

func newRoom(log log.Sugar, callback roomCallback, id string) *room {
	r := &room{
		Sugar:    log,
		callback: callback,
		id:       id,
		users:    make(map[string]*roomUser),
		regCh:    make(chan *regUser),
		getCh:    make(chan *getUser),
		addCh:    make(chan *addUserConn),
		delCh:    make(chan *delUserConn),
		msgCh:    make(chan *sendMsg),
		doneCh:   make(chan bool)}
	return r
}

func (r *room) register(uid string) {
	user := &roomUser{
		room:  r,
		id:    uid,
		conns: make(map[int]*userConn),
	}
	req := &regUser{uid: uid, user: user}
	r.regCh <- req
}

func (r *room) handler(uid string) http.Handler {
	req := &getUser{uid: uid, resCh: make(chan *roomUser)}
	r.getCh <- req
	user := <-req.resCh
	if user == nil {
		return nil
	}
	return websocket.Handler(user.serve)
}

func (r *room) close() {
	r.doneCh <- true
}

func (r *room) add(uid string, c *userConn) {
	r.addCh <- &addUserConn{uid: uid, conn: c}
}

func (r *room) del(uid string, c *userConn) {
	r.delCh <- &delUserConn{uid: uid, conn: c}
}

func (r *room) send(from, to string, body interface{}) {
	r.msgCh <- &sendMsg{from: from, to: to, body: body}
}

// Listen and serve.
// It serves client connection and broadcast request.
func (r *room) loop() {
	r.Infof("Accepting websocket: %s", r.id)
	for {
		select {
		case req := <-r.regCh:
			r.Infof("Registering user: %s", req.user.id)
			u := r.users[req.uid]
			if u != nil && len(u.conns) > 0 {
				for _, c := range u.conns {
					r.Warningf("Kicking existing conn: %s => %s", req.uid, c.String())
					c.close(nil)
				}
			}
			r.users[req.uid] = req.user
		case req := <-r.getCh:
			req.resCh <- r.users[req.uid]
		case req := <-r.addCh:
			r.Infof("Adding conn of user: %s => %s", req.uid, req.conn.String())
			u := r.users[req.uid]
			u.conns[u.num] = req.conn
			req.conn.seq = u.num
			u.num++
			go r.callback.OnAdd(req.conn.ws.Request().RemoteAddr, r.id, u.id)
		case req := <-r.delCh:
			r.Infof("Deleting conn of user: %s => %s", req.uid, req.conn.String())
			u := r.users[req.uid]
			delete(u.conns, req.conn.seq)
			go r.callback.OnDel(req.conn.ws.Request().RemoteAddr, r.id, u.id)
		case req := <-r.msgCh:
			msg := &eventMessage{}
			msg.Body = &struct {
				Time int64       `json:"time"`
				From string      `json:"from"`
				Body interface{} `json:"body"`
			}{
				Time: time.Now().UnixNano() / 1000000,
				From: req.from,
				Body: req.body,
			}
			if req.to != "" {
				msg.Event = "message"
				delivered := false
				r.Infof("Messaging: %s => %s", req.from, req.to)
				u := r.users[req.to]
				for _, c := range u.conns {
					if c.write(msg) == nil {
						delivered = true
					}
				}
				if !delivered {
					r.Warningf("Message not received: %s => %s", req.from, req.to)
				}
			} else {
				msg.Event = "broadcast"
				r.Infof("Broadcasting from: %s", req.from)
				count := 0
				for uid, u := range r.users {
					if uid == req.from {
						continue
					}
					delivered := false
					for _, c := range u.conns {
						if c.write(msg) == nil {
							delivered = true
						}
					}
					if delivered {
						count++
					}
				}
				if count > 0 {
					r.Infof("Broadcasted to %d user(s)", count)
				} else {
					r.Warningf("Broadcast not received: %s", req.from)
				}
			}
		case <-r.doneCh:
			for _, u := range r.users {
				for k, c := range u.conns {
					c.close(func() {
						delete(u.conns, k)
					})
				}
			}
			r.Infof("Done")
			return
		}
	}
}

type roomUser struct {
	room  *room
	id    string
	num   int
	conns map[int]*userConn
}

func (u *roomUser) serve(ws *websocket.Conn) {
	u.room.Infof("Websocket connected: %s", ws.RemoteAddr().String())
	conn := &userConn{
		Sugar:        u.room,
		user:         u,
		ws:           ws,
		timeout:      make(chan bool),
		msgCh:        make(chan *eventMessage, bufferLevel),
		closeCh:      make(chan bool),
		lastActivity: time.Now(),
	}
	conn.closeCb = func() {
		u.room.del(u.id, conn)
	}
	u.room.add(u.id, conn)
	conn.listen() // blocks here
}

type eventMessage struct {
	Event string      `json:"event"`
	Body  interface{} `json:"body,omitempty"`
}

type userConn struct {
	log.Sugar
	user         *roomUser
	seq          int
	ws           *websocket.Conn
	closeCb      func()
	msgCh        chan *eventMessage
	closeCh      chan bool
	ticker       *time.Ticker
	tickerStop   chan bool
	timeout      chan bool
	lastActivity time.Time
}

func (c *userConn) close(closeCb func()) {
	c.Infof("Closing user conn: %s", c.String())
	c.closeCb = closeCb
	c.onClose()
}

func (c *userConn) String() string {
	return fmt.Sprintf("%s:%s[%d]@%s", c.user.room.id, c.user.id, c.seq, c.ws.Request().RemoteAddr)
}

func (c *userConn) write(msg *eventMessage) error {
	select {
	case c.msgCh <- msg:
		return nil
	default:
		c.Errorf("Buffer overflows %s: %d", c.String(), bufferLevel)
		return fmt.Errorf("Buffer overflows")
	}
}

// Listen Write and Read request via chanel
func (c *userConn) listen() {
	c.ticker = time.NewTicker(heartbeatCheck)
	c.tickerStop = make(chan bool)
	go func() {
		defer c.ticker.Stop()
		for {
			select {
			case <-c.ticker.C:
				if time.Now().Sub(c.lastActivity) > heartbeatTimeout {
					c.timeout <- true
				}
			case <-c.tickerStop:
				return
			}
		}
	}()
	go c.listenRead()
	c.listenWrite()
}

func (c *userConn) onClose() {
	select {
	case c.tickerStop <- true:
	default:
	}
	select {
	case c.closeCh <- true:
	default:
	}
	if c.closeCb != nil {
		c.closeCb()
	}
}

func (c *userConn) listenWrite() {
	defer c.closeRead()
	c.Infof("Awaiting for write web socket: %s", c.String())
	for {
		select {
		case msg := <-c.msgCh:
			err := websocket.JSON.Send(c.ws, msg)
			if err != nil {
				c.Errorf("Failed to send to %s: %s", c.String(), err.Error())
				return
			}
		case <-c.closeCh:
			return
		case <-c.timeout:
			c.Warningf("User conn %s timed out", c.String())
			return
		}
	}
}

func (c *userConn) closeWrite() {
	c.closeCh <- true
}

func (c *userConn) listenRead() {
	defer c.onClose()
	c.Infof("Reading web socket: %s", c.String())
	for {
		var received []byte
		err := websocket.Message.Receive(c.ws, &received)
		if err != nil {
			c.Errorf("Failed to receive from %s: %s", c.String(), err.Error())
			return
		}
		msg := &eventMessage{}
		err = json.Unmarshal(received, msg)
		if err != nil {
			c.Errorf("Failed to unmarshal message from %s: %s", c.String(), err.Error())
			continue
		}
		c.lastActivity = time.Now()
		switch msg.Event {
		case "send":
			msgBody := msg.Body.(map[string]interface{})
			to := msgBody["to"]
			body := msgBody["body"]
			dst := ""
			if to != nil {
				dst = to.(string)
			}
			c.user.room.send(c.user.id, dst, body)
		case "heartbeat":
		default:
			c.Errorf("Unknown event from %s: %s", c.String(), msg.Event)
		}
	}
}

func (c *userConn) closeRead() {
	c.ws.Close()
}
