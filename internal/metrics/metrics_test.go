package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRenderExposesBucketsCountSumMaxAndMean(t *testing.T) {
	m := New()
	m.ObserveCall("sendMessage", 120*time.Millisecond)
	m.ObserveCall("sendMessage", 3200*time.Millisecond)
	m.ObserveCall("createForumTopic", 30*time.Millisecond)

	out := m.Render()
	for _, want := range []string{
		// Cumulative buckets: both observations are at or below 5000 ms.
		`bvk_telegram_call_ms_bucket{label="sendMessage",le="5000"} 2`,
		// Only the fast one fits in 250 ms.
		`bvk_telegram_call_ms_bucket{label="sendMessage",le="250"} 1`,
		`bvk_telegram_call_ms_count{label="sendMessage"} 2`,
		`bvk_telegram_call_ms_max{label="sendMessage"} 3200`,
		// Mean of 120 and 3200 is 1660.
		`bvk_telegram_call_ms_mean{label="sendMessage"} 1660`,
		`bvk_telegram_call_ms_sum{label="sendMessage"} 3320`,
		`bvk_telegram_call_ms_max{label="createForumTopic"} 30`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered metrics lack %q\n%s", want, out)
		}
	}
	// A method that never ran must not appear at all.
	if strings.Contains(out, "label=\"deleteMessage\"") {
		t.Fatal("unobserved method was reported")
	}
}

func TestUnsetMetricsAreSafeToUse(t *testing.T) {
	// The workers and the client both call these on a possibly nil receiver, so a
	// test build without metrics wiring must not panic.
	var m *Metrics
	m.ObserveCall("sendMessage", time.Second)
	m.ObserveOperation("create_topic", time.Second)
	m.ObserveMilliseconds("bvk_ratelimit_wait", "chat", time.Second)
	if out := m.Render(); out != "" {
		t.Fatalf("nil metrics rendered output: %q", out)
	}
}

func TestConcurrentRecordingIsRaceFree(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				m.ObserveCall("sendMessage", time.Duration(j)*time.Millisecond)
				m.ObserveOperation("create_topic", time.Duration(j)*time.Second)
				_ = m.Render()
			}
		}(i)
	}
	wg.Wait()
	if out := m.Render(); !strings.Contains(out, `bvk_telegram_call_ms_count{label="sendMessage"} 400`) {
		t.Fatalf("concurrent observations lost: %s", out)
	}
}

func TestOperationBucketsUseSeconds(t *testing.T) {
	m := New()
	m.ObserveOperation("create_topic", 9*time.Second)
	out := m.Render()
	if !strings.Contains(out, `bvk_operation_s_bucket{label="create_topic",le="10"} 1`) {
		t.Fatalf("operation seconds bucket wrong:\n%s", out)
	}
	if !strings.Contains(out, `bvk_operation_s_bucket{label="create_topic",le="8"} 0`) {
		t.Fatalf("operation below-bound bucket wrong:\n%s", out)
	}
}
