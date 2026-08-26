package server

import (
	"errors"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/languages"
)

// statusBackend implements StatusReporter to verify §Q2 error projection.
type statusBackend struct {
	mockBackend
	msg string
}

func (b *statusBackend) BackendStatusMessage() string { return b.msg }

func TestQ2_ErrorProjectionCarriesSupervisorGuidance(t *testing.T) {
	t.Run("backend with supervisor message enriches text", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &statusBackend{mockBackend{langID: "go"}, "Backend unhealthy, restarting (crash #2, epoch 3). Recovery in progress."})
		err := ierrors.New(ierrors.ErrBackendUnavailable, "go", "pyright process terminated")
		got := projectBackendError(s, err)
		want := err.Error() + " [Backend unhealthy, restarting (crash #2, epoch 3). Recovery in progress.]"
		if got != want {
			t.Errorf("projection = %q, want %q", got, want)
		}
	})

	t.Run("unknown backend or plain error passes through", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &statusBackend{mockBackend{langID: "go"}, "ready"})
		if got := projectBackendError(s, errors.New("scheduler queue full")); got != "scheduler queue full" {
			t.Errorf("plain error mangled: %q", got)
		}
		plain := ierrors.New(ierrors.ErrTimeout, "", "timed out")
		if got := projectBackendError(s, plain); got != plain.Error() {
			t.Errorf("op-less error mangled: %q", got)
		}
	})

	t.Run("backend without StatusReporter stays bare", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go"})
		err := ierrors.New(ierrors.ErrBackendUnavailable, "go", "terminated")
		if got := projectBackendError(s, err); got != err.Error() {
			t.Errorf("bare backend projection = %q", got)
		}
	})
}

var _ languages.StatusReporter = (*statusBackend)(nil)
