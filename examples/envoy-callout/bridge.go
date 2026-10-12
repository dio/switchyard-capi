package envoycallout

import (
	"context"
	"fmt"
	"time"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// completion is one callout's outcome, translated out of the SDK's types so the
// waiting logic can be exercised without a live Envoy.
type completion struct {
	result shared.HttpCalloutResult
	body   []byte
}

// calloutWaiter implements shared.HttpCalloutCallback and funnels the reply to a
// channel.
//
// Envoy delivers OnHttpCalloutDone on the thread processing the stream, so the
// callback only forwards: it never blocks, and the channel is buffered so a reply
// arriving after a timeout is dropped rather than stalling that thread.
type calloutWaiter struct {
	done chan completion
}

func newCalloutWaiter() *calloutWaiter {
	return &calloutWaiter{done: make(chan completion, 1)}
}

// OnHttpCalloutDone implements shared.HttpCalloutCallback.
func (w *calloutWaiter) OnHttpCalloutDone(
	_ uint64,
	result shared.HttpCalloutResult,
	_ [][2]shared.UnsafeEnvoyBuffer,
	body []shared.UnsafeEnvoyBuffer,
) {
	reply := completion{result: result}
	// Envoy may split the body across chunks, and the buffers are only valid for
	// the duration of this callback: join and copy before it returns.
	for _, chunk := range body {
		reply.body = append(reply.body, chunk.ToBytes()...)
	}
	w.done <- reply
}

// await blocks until the callout completes, the timeout passes, or ctx is done.
func (w *calloutWaiter) await(ctx context.Context, timeout time.Duration) ([]byte, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case reply := <-w.done:
		if reply.result != shared.HttpCalloutSuccess {
			return nil, fmt.Errorf("judge callout failed: %s", calloutResultName(reply.result))
		}
		return reply.body, nil
	case <-timer.C:
		return nil, fmt.Errorf("judge callout timed out after %s", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func calloutResultName(result shared.HttpCalloutResult) string {
	switch result {
	case shared.HttpCalloutSuccess:
		return "success"
	case shared.HttpCalloutReset:
		return "reset"
	case shared.HttpCalloutExceedResponseBufferLimit:
		return "response buffer limit exceeded"
	default:
		return fmt.Sprintf("unknown result %d", result)
	}
}

func initResultName(result shared.HttpCalloutInitResult) string {
	switch result {
	case shared.HttpCalloutInitSuccess:
		return "success"
	case shared.HttpCalloutInitMissingRequiredHeaders:
		return "missing required headers"
	case shared.HttpCalloutInitClusterNotFound:
		return "cluster not found"
	case shared.HttpCalloutInitDuplicateCalloutId:
		return "duplicate callout id"
	case shared.HttpCalloutInitCannotCreateRequest:
		return "cannot create request"
	default:
		return fmt.Sprintf("unknown init result %d", result)
	}
}
