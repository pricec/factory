package tasks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pricec/factory/internal/tasks"
)

func TestSettings(t *testing.T) {
	ctx := context.Background()
	_, st, _ := newStore(t)
	if v, ok, err := st.GetSetting(ctx, "k"); err != nil || ok || v != "" {
		t.Fatalf("unset: %q %v %v", v, ok, err)
	}
	if v, err := st.PriorityCriteria(ctx); err != nil || v != "" {
		t.Fatalf("unset criteria: %q %v", v, err)
	}
	for _, v := range []string{"first", "", "second\nline"} {
		if err := st.SetPriorityCriteria(ctx, v); err != nil {
			t.Fatal(err)
		}
		if got, err := st.PriorityCriteria(ctx); err != nil || got != v {
			t.Errorf("criteria = %q %v, want %q", got, err, v)
		}
	}
	if v, ok, _ := st.GetSetting(ctx, tasks.KeyPriorityCriteria); !ok || v != "second\nline" {
		t.Errorf("raw key: %q %v", v, ok)
	}
	if err := st.SetSetting(ctx, "", "x"); !errors.Is(err, tasks.ErrInvalid) {
		t.Errorf("empty key: %v", err)
	}
}
