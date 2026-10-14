package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

var errDispatchUnknown = errors.New("unknown worker dispatch")

type admissionKey struct{}
type launchAdmission struct {
	permit   *admission.Permit
	retained bool
}

func (l *Loop) withAdmission(ctx context.Context, runtimeName string, launch func(context.Context) error) error {
	if l.resourceUnknown != nil {
		return l.resourceUnknown
	}
	if l.deps.Admission == nil || ctx.Value(admissionKey{}) != nil {
		return launch(ctx)
	}
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		runtimeName = l.config.Runtime.Default
	}
	if runtimeName == "" {
		runtimeName = "process"
	}
	if rt, ok := l.deps.Runtimes[runtimeName].(interface{ CapacityAvailable() bool }); ok && !rt.CapacityAvailable() {
		return admission.ErrDeferred
	}
	permit, err := l.deps.Admission.TryAcquire(l.deps.AdmissionContext)
	if err != nil {
		return err
	}
	accepted := &launchAdmission{permit: permit}
	defer func() {
		if !accepted.retained {
			permit.Release()
		}
	}()
	return launch(context.WithValue(ctx, admissionKey{}, accepted))
}

type resourceSession struct {
	loopruntime.SessionHandle
	permit *admission.Permit
	done   chan struct{}
}

func (s *resourceSession) Done() <-chan struct{} { return s.done }

func (l *Loop) retainDispatched(ctx context.Context, session loopruntime.SessionHandle) loopruntime.SessionHandle {
	accepted, _ := ctx.Value(admissionKey{}).(*launchAdmission)
	if accepted == nil {
		return session
	}
	accepted.retained = true
	wrapped := &resourceSession{SessionHandle: session, permit: accepted.permit, done: make(chan struct{})}
	go func() { <-session.Done(); accepted.permit.Release(); close(wrapped.done) }()
	return wrapped
}

func (l *Loop) withReplacement(cook *cookHandle, launch func(context.Context) error) error {
	if l.resourceUnknown != nil {
		return l.resourceUnknown
	}
	ctx := context.Background()
	if l.deps.Admission == nil {
		return launch(ctx)
	}
	original, ok := cook.session.(*resourceSession)
	if !ok {
		return fmt.Errorf("replacement requires original worker resource owner")
	}
	permit, err := original.permit.ReserveReplacement(l.deps.AdmissionContext)
	if err != nil {
		return err
	}
	accepted := &launchAdmission{permit: permit}
	defer func() {
		if !accepted.retained {
			permit.Release()
		}
	}()
	return launch(context.WithValue(ctx, admissionKey{}, accepted))
}

func (l *Loop) retainUnknownDispatch(ctx context.Context, err error) error {
	if accepted, ok := ctx.Value(admissionKey{}).(*launchAdmission); ok {
		accepted.retained = true
	}
	l.resourceUnknown = fmt.Errorf("%w; original owner readback required: %v", errDispatchUnknown, err)
	return l.resourceUnknown
}
