package rsocket

import (
	"context"
	"errors"
	"fmt"
	"github.com/rsocket/rsocket-go/common"
	"github.com/rsocket/rsocket-go/framing"
	"github.com/rsocket/rsocket-go/logger"
	"github.com/rsocket/rsocket-go/transport"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var (
	unsupportedRequestStream   = []byte("Request-Stream not implemented.")
	unsupportedRequestResponse = []byte("Request-Response not implemented.")
	unsupportedRequestChannel  = []byte("Request-Channel not implemented.")
)

type duplexRSocket struct {
	responder       RSocket
	tp              transport.Transport
	serverMode      bool
	requestStreamID uint32
	messages        *sync.Map // sid -> rx
	scheduler       Scheduler
}

func (p *duplexRSocket) releaseAll() {
	disposables := make([]Disposable, 0)
	p.messages.Range(func(key, value interface{}) bool {
		disposables = append(disposables, value.(Disposable))
		return true
	})
	for _, value := range disposables {
		value.Dispose()
	}
}

func (p *duplexRSocket) Close() error {
	return p.tp.Close()
}

func (p *duplexRSocket) FireAndForget(payload Payload) {
	_ = p.tp.Send(framing.NewFrameFNF(p.nextStreamID(), payload.Data(), payload.Metadata()))
}

func (p *duplexRSocket) MetadataPush(payload Payload) {
	_ = p.tp.Send(framing.NewFrameMetadataPush(payload.Metadata()))
}

func (p *duplexRSocket) RequestResponse(payload Payload) Mono {
	sid := p.nextStreamID()
	resp := NewMono(nil)
	resp.DoAfterSuccess(func(ctx context.Context, item Payload) {
		item.Release()
	})
	p.setPublisher(sid, resp)
	resp.
		DoFinally(func(ctx context.Context, sig SignalType) {
			if sig == SignalCancel {
				_ = p.tp.Send(framing.NewFrameCancel(sid))
			}
			p.unsetPublisher(sid)
		})

	p.scheduler.Do(context.Background(), func(ctx context.Context) {
		defer payload.Release()
		if err := p.tp.Send(framing.NewFrameRequestResponse(sid, payload.Data(), payload.Metadata())); err != nil {
			resp.(MonoProducer).Error(err)
		}
	})
	return resp
}

func (p *duplexRSocket) RequestStream(payload Payload) Flux {
	sid := p.nextStreamID()
	flux := NewFlux(nil)
	p.setPublisher(sid, flux)

	merge := struct {
		sid uint32
		tp  transport.Transport
	}{sid, p.tp}

	flux.
		DoAfterNext(func(ctx context.Context, payload Payload) {
			payload.Release()
		}).
		DoFinally(func(ctx context.Context, sig SignalType) {
			p.unsetPublisher(sid)
			if sig == SignalCancel {
				_ = merge.tp.Send(framing.NewFrameCancel(merge.sid))
			}
		}).
		DoOnRequest(func(ctx context.Context, n int) {
			_ = merge.tp.Send(framing.NewFrameRequestN(merge.sid, uint32(n)))
		}).
		DoOnSubscribe(func(ctx context.Context, s Subscription) {
			defer payload.Release()
			if err := merge.tp.Send(framing.NewFrameRequestStream(merge.sid, uint32(s.n()), payload.Data(), payload.Metadata())); err != nil {
				flux.(Producer).Error(err)
			}
		})
	return flux
}

func (p *duplexRSocket) RequestChannel(payloads Publisher) Flux {
	sid := p.nextStreamID()
	inputs := payloads.(Flux)
	fx := NewFlux(nil)
	p.messages.Store(sid, fx)
	fx.DoFinally(func(ctx context.Context, sig SignalType) {
		p.messages.Delete(sid)
	})

	var idx uint32
	merge := struct {
		tp  transport.Transport
		sid uint32
		i   *uint32
	}{p.tp, sid, &idx}

	inputs.
		DoFinally(func(ctx context.Context, sig SignalType) {
			_ = merge.tp.Send(framing.NewFramePayload(merge.sid, nil, nil, framing.FlagComplete))
		}).
		SubscribeOn(p.scheduler).
		Subscribe(context.Background(), OnNext(func(ctx context.Context, sub Subscription, item Payload) {
			defer item.Release()
			// TODO: request N
			if atomic.AddUint32(merge.i, 1) == 1 {
				_ = merge.tp.Send(framing.NewFrameRequestChannel(merge.sid, math.MaxUint32, item.Data(), item.Metadata(), framing.FlagNext))
			} else {
				_ = merge.tp.Send(framing.NewFramePayload(merge.sid, item.Data(), item.Metadata(), framing.FlagNext))
			}
		}))

	return fx
}

func (p *duplexRSocket) respondRequestResponse(input framing.Frame) error {
	// 0. do some convert jobs
	f := input.(*framing.FrameRequestResponse)
	sid := f.Header().StreamID()
	// 1. execute socket handler
	send, err := func() (mono Mono, err error) {
		defer func() {
			err = toError(recover())
		}()
		mono = p.responder.RequestResponse(f)
		return
	}()
	// 2. send error with panic
	if err != nil {
		_ = p.writeError(sid, err)
		return nil
	}
	// 3. send error with unsupported handler
	if send == nil {
		_ = p.writeError(sid, framing.NewFrameError(sid, common.ErrorCodeApplicationError, unsupportedRequestResponse))
		return nil
	}
	// 4. register publisher
	p.setPublisher(sid, send)
	// 5. async subscribe publisher
	send.
		DoFinally(func(ctx context.Context, sig SignalType) {
			p.unsetPublisher(sid)
			f.Release()
		}).
		DoOnError(func(ctx context.Context, err error) {
			_ = p.writeError(sid, err)
		}).
		SubscribeOn(p.scheduler).
		Subscribe(context.Background(), OnNext(func(ctx context.Context, sub Subscription, item Payload) {
			v, ok := item.(*framing.FrameRequestResponse)
			if !ok || v != f {
				_ = p.tp.Send(framing.NewFramePayload(sid, item.Data(), item.Metadata(), framing.FlagNext|framing.FlagComplete))
				return
			}
			// reuse request frame, reduce copy
			fg := framing.FlagNext | framing.FlagComplete
			if len(v.Metadata()) > 0 {
				fg |= framing.FlagMetadata
			}
			send := &framing.FramePayload{
				BaseFrame: framing.NewBaseFrame(framing.NewFrameHeader(sid, framing.FrameTypePayload, fg), v.Body()),
			}
			v.SetBody(nil)
			_ = p.tp.Send(send)
		}))
	return nil
}

func (p *duplexRSocket) respondRequestChannel(input framing.Frame) error {
	f := input.(*framing.FrameRequestChannel)
	sid := f.Header().StreamID()
	inputs := NewFlux(nil)
	outputs, err := func() (flux Flux, err error) {
		defer func() {
			err = toError(recover())
		}()
		flux = p.responder.RequestChannel(inputs.(Flux))
		if flux == nil {
			err = framing.NewFrameError(sid, common.ErrorCodeApplicationError, unsupportedRequestChannel)
		}
		return
	}()
	if err != nil {
		return p.writeError(sid, err)
	}

	p.setPublisher(sid, inputs)
	initialRequestN := f.InitialRequestN()
	inputs.(Producer).Next(f)
	// TODO: process send error
	_ = p.tp.Send(framing.NewFrameRequestN(sid, initialRequestN))

	if inputs != outputs {
		// auto release frame for each consumer
		inputs.DoAfterNext(func(ctx context.Context, item Payload) {
			item.Release()
		})
	}

	outputs.
		DoFinally(func(ctx context.Context, sig SignalType) {
			p.unsetPublisher(sid)
		}).
		DoOnError(func(ctx context.Context, err error) {
			_ = p.writeError(sid, err)
		}).
		DoOnComplete(func(ctx context.Context) {
			_ = p.tp.Send(framing.NewFramePayload(sid, nil, nil, framing.FlagComplete))
		}).
		SubscribeOn(p.scheduler).
		Subscribe(context.Background(), p.toSender(sid, framing.FlagNext))
	return nil
}

func (p *duplexRSocket) respondMetadataPush(input framing.Frame) error {
	p.scheduler.Do(context.Background(), func(ctx context.Context) {
		defer input.Release()
		defer func() {
			if e := recover(); e != nil {
				logger.Errorf("respond metadata push failed: %s\n", e)
			}
		}()
		p.responder.MetadataPush(input.(*framing.FrameMetadataPush))
	})
	return nil
}

func (p *duplexRSocket) respondFNF(input framing.Frame) error {
	p.scheduler.Do(context.Background(), func(ctx context.Context) {
		defer input.Release()
		defer func() {
			if e := recover(); e != nil {
				logger.Errorf("respond FireAndForget failed: %s\n", e)
			}
		}()
		p.responder.FireAndForget(input.(*framing.FrameFNF))
	})
	return nil
}

func (p *duplexRSocket) respondRequestStream(input framing.Frame) error {
	f := input.(*framing.FrameRequestStream)
	sid := f.Header().StreamID()

	// 1. execute request stream handler
	resp, err := func() (resp Flux, err error) {
		defer func() {
			err = toError(recover())
		}()
		resp = p.responder.RequestStream(f)
		if resp == nil {
			err = framing.NewFrameError(sid, common.ErrorCodeApplicationError, unsupportedRequestStream)
		}
		return
	}()

	// 2. send error with panic
	if err != nil {
		return p.writeError(sid, err)
	}

	// 3. register publisher
	p.setPublisher(sid, resp)

	// 4. async subscribe publisher
	resp.
		DoFinally(func(ctx context.Context, sig SignalType) {
			f.Release()
			p.unsetPublisher(sid)
		}).
		DoOnComplete(func(ctx context.Context) {
			_ = p.tp.Send(framing.NewFramePayload(sid, nil, nil, framing.FlagComplete))
		}).
		DoOnError(func(ctx context.Context, err error) {
			_ = p.writeError(sid, err)
		}).
		SubscribeOn(p.scheduler).
		Subscribe(context.Background(), OnSubscribe(func(ctx context.Context, s Subscription) {
			s.Request(int(f.InitialRequestN()))
		}), p.toSender(sid, framing.FlagNext))
	return nil
}

func (p *duplexRSocket) writeError(sid uint32, err error) error {
	v, ok := err.(*framing.FrameError)
	if ok {
		return p.tp.Send(v)
	}
	return p.tp.Send(framing.NewFrameError(sid, common.ErrorCodeApplicationError, []byte(err.Error())))
}

func (p *duplexRSocket) bindResponder(socket RSocket) {
	p.responder = socket
	p.tp.HandleRequestResponse(p.respondRequestResponse)
	p.tp.HandleMetadataPush(p.respondMetadataPush)
	p.tp.HandleFNF(p.respondFNF)
	p.tp.HandleRequestStream(p.respondRequestStream)
	p.tp.HandleRequestChannel(p.respondRequestChannel)
}

func (p *duplexRSocket) start() {
	p.tp.HandleCancel(func(frame framing.Frame) (err error) {
		logger.Warnf("incoming cancel frame: %s\n", frame)
		defer frame.Release()
		sid := frame.Header().StreamID()
		v, ok := p.messages.Load(sid)
		if !ok {
			return fmt.Errorf("invalid stream id: %d", sid)
		}
		v.(Disposable).Dispose()
		return nil
	})
	p.tp.HandleError(func(input framing.Frame) (err error) {
		f := input.(*framing.FrameError)
		logger.Errorf("handle error frame: %s\n", f)
		v, ok := p.messages.Load(f.Header().StreamID())
		if !ok {
			return fmt.Errorf("invalid stream id: %d", f.Header().StreamID())
		}
		switch foo := v.(type) {
		case Mono:
			foo.DoFinally(func(ctx context.Context, sig SignalType) {
				f.Release()
			})
		case Flux:
			foo.DoFinally(func(ctx context.Context, sig SignalType) {
				f.Release()
			})
		}
		switch foo := v.(type) {
		case MonoProducer:
			foo.Error(f)
		case Producer:
			foo.Error(f)
		}
		return
	})

	p.tp.HandleRequestN(func(input framing.Frame) (err error) {
		f := input.(*framing.FrameRequestN)
		defer f.Release()
		sid := f.Header().StreamID()
		found, ok := p.messages.Load(sid)
		if !ok {
			return fmt.Errorf("non-exists stream id: %d", sid)
		}
		flux, ok := found.(Flux)
		if !ok {
			return fmt.Errorf("unsupport request n: streamId=%d", sid)
		}
		flux.(Subscription).Request(int(f.N()))
		return nil
	})

	p.tp.HandlePayload(func(input framing.Frame) (err error) {
		f := input.(*framing.FramePayload)
		sid := f.Header().StreamID()
		v, ok := p.messages.Load(sid)
		if !ok {
			return fmt.Errorf("non-exist stream id: %d", sid)
		}
		switch vv := v.(type) {
		case MonoProducer:
			vv.Success(f)
		case Producer:
			if f.Header().Flag().Check(framing.FlagNext) {
				vv.Next(f)
			} else if f.Header().Flag().Check(framing.FlagComplete) {
				vv.Complete()
			}
		}
		return
	})
}

func (p *duplexRSocket) toSender(sid uint32, fg framing.FrameFlag) OptSubscribe {
	merge := struct {
		tp  transport.Transport
		sid uint32
		fg  framing.FrameFlag
	}{p.tp, sid, fg}
	return OnNext(func(ctx context.Context, sub Subscription, payload Payload) {
		switch v := payload.(type) {
		case *framing.FramePayload:
			if v.Header().Flag().Check(framing.FlagMetadata) {
				h := framing.NewFrameHeader(merge.sid, framing.FrameTypePayload, merge.fg|framing.FlagMetadata)
				v.SetHeader(h)
			} else {
				h := framing.NewFrameHeader(merge.sid, framing.FrameTypePayload, merge.fg)
				v.SetHeader(h)
			}
			_ = merge.tp.Send(v)
		default:
			defer payload.Release()
			_ = merge.tp.Send(framing.NewFramePayload(merge.sid, payload.Data(), payload.Metadata(), merge.fg))
		}
	})
}

func (p *duplexRSocket) nextStreamID() uint32 {
	if p.serverMode {
		// 2,4,6,8...
		return 2 * atomic.AddUint32(&p.requestStreamID, 1)
	}
	// 1,3,5,7
	return 2*(atomic.AddUint32(&p.requestStreamID, 1)-1) + 1
}

func (p *duplexRSocket) setPublisher(sid uint32, pub Publisher) {
	p.messages.Store(sid, pub)
}

func (p *duplexRSocket) unsetPublisher(sid uint32) {
	p.messages.Delete(sid)
}

func newDuplexRSocket(tp transport.Transport, serverMode bool, scheduler Scheduler) *duplexRSocket {
	sk := &duplexRSocket{
		tp:         tp,
		serverMode: serverMode,
		messages:   &sync.Map{},
		scheduler:  scheduler,
	}
	tp.OnClose(func() {
		sk.releaseAll()
	})

	if logger.IsDebugEnabled() {
		done := make(chan struct{})
		tk := time.NewTicker(10 * time.Second)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-tk.C:
					var ccc int
					sk.messages.Range(func(key, value interface{}) bool {
						ccc++
						return true
					})
					if ccc > 0 {
						logger.Debugf("[LEAK] messages count: %d\n", ccc)
					}
				}
			}
		}()
		tp.OnClose(func() {
			tk.Stop()
			close(done)
		})
	}
	defer sk.start()
	return sk
}

// toError try convert something to error
func toError(err interface{}) error {
	if err == nil {
		return nil
	}
	switch v := err.(type) {
	case error:
		return v
	case string:
		return errors.New(v)
	default:
		return fmt.Errorf("%s", v)
	}
}
