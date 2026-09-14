package eventbus

import (
	"context"
	"errors"
	"testing"
)

func TestPublishContinuesAfterHandlerError(t *testing.T) {
	bus := New()
	called := 0
	if _, err := bus.Subscribe("demo", func(context.Context, Event) error {
		called++
		return errors.New("第一个订阅者失败")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe("demo", func(context.Context, Event) error {
		called++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	errs := bus.Publish(context.Background(), Event{Name: "demo"})
	if called != 2 || len(errs) != 1 {
		t.Fatalf("called=%d errors=%d", called, len(errs))
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := New()
	called := 0
	cancel, err := bus.Subscribe("demo", func(context.Context, Event) error {
		called++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	bus.Publish(context.Background(), Event{Name: "demo"})
	if called != 0 {
		t.Fatalf("取消订阅后仍调用了 %d 次", called)
	}
}
