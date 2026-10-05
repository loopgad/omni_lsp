package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// HandlerFunc handles a request or notification.
// For requests: return (result, nil) or (nil, error).
// For notifications: return (nil, nil).
type HandlerFunc func(ctx context.Context, msg *Message) (json.RawMessage, error)

// Dispatcher routes incoming messages to registered handlers.
//
// Invariants:
//   - Thread-safe: handlers can be registered concurrently with dispatching.
//   - Unknown methods return MethodNotFound error for requests.
//   - Unknown notifications are silently ignored.
//   - No handler should panic; the dispatcher does not recover.
type Dispatcher struct {
	mu       sync.RWMutex
	handlers map[string]HandlerFunc
}

// NewDispatcher creates a new dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string]HandlerFunc),
	}
}

// Register registers a handler for a method name.
func (d *Dispatcher) Register(method string, handler HandlerFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[method] = handler
}

// Methods 返回已注册的方法名（排序后拷贝）。存在的唯一理由是让注册表与
// protocol.manifest 的方法指纹对账——生产路径不依赖它。
func (d *Dispatcher) Methods() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.handlers))
	for m := range d.handlers {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Dispatch routes a message to the appropriate handler.
//
// F16: Panic boundary — recovers from handler panics to preserve process integrity.
// On recover: classifies as InternalError, returns structured error response.
func (d *Dispatcher) Dispatch(ctx context.Context, msg *Message) (resp *Message) {
	defer func() {
		if r := recover(); r != nil {
			// F16: fail the affected request with a typed error response;
			// never leave the client waiting (exactly one terminal outcome).
			if id := msg.ID; id != nil {
				resp = NewErrorResponse(*id, InternalError,
					fmt.Sprintf("handler panic: %v", r), nil)
				return
			}
			// Notification panic — discard, no response possible.
			resp = nil
		}
	}()
	if msg.IsNotification() {
		return d.dispatchNotification(ctx, msg)
	}
	if msg.IsRequest() {
		return d.dispatchRequest(ctx, msg)
	}
	// Response messages are not dispatched — they should be handled by the caller.
	return nil
}

func (d *Dispatcher) dispatchRequest(ctx context.Context, msg *Message) *Message {
	d.mu.RLock()
	handler, ok := d.handlers[msg.Method]
	d.mu.RUnlock()

	if !ok {
		return NewErrorResponse(*msg.ID, MethodNotFound,
			fmt.Sprintf("method not found: %s", msg.Method), nil)
	}

	result, err := handler(ctx, msg)
	if err != nil {
		if rpcErr, ok := err.(*ResponseError); ok {
			return NewErrorResponse(*msg.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		}
		if ierrors.IsKind(err, ierrors.ErrContentModified) {
			return NewErrorResponse(*msg.ID, ContentModified, err.Error(), nil)
		}
		return NewErrorResponse(*msg.ID, InternalError, err.Error(), nil)
	}

	return NewResponse(*msg.ID, result)
}

func (d *Dispatcher) dispatchNotification(ctx context.Context, msg *Message) *Message {
	d.mu.RLock()
	handler, ok := d.handlers[msg.Method]
	d.mu.RUnlock()

	if !ok {
		// Unknown notifications are silently ignored per JSON-RPC spec.
		return nil
	}

	_, _ = handler(ctx, msg)
	return nil
}

// HasHandler reports whether a handler is registered for the given method.
func (d *Dispatcher) HasHandler(method string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.handlers[method]
	return ok
}
