package main

import (
	"log"
	"time"

	"github.com/lytics/dfa"
	"github.com/lytics/grid/grid2"
	"github.com/lytics/grid/grid2/condition"
	"github.com/lytics/grid/grid2/ring"
)

type ProducerState struct {
	SentMessages int     `json:"sent_messages"`
	Duration     float64 `json:"send_messages"`
}

func NewProducerState() *ProducerState {
	return &ProducerState{SentMessages: 0, Duration: 0}
}

func NewProducerActor(def *grid2.ActorDef, conf *Conf) grid2.Actor {
	return &ProducerActor{
		def:  def,
		conf: conf,
		flow: Flow(def.Settings["flow"]),
	}
}

type ProducerActor struct {
	def      *grid2.ActorDef
	conf     *Conf
	flow     Flow
	grid     grid2.Grid
	tx       grid2.Sender
	exit     <-chan bool
	started  condition.Join
	finished condition.Join
	state    *ProducerState
	chaos    *Chaos
}

func (a *ProducerActor) ID() string {
	return a.def.ID()
}

func (a *ProducerActor) String() string {
	return a.ID()
}

func (a *ProducerActor) Act(g grid2.Grid, exit <-chan bool) bool {
	tx, err := grid2.NewSender(g.Nats(), 100)
	if err != nil {
		log.Fatalf("%v: error: %v", a.ID(), err)
	}
	defer tx.Close()

	a.tx = tx
	a.grid = g
	a.exit = exit
	a.chaos = NewChaos(a.ID())
	defer a.chaos.Stop()

	d := dfa.New()
	d.SetStartState(Starting)
	d.SetTerminalStates(Exiting, Terminating)
	d.SetTransitionLogger(func(state dfa.State) {
		log.Printf("%v: switched to state: %v", a, state)
	})

	d.SetTransition(Starting, EverybodyStarted, Running, a.Running)
	d.SetTransition(Starting, EverybodyFinished, Terminating, a.Terminating)
	d.SetTransition(Starting, Failure, Exiting, a.Exiting)
	d.SetTransition(Starting, Exit, Exiting, a.Exiting)

	d.SetTransition(Running, SendFailure, Resending, a.Resending)
	d.SetTransition(Running, IndividualFinished, Finishing, a.Finishing)
	d.SetTransition(Running, Failure, Exiting, a.Exiting)
	d.SetTransition(Running, Exit, Exiting, a.Exiting)

	d.SetTransition(Resending, SendSuccess, Running, a.Running)
	d.SetTransition(Resending, SendFailure, Resending, a.Resending)
	d.SetTransition(Resending, Failure, Exiting, a.Exiting)
	d.SetTransition(Resending, Exit, Exiting, a.Exiting)

	d.SetTransition(Finishing, EverybodyFinished, Terminating, a.Terminating)
	d.SetTransition(Finishing, Failure, Exiting, a.Exiting)
	d.SetTransition(Finishing, Exit, Exiting, a.Exiting)

	final, err := d.Run(a.Starting)
	if err != nil {
		log.Fatalf("%v: error: %v", a, err)
	}
	if final == Terminating {
		return true
	}
	return false
}

func (a *ProducerActor) Starting() dfa.Letter {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	time.Sleep(3 * time.Second)

	j := condition.NewJoin(a.grid.Etcd(), 2*time.Minute, a.grid.Name(), a.flow.Name(), "started", a.ID())
	if err := j.Rejoin(); err != nil {
		return Failure
	}
	a.started = j

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "started")
	defer w.Stop()

	f := condition.NewNameWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "finished")
	defer f.Stop()

	started := w.WatchUntil(a.conf.NrConsumers + a.conf.NrProducers + 1)
	finished := f.WatchUntil(a.flow.NewContextualName("leader"))
	for {
		select {
		case <-a.exit:
			return Exit
		case <-a.chaos.C:
			return Failure
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				return Failure
			}
		case <-started:
			return EverybodyStarted
		case <-finished:
			return EverybodyFinished
		}
	}
}

