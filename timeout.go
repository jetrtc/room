package room

import "time"

type timeout struct {
	timer      *time.Timer
	expire     time.Time
	cancelChan chan bool
}

func newTimeout() *timeout {
	timeout := &timeout{
		cancelChan: make(chan bool),
	}
	return timeout
}

func (t *timeout) start(s *Service, id string, ttl time.Duration) {
	t.timer = time.NewTimer(ttl)
	defer t.timer.Stop()
	t.expire = time.Now().Add(ttl)
	s.Infof("Scheduling timeout: %s => %v", id, t.expire)
	select {
	case <-t.timer.C:
		s.Infof("Channel timed out: %s", id)
		s.delCh <- &delRoom{id}
	case <-t.cancelChan:
		return
	}
}

func (t *timeout) cancel() {
	select {
	case t.cancelChan <- true:
	default:
	}
}
