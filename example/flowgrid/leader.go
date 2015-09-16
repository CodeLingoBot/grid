package main

import (
	"log"
	"strings"
	"time"

	"github.com/lytics/grid"
	"github.com/lytics/grid/condition"
	"github.com/mdmarek/dfa"
)

func NewLeaderActor(def *grid.ActorDef, conf *Conf) grid.Actor {
	return &LeaderActor{
		def:    def,
		conf:   conf,
		flow:   Flow(def.Settings["flow"]),
		events: make(chan dfa.Letter),
	}
}

type LeaderActor struct {
	def      *grid.ActorDef
	conf     *Conf
	flow     Flow
	grid     grid.Grid
	conn     grid.Conn
	exit     <-chan bool
	events   chan dfa.Letter
	started  condition.Join
	finished condition.Join
	state    *LeaderState
	chaos    *Chaos
}

func (a *LeaderActor) ID() string {
	return a.def.ID()
}

func (a *LeaderActor) String() string {
	return a.ID()
}

func (a *LeaderActor) Act(g grid.Grid, exit <-chan bool) bool {
	c, err := grid.NewConn(a.ID(), g.Nats())
	if err != nil {
		log.Fatalf("%v: error: %v", a.ID(), err)
	}
	defer c.Close()
	defer c.Flush()

	a.conn = c
	a.grid = g
	a.exit = exit
	a.chaos = NewChaos(a.ID())
	defer a.chaos.Stop()

	d := dfa.New()
	d.SetStartState(Starting)
	d.SetTerminalStates(Exiting, Terminating)

	d.SetTransition(Starting, EverybodyStarted, Running, a.Running)
	d.SetTransition(Starting, GeneralFailure, Exiting, a.Exiting)
	d.SetTransition(Starting, Exit, Exiting, a.Exiting)

	d.SetTransition(Running, EverybodyFinished, Finishing, a.Finishing)
	d.SetTransition(Running, GeneralFailure, Exiting, a.Exiting)
	d.SetTransition(Running, Exit, Exiting, a.Exiting)

	d.SetTransition(Finishing, EverybodyFinished, Terminating, a.Terminating)
	d.SetTransition(Finishing, GeneralFailure, Exiting, a.Exiting)
	d.SetTransition(Finishing, Exit, Exiting, a.Exiting)

	a.events = make(chan dfa.Letter)
	defer close(a.events)

	err = d.Run(a.events)
	if err != nil {
		log.Fatalf("%v: error: %v", a, err)
	}
	go a.Starting()

	final, err := d.Done()
	if err != nil {
		log.Fatalf("%v: error: %v", a, err)
	}
	if final == Terminating {
		return true
	}
	return false
}

func (a *LeaderActor) Starting() {
	log.Printf("%v: switched to state: %v", a, "Starting")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	j := condition.NewJoin(a.grid.Etcd(), 2*time.Minute, a.grid.Name(), a.flow.Name(), "started", a.ID())
	if err := j.Rejoin(); err != nil {
		a.events <- GeneralFailure
		return
	}
	a.started = j

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "started")
	defer w.Stop()

	started := w.WatchUntil(a.conf.NrConsumers + a.conf.NrProducers + 1)
	for {
		select {
		case <-a.exit:
			a.events <- Exit
			return
		case <-a.chaos.Roll():
			a.events <- GeneralFailure
			return
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				a.events <- GeneralFailure
				return
			}
		case <-started:
			a.events <- EverybodyStarted
			return
		}
	}
	return
}

func (a *LeaderActor) Finishing() {
	log.Printf("%v: switched to state: %v", a, "Finishing")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	j := condition.NewJoin(a.grid.Etcd(), 10*time.Minute, a.grid.Name(), a.flow.Name(), "finished", a.ID())
	if err := j.Rejoin(); err != nil {
		a.events <- GeneralFailure
		return
	}
	a.finished = j

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "finished")
	defer w.Stop()

	finished := w.WatchUntil(a.conf.NrConsumers + a.conf.NrProducers + 1)
	for {
		select {
		case <-a.exit:
			a.events <- Exit
			return
		case <-a.chaos.Roll():
			a.events <- GeneralFailure
			return
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				a.events <- GeneralFailure
				return
			}
			if err := a.finished.Alive(); err != nil {
				a.events <- GeneralFailure
				return
			}
		case <-finished:
			a.started.Exit()
			a.finished.Alive()
			a.events <- EverybodyFinished
			return
		case err := <-w.WatchError():
			log.Printf("%v: error: %v", a, err)
			a.events <- GeneralFailure
			return
		}
	}
	return
}

func (a *LeaderActor) Running() {
	log.Printf("%v: switched to state: %v", a, "Running")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	a.state = NewLeaderState()
	s := condition.NewState(a.grid.Etcd(), 10*time.Minute, a.grid.Name(), a.flow.Name(), "state", a.ID())
	if err := s.Init(a.state); err != nil {
		if _, err := s.Fetch(a.state); err != nil {
			a.events <- FetchStateFailure
			return
		}
	}
	log.Printf("%v: starting with state: %v, index: %v", a.ID(), a.state, s.Index())

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "finished")
	defer w.Stop()

	finished := w.WatchUntil(a.conf.NrConsumers + a.conf.NrProducers)
	for {
		select {
		case <-a.exit:
			if _, err := s.Store(a.state); err != nil {
				log.Printf("%v: failed to save state: %v", a, err)
			}
			a.events <- Exit
			return
		case <-a.chaos.Roll():
			log.Printf("%v: trying to save state", a)
			if _, err := s.Store(a.state); err != nil {
				log.Printf("%v: failed to save state: %v", a, err)
			}
			log.Printf("%v: finished saving state", a)
			a.events <- GeneralFailure
			return
		case <-ticker.C:
			if err := a.started.Alive(); err != nil {
				a.events <- GeneralFailure
				return
			}
		case <-finished:
			a.events <- EverybodyFinished
			return
		case err := <-w.WatchError():
			log.Printf("%v: error: %v", a, err)
			a.events <- GeneralFailure
			return
		case m := <-a.conn.ReceiveC():
			switch m := m.(type) {
			case ResultMsg:
				if strings.Contains(m.From, "producer") {
					a.state.ProducerCounts[m.Producer] = m.Count
				}
				if strings.Contains(m.From, "consumer") {
					a.state.ConsumerCounts[m.Producer] += m.Count
				}
			}
		}
	}
}

func (a *LeaderActor) Exiting() {
	log.Printf("%v: switched to state: %v", a, "Exiting")
}

func (a *LeaderActor) Terminating() {
	log.Printf("%v: switched to state: %v", a, "Terminating")
	for p, n := range a.state.ProducerCounts {
		log.Printf("%v: producer: %v, sent: %v, consumers received: %v, delta: %v", a.ID(), p, n, a.state.ConsumerCounts[p], n-a.state.ConsumerCounts[p])
	}
}

type LeaderState struct {
	Start          time.Time
	ConsumerCounts map[string]int
	ProducerCounts map[string]int
}

func NewLeaderState() *LeaderState {
	return &LeaderState{ConsumerCounts: make(map[string]int), ProducerCounts: make(map[string]int)}
}
