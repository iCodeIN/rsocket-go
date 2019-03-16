package rsocket

import (
	"github.com/rsocket/rsocket-go/framing"
	"github.com/rsocket/rsocket-go/transport"
	"sync"
)

// ServerBuilder can be used to build a RSocket server.
type ServerBuilder interface {
	// Acceptor register server acceptor which is used to handle incoming RSockets.
	Acceptor(acceptor ServerAcceptor) ServerTransportBuilder
}

// ServerTransportBuilder is used to build a RSocket server with custom Transport string.
type ServerTransportBuilder interface {
	// Transport specifiy transport string.
	Transport(transport string) Start
}

// Start start a RSocket server.
type Start interface {
	// Serve serve RSocket server.
	Serve() error
}

type xServer struct {
	addr      string
	acc       ServerAcceptor
	scheduler Scheduler
	responses *sync.Map // sid -> flux/mono
}

func (p *xServer) Acceptor(acceptor ServerAcceptor) ServerTransportBuilder {
	p.acc = acceptor
	return p
}

func (p *xServer) Transport(transport string) Start {
	p.addr = transport
	return p
}

func (p *xServer) Serve() error {
	defer func() {
		_ = p.scheduler.Close()
	}()
	t, err := transport.NewTCPServerTransport(p.addr)
	if err != nil {
		return err
	}
	t.Accept(func(setup *framing.FrameSetup, tp transport.Transport) error {
		defer setup.Release()
		sendingSocket := newDuplexRSocket(tp, true, p.scheduler)
		sendingSocket.bindResponder(p.acc(setup, sendingSocket))
		return nil
	})
	return t.Listen()
}

// Receive receives server connections from client RSockets.
func Receive() ServerBuilder {
	return &xServer{
		responses: &sync.Map{},
		scheduler: NewElasticScheduler(1000),
	}
}
