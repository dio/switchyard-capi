package envoycallout

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// fakeHandle stands in for shared.HttpFilterHandle: it records the call and
// drives the callback the way Envoy would.
type fakeHandle struct {
	init      shared.HttpCalloutInitResult
	result    shared.HttpCalloutResult
	reply     []byte
	cluster   string
	headers   [][2]string
	body      []byte
	timeoutMs uint64
}

func (f *fakeHandle) HttpCallout(
	cluster string,
	headers [][2]string,
	body []byte,
	timeoutMs uint64,
	cb shared.HttpCalloutCallback,
) (shared.HttpCalloutInitResult, uint64) {
	f.cluster, f.headers, f.body, f.timeoutMs = cluster, headers, body, timeoutMs
	if f.init == shared.HttpCalloutInitSuccess {
		cb.OnHttpCalloutDone(1, f.result, nil, []shared.UnsafeEnvoyBuffer{buffer(f.reply)})
	}
	return f.init, 1
}

func TestEnvoyCalloutTranslatesTheRequest(t *testing.T) {
	handle := &fakeHandle{init: shared.HttpCalloutInitSuccess, reply: []byte(`{"ok":true}`)}
	callout := NewEnvoyCallout(handle)

	body, err := callout.Do(context.Background(), CalloutRequest{
		Cluster: "judge",
		Headers: [][2]string{{":method", "POST"}},
		Body:    []byte(`{"model":"jev-latest"}`),
		Timeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if handle.cluster != "judge" {
		t.Fatalf("cluster = %q", handle.cluster)
	}
	if handle.timeoutMs != 1500 {
		t.Fatalf("timeoutMs = %d, want 1500", handle.timeoutMs)
	}
	if string(handle.body) != `{"model":"jev-latest"}` {
		t.Fatalf("body sent = %q", handle.body)
	}
}

// A non-success init means no callback will ever fire, so it must be reported
// rather than waited on.
func TestEnvoyCalloutReportsInitFailure(t *testing.T) {
	handle := &fakeHandle{init: shared.HttpCalloutInitClusterNotFound}

	_, err := NewEnvoyCallout(handle).Do(context.Background(), CalloutRequest{
		Cluster: "missing", Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "cluster not found") {
		t.Fatalf("err = %v, want a cluster-not-found failure", err)
	}
}

func TestEnvoyCalloutPropagatesCalloutFailure(t *testing.T) {
	handle := &fakeHandle{
		init:   shared.HttpCalloutInitSuccess,
		result: shared.HttpCalloutReset,
	}

	_, err := NewEnvoyCallout(handle).Do(context.Background(), CalloutRequest{
		Cluster: "judge", Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("err = %v, want a reset failure", err)
	}
}
