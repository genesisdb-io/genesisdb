package core

import (
	"reflect"
	"testing"
	"time"
)

type recordingObserver struct{ signals []string }

func (o *recordingObserver) EventCommitted(subject, eventType string) {
	o.signals = append(o.signals, "event "+subject+" "+eventType)
}
func (o *recordingObserver) CacheHit()  { o.signals = append(o.signals, "hit") }
func (o *recordingObserver) CacheMiss() { o.signals = append(o.signals, "miss") }
func (o *recordingObserver) CacheSizeChanged(entries int) {
	o.signals = append(o.signals, "size")
}
func (o *recordingObserver) OperationCompleted(operation string, duration time.Duration) {
	o.signals = append(o.signals, "done "+operation)
}
func (o *recordingObserver) ErrorOccurred(operation string) {
	o.signals = append(o.signals, "error "+operation)
}

func TestMultiObserver_ForwardsEverySignalToEveryObserver(t *testing.T) {
	first, second := &recordingObserver{}, &recordingObserver{}
	var observer Observer = MultiObserver{first, NoopObserver{}, second}
	observer.EventCommitted("/a", "t")
	observer.CacheHit()
	observer.CacheMiss()
	observer.CacheSizeChanged(1)
	observer.OperationCompleted("fetch", time.Millisecond)
	observer.ErrorOccurred("commit")
	expected := []string{"event /a t", "hit", "miss", "size", "done fetch", "error commit"}
	for _, recorder := range []*recordingObserver{first, second} {
		if !reflect.DeepEqual(recorder.signals, expected) {
			t.Errorf("signals %v", recorder.signals)
		}
	}
	// An empty MultiObserver is a valid observer.
	MultiObserver(nil).CacheHit()
}
