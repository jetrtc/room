package room

import (
	"net/http"
	"time"

	"github.com/jetrtc/log"
)

func NewService(log log.Sugar) *Service {
	return &Service{
		Sugar:      log,
		rooms:      make(map[string]*room),
		timeouts:   make(map[string]*timeout),
		addCh:      make(chan *addRoomAndUser),
		getCh:      make(chan *getRoom),
		delCh:      make(chan *delRoom),
		shutdownCh: make(chan *shutdown),
	}
}

type Service struct {
	log.Sugar
	rooms      map[string]*room
	timeouts   map[string]*timeout
	addCh      chan *addRoomAndUser
	getCh      chan *getRoom
	delCh      chan *delRoom
	shutdownCh chan *shutdown
}

type addRoomAndUser struct {
	id  string
	ttl time.Duration
	uid string
}

type getRoom struct {
	id    string
	resCh chan *room
}

type delRoom struct {
	id string
}

type shutdown struct {
	resCh chan bool
}

func (s *Service) Create(cid string, ttl time.Duration, uid string) error {
	s.addCh <- &addRoomAndUser{id: cid, ttl: ttl, uid: uid}
	return nil
}

func (s *Service) Handler(cid string, uid string) http.Handler {
	resCh := make(chan *room, 1)
	s.getCh <- &getRoom{id: cid, resCh: resCh}
	rm := <-resCh
	if rm == nil {
		return nil
	}
	return rm.handler(uid)
}

func (s *Service) Shtudown() {
	resCh := make(chan bool, 1)
	s.shutdownCh <- &shutdown{resCh: resCh}
	<-resCh
}

func (s *Service) OnAdd(addr, cid, uid string) {
}

func (s *Service) OnDel(addr, cid, uid string) {
}

func (s *Service) Loop() {
	for {
		select {
		case req := <-s.addCh:
			r := s.rooms[req.id]
			if r == nil {
				s.Infof("Creating room: %s => %v", req.id, req.ttl)
				r = newRoom(s, s, req.id)
				s.rooms[req.id] = r
				go r.loop()
			}
			r.register(req.uid)
			timeout := s.timeouts[req.id]
			if timeout != nil {
				s.Infof("Canceling existing timeout: %s => %v", req.id, timeout.expire)
				timeout.cancel()
				delete(s.timeouts, req.id)
			}
			timeout = newTimeout()
			go timeout.start(s, req.id, req.ttl)
			s.timeouts[req.id] = timeout
		case req := <-s.getCh:
			r := s.rooms[req.id]
			req.resCh <- r
		case req := <-s.delCh:
			timeout := s.timeouts[req.id]
			if timeout != nil {
				timeout.cancel()
				delete(s.timeouts, req.id)
			}
			r := s.rooms[req.id]
			if r != nil {
				s.Infof("Closing room: %s", req.id)
				r.close()
				delete(s.rooms, req.id)
			}
		case req := <-s.shutdownCh:
			s.Infof("Closing rooms...")
			for id, r := range s.rooms {
				timeout := s.timeouts[id]
				if timeout != nil {
					timeout.cancel()
					delete(s.timeouts, id)
				}
				s.Debugf("Closing room: %s", id)
				r.close()
			}
			req.resCh <- true
			return
		}
	}
}
