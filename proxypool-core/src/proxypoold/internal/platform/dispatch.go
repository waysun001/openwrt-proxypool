package platform

import (
	"context"

	"proxypoold/internal/model"
)

// ProtocolDispatcher keeps the scheduler protocol-agnostic while refusing to
// guess a fallback adapter for an unregistered protocol.
type ProtocolDispatcher struct {
	adapters map[model.Protocol]NodeAdapter
}

func NewProtocolDispatcher(adapters map[model.Protocol]NodeAdapter) *ProtocolDispatcher {
	copyAdapters := make(map[model.Protocol]NodeAdapter, len(adapters))
	for protocol, adapter := range adapters {
		if adapter != nil {
			copyAdapters[protocol] = adapter
		}
	}
	return &ProtocolDispatcher{adapters: copyAdapters}
}

func (dispatcher *ProtocolDispatcher) Start(ctx context.Context, request NodeRequest) (Session, error) {
	adapter, err := dispatcher.adapter(request.Node.Protocol)
	if err != nil {
		return Session{}, err
	}
	return adapter.Start(ctx, request)
}

func (dispatcher *ProtocolDispatcher) Probe(ctx context.Context, request NodeRequest, session Session) error {
	adapter, err := dispatcher.adapter(request.Node.Protocol)
	if err != nil {
		return err
	}
	return adapter.Probe(ctx, request, session)
}

func (dispatcher *ProtocolDispatcher) Stop(ctx context.Context, request NodeRequest, session Session) error {
	adapter, err := dispatcher.adapter(request.Node.Protocol)
	if err != nil {
		return err
	}
	return adapter.Stop(ctx, request, session)
}

func (dispatcher *ProtocolDispatcher) adapter(protocol model.Protocol) (NodeAdapter, error) {
	if dispatcher != nil {
		if adapter := dispatcher.adapters[protocol]; adapter != nil {
			return adapter, nil
		}
	}
	return nil, &model.CodeError{Code: "unsupported", Message: "protocol adapter is unavailable"}
}

var _ NodeAdapter = (*ProtocolDispatcher)(nil)
