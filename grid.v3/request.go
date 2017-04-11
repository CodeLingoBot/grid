package grid

import (
	"context"
	"errors"
	"sync"

	"github.com/lytics/grid/grid.v3/codec"
	netcontext "golang.org/x/net/context"
)

// Ack is the message sent back when the Ack() method of a
// request is called.
const Ack = "__ACK__"

var (
	ErrAlreadyResponded = errors.New("already responded")
)

// Request which must receive an ack or response.
type Request interface {
	Context() context.Context
	Msg() interface{}
	Ack() error
	Respond(msg interface{}) error
}

// newRequest state for use in the server. This actually converts
// between the "context" and "golang.org/x/net/context" types of
// Context so that method signatures are satisfied.
func newRequest(ctx netcontext.Context, msg interface{}) *request {
	return &request{
		ctx:      context.WithValue(ctx, "", ""),
		msg:      msg,
		failure:  make(chan error, 1),
		response: make(chan []byte, 1),
	}
}

type request struct {
	mu       sync.Mutex
	msg      interface{}
	ctx      context.Context
	failure  chan error
	response chan []byte
	finished bool
}

// Context of request.
func (req *request) Context() context.Context {
	return req.ctx
}

// Msg of the request.
func (req *request) Msg() interface{} {
	return req.msg
}

// Ack request, same as responding with Respond
// and constant "Ack".
func (req *request) Ack() error {
	return req.Respond(Ack)
}

// Respond to request with a message.
func (req *request) Respond(msg interface{}) error {
	req.mu.Lock()
	defer req.mu.Unlock()

	if req.finished {
		return ErrAlreadyResponded
	}
	req.finished = true

	fail, ok := msg.(error)
	if ok {
		select {
		case req.failure <- fail:
			return nil
		default:
			panic("grid: respond called multiple times")
		}
	}

	// Encode the message here, in the thread of
	// execution of the caller.
	cn := codec.Name(msg)
	msgCodec, registed := codec.Registry().GetCodecName(cn)
	if !registed {
		return ErrUnRegisteredMsgType
	}
	b, err := msgCodec.Marshal(msg)
	if err != nil {
		return err
	}

	// Send the response bytes. Again, the bytes need
	// to be generated by the thread of execution of
	// the caller of Respond.
	select {
	case req.response <- b:
		return nil
	default:
		panic("grid: respond called multiple times")
	}
}
