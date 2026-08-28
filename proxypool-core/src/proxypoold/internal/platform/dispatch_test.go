package platform

import (
	"context"
	"errors"
	"testing"

	"proxypoold/internal/model"
)

func TestProtocolDispatcherRoutesEachRequestToItsExactAdapter(t *testing.T) {
	l2tp := dispatchTestAdapter{session: Session{Protocol: model.ProtocolL2TP, Interface: "l2tp-ppv20001"}}
	socks := dispatchTestAdapter{session: Session{Protocol: model.ProtocolSOCKS5, Interface: "proxypool-socks5", LocalPort: 12002}}
	dispatcher := NewProtocolDispatcher(map[model.Protocol]NodeAdapter{
		model.ProtocolL2TP:   l2tp,
		model.ProtocolSOCKS5: socks,
	})

	l2tpSession, err := dispatcher.Start(context.Background(), dispatchRequest(model.ProtocolL2TP))
	if err != nil || l2tpSession.Interface != "l2tp-ppv20001" || l2tpSession.Protocol != model.ProtocolL2TP {
		t.Fatalf("L2TP dispatch = %#v, %v", l2tpSession, err)
	}
	socksSession, err := dispatcher.Start(context.Background(), dispatchRequest(model.ProtocolSOCKS5))
	if err != nil || socksSession.LocalPort != 12002 || socksSession.Protocol != model.ProtocolSOCKS5 {
		t.Fatalf("SOCKS5 dispatch = %#v, %v", socksSession, err)
	}
}

func TestProtocolDispatcherRejectsUnregisteredProtocolForEveryLifecycleMethod(t *testing.T) {
	dispatcher := NewProtocolDispatcher(map[model.Protocol]NodeAdapter{
		model.ProtocolL2TP: dispatchTestAdapter{},
	})
	request := dispatchRequest(model.ProtocolSLP)
	session := Session{NodeID: request.Node.ID, Generation: request.Generation, Protocol: model.ProtocolSLP}

	if _, err := dispatcher.Start(context.Background(), request); !isUnsupportedDispatch(err) {
		t.Fatalf("Start error = %v", err)
	}
	if err := dispatcher.Probe(context.Background(), request, session); !isUnsupportedDispatch(err) {
		t.Fatalf("Probe error = %v", err)
	}
	if err := dispatcher.Stop(context.Background(), request, session); !isUnsupportedDispatch(err) {
		t.Fatalf("Stop error = %v", err)
	}
}

func isUnsupportedDispatch(err error) bool {
	var coded *model.CodeError
	return errors.As(err, &coded) && coded.Code == "unsupported"
}

func dispatchRequest(protocol model.Protocol) NodeRequest {
	return NodeRequest{
		Node:  model.Node{ID: "node_a", Protocol: protocol, PolicyID: 1, Revision: 2},
		JobID: "job_a", Generation: 3,
	}
}

type dispatchTestAdapter struct{ session Session }

func (adapter dispatchTestAdapter) Start(_ context.Context, request NodeRequest) (Session, error) {
	session := adapter.session
	session.NodeID = request.Node.ID
	session.Generation = request.Generation
	return session, nil
}

func (dispatchTestAdapter) Probe(context.Context, NodeRequest, Session) error { return nil }
func (dispatchTestAdapter) Stop(context.Context, NodeRequest, Session) error  { return nil }
