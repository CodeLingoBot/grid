package grid

import (
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats"
)

const (
	BuffSize = 8000
)

// Conn is a named bi-directional channel connected to Nats.
// It request messages on its name. Anyone that queue
// subscribes to its name will start getting pushish
// requests.
type Conn interface {
	ReceiveC() <-chan interface{}
	Send(receiver string, m interface{}) error
	Close()
	Published() <-chan bool
	Size() int
}

func newDataSet() []interface{} {
	return make([]interface{}, 0, BuffSize/2)
}

type conn struct {
	ec        *nats.EncodedConn
	name      string
	exit      chan bool
	intput    chan interface{}
	outputs   map[string]chan interface{}
	stoponce  *sync.Once
	published chan bool
}

func NewConn(name string, ec *nats.EncodedConn) Conn {
	c := &conn{
		ec:        ec,
		name:      name,
		exit:      make(chan bool),
		intput:    make(chan interface{}),
		outputs:   make(map[string]chan interface{}),
		stoponce:  new(sync.Once),
		published: make(chan bool),
	}
	log.Printf("%v: connected", name)
	go func() {
		for {
			ds := newDataSet()
			err := c.ec.Request(c.name, c.name, &ds, 1*time.Second)
			if err != nil {
				if err.Error() != "nats: Timeout" {
					select {
					case c.intput <- err:
					case <-c.exit:
						return
					}
				}
			} else {
				for _, d := range ds {
					select {
					case c.intput <- d:
					case <-c.exit:
						return
					}
				}
			}
		}
	}()

	return c
}

// ReceiveC is the channel of inputs for this Conn.
func (c *conn) ReceiveC() <-chan interface{} {
	return c.intput
}

// Send a message to the receiver.
func (c *conn) Send(receiver string, m interface{}) error {
	out, ok := c.outputs[receiver]
	if !ok {
		out = make(chan interface{}, BuffSize)
		c.outputs[receiver] = out
		log.Printf("%v: subscribing: %v", c.name, receiver)
		sub, err := c.ec.QueueSubscribe(receiver, receiver, func(m *nats.Msg) {
			tik := time.NewTicker(50 * time.Millisecond)
			defer tik.Stop()
			ds := newDataSet()
			eof := false
			for !eof {
				select {
				case <-tik.C:
					eof = true
				case d := <-out:
					ds = append(ds, d)
					if len(ds) >= BuffSize/3 {
						eof = true
					}
				}
			}
			err := c.ec.Publish(m.Reply, ds)
			if err != nil {
				log.Printf("to: %v, actor: %v, failed to send: %v", m.Reply, receiver, err)
			}
			select {
			case c.published <- true:
			default:
			}
		})
		if err != nil {
			return err
		}
		go func() {
			<-c.exit
			sub.Unsubscribe()
		}()
	}
	out <- m
	return nil
}

// Published sends a 'true' every time messages are picked up from
// output queues of this Conn.
func (c *conn) Published() <-chan bool {
	return c.published
}

// Size of all output queues of this Conn summed up.
func (c *conn) Size() int {
	size := 0
	for _, q := range c.outputs {
		size += len(q)
	}
	return size
}

// Close.
func (c *conn) Close() {
	c.stoponce.Do(func() {
		close(c.exit)
	})
}
