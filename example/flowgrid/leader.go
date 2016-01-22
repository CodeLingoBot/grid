package main

import (
	"log"
	"strings"
	"time"

	"github.com/lytics/dfa"
	"github.com/lytics/grid/grid2"
	"github.com/lytics/grid/grid2/condition"
)

func NewLeaderActor(def *grid2.ActorDef, conf *Conf) grid2.Actor {
	return &LeaderActor{
		def:  def,
		conf: conf,
		flow: Flow(def.Settings["flow"]),
	}
}

type LeaderActor struct {
	def      *grid2.ActorDef
	conf     *Conf
	flow     Flow
	grid     grid2.Grid
	rx       grid2.Receiver
	exit     <-chan bool
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

func (a *LeaderActor) Act(g grid2.Grid, exit <-chan bool) bool {
	rx, err := grid2.NewReceiver(g.Nats(), a.ID(), 4, 0)
	if err != nil {
		log.Fatalf("%v: error: %v", a.ID(), err)
	}
	defer rx.Close()

	a.rx = rx
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

	d.SetTransition(Running, EverybodyFinished, Finishing, a.Finishing)
	d.SetTransition(Running, Failure, Exiting, a.Exiting)
	d.SetTransition(Running, Exit, Exiting, a.Exiting)

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

func (a *LeaderActor) Starting() dfa.Letter {
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

func (a *LeaderActor) Finishing() dfa.Letter {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	j := condition.NewJoin(a.grid.Etcd(), 10*time.Minute, a.grid.Name(), a.flow.Name(), "finished", a.ID())
	if err := j.Rejoin(); err != nil {
		return Failure
	}
	a.finished = j

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "finished")
	defer w.Stop()

	finished := w.WatchUntil(a.conf.NrConsumers + a.conf.NrProducers + 1)
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

func (a *LeaderActor) Running() dfa.Letter {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	a.state = NewLeaderState()
	s := condition.NewState(a.grid.Etcd(), 10*time.Minute, a.grid.Name(), a.flow.Name(), "state", a.ID())
	defer s.Stop()

	if err := s.Init(a.state); err != nil {
		if _, err := s.Fetch(a.state); err != nil {
			return FetchStateFailure
		}
	}
	log.Printf("%v: running with state: %v, index: %v", a.ID(), a.state, s.Index())

	w := condition.NewCountWatch(a.grid.Etcd(), a.grid.Name(), a.flow.Name(), "finished")
	defer w.Stop()

	finished := w.WatchUntil(a.conf.NrConsumers + a.conf.NrProducers)
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
		case <-finished:
			return EverybodyFinished
		case err := <-w.WatchError():
			log.Printf("%v: error: %v", a, err)
			return Failure
		case m := <-a.rx.Msgs():
			switch m := m.(type) {
			case ResultMsg:
				if strings.Contains(m.From, "producer") {
					a.state.ProducerCounts[m.Producer] = m.Count
					a.state.ProducerDurations[m.Producer] = m.Duration
				}
				if strings.Contains(m.From, "consumer") {
					a.state.ConsumerCounts[m.Producer] += m.Count
				}
			}
		}
	}
}

func (a *LeaderActor) Exiting() {
	if a.started != nil {
		a.started.Stop()
	}
	if a.finished != nil {
		a.finished.Stop()
	}
}

func (a *LeaderActor) Terminating() {
	if a.started != nil {
		a.started.Stop()
	}
	if a.finished != nil {
		a.finished.Stop()
	}

	for p, n := range a.state.ProducerCounts {
		rx := a.state.ConsumerCounts[p]
		delta := a.state.ConsumerCounts[p] - n
		rate := float64(a.state.ProducerCounts[p]) / a.state.ProducerDurations[p]
		log.Printf("%v: producer: %v, sent: %v, consumers received: %v, delta: %v, rate: %2.f/s", a.ID(), p, n, rx, delta, rate)
	}
}

type LeaderState struct {
	Start             time.Time
	ConsumerCounts    map[string]int
	ProducerCounts    map[string]int
	ProducerDurations map[string]float64
}

func NewLeaderState() *LeaderState {
	return &LeaderState{ConsumerCounts: make(map[string]int), ProducerCounts: make(map[string]int), ProducerDurations: make(map[string]float64)}
}