func (a *ProducerActor) Finishing() dfa.Letter {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	j := condition.NewJoin(a.grid.Etcd(), 10*time.Minute, a.grid.Name(), a.flow.Name(), "finished", a.ID())
	if err := j.Rejoin(); err != nil {
		return Failure
	}
	a.finished = j

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "finished")
	defer w.Stop()

	finished := w.WatchUntil(a.conf.NrConsumers + a.conf.NrConsumers + 1)
	for {
		select {
		case <-a.exit:
			return Exit
		case <-a.chaos.C:
			return Failure
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				return Failure
			}
			if err := a.finished.Alive(); err != nil {
				return Failure
			}
		case <-finished:
			a.started.Exit()
			a.finished.Alive()
			return EverybodyFinished
		case err := <-w.WatchError():
			log.Printf("%v: error: %v", a, err)
			return Failure
		}
	}
}

func (a *ProducerActor) Running() dfa.Letter {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	a.state = NewProducerState()
	s := condition.NewState(a.grid.Etcd(), 30*time.Minute, a.grid.Name(), a.flow.Name(), "state", a.ID())
	defer s.Stop()

	if err := s.Init(a.state); err != nil {
		if _, err := s.Fetch(a.state); err != nil {
			return FetchStateFailure
		}
	}
	log.Printf("%v: running with state: %v, index: %v", a.ID(), a.state, s.Index())

	// Make some random length string data.
	data := NewDataMaker(a.conf.MsgSize, a.conf.MsgCount-a.state.SentMessages)
	defer data.Stop()

	r := ring.New(a.flow.NewContextualName("consumer"), a.conf.NrConsumers, a.grid)
	start := time.Now()
	for {
		select {
		case <-a.exit:
			if _, err := s.Store(a.state); err != nil {
				log.Printf("%v: failed to save state: %v", a, err)
			}
			return Exit
		case <-a.chaos.C:
			if _, err := s.Store(a.state); err != nil {
				log.Printf("%v: failed to save state: %v", a, err)
			}
			return Failure
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				return Failure
			}
			if _, err := s.Store(a.state); err != nil {
				return Failure
			}
		case <-data.Done():
			if err := a.tx.Flush(); err != nil {
				return SendFailure
			}
			if err := a.tx.Send(a.flow.NewContextualName("leader"), &ResultMsg{Producer: a.ID(), Count: a.state.SentMessages, From: a.ID(), Duration: a.state.Duration}); err != nil {
				return SendFailure
			}
			if _, err := s.Store(a.state); err != nil {
				log.Printf("%v: failed to save state: %v", a, err)
			}
			return IndividualFinished
		case d := <-data.Next():
			if a.state.SentMessages%100 == 0 {
				a.state.Duration += time.Now().Sub(start).Seconds()
				start = time.Now()
			}
			if a.state.SentMessages%10000 == 0 {
				if _, err := s.Store(a.state); err != nil {
					log.Printf("%v: failed to save state: %v", a, err)
					return Failure
				}
			}
			if err := a.tx.SendBuffered(r.ByInt(a.state.SentMessages), &DataMsg{Producer: a.ID(), Data: d}); err != nil {
				if _, err := s.Store(a.state); err != nil {
					log.Printf("%v: failed to save state: %v", a, err)
				}
				return SendFailure
			}
			a.state.SentMessages++
		}
	}
}

func (a *ProducerActor) Resending() dfa.Letter {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	fastticker := time.NewTicker(5 * time.Second)
	defer fastticker.Stop()
	for {
		select {
		case <-a.exit:
			return Exit
		case <-a.chaos.C:
			return Failure
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				log.Printf("%v: failed to report 'started' liveness, but ignoring to flush send buffers", a)
			}
		case <-fastticker.C:
			if err := a.tx.Flush(); err == nil {
				return SendSuccess
			}
		}
	}
}

func (a *ProducerActor) Exiting() {
	if a.started != nil {
		a.started.Stop()
	}
	if a.finished != nil {
		a.finished.Stop()
	}

	if err := a.tx.Flush(); err != nil {
		log.Printf("%v: failed to flush send buffers, trying again", a)
		time.Sleep(5 * time.Second)
		if err := a.tx.Flush(); err != nil {
			log.Printf("%v: failed to flush send buffers, data is being dropped", a)
		}
	}
}

func (a *ProducerActor) Terminating() {
	if a.started != nil {
		a.started.Stop()
	}
	if a.finished != nil {
		a.finished.Stop()
	}
}
