package supervisor

import (
	"context"
	"time"
)

// Option defines Supervisor option type.
type Option func(*Supervisor) error

// WithEventHook is an option that sets the event hook
func WithEventHook(eventHook EventHook) Option {
	return func(s *Supervisor) error {
		s.EventHook = eventHook
		return nil
	}
}

// WithFailureDecay is an option that sets the FailureDecay
func WithFailureDecay(failureDecay float64) Option {
	return func(s *Supervisor) error {
		s.FailureDecay = failureDecay
		return nil
	}
}

// WithFailureThreshold is an option that sets the FailureThreshold
func WithFailureThreshold(failureThreshold float64) Option {
	return func(s *Supervisor) error {
		s.FailureThreshold = failureThreshold
		return nil
	}
}

// WithFailureBackoff is an option that sets the FailureBackoff
func WithFailureBackoff(failureBackoff time.Duration) Option {
	return func(s *Supervisor) error {
		s.FailureBackoff = failureBackoff
		return nil
	}
}

// WithBackoffJitter is an option that sets the BackoffJitter
func WithBackoffJitter(backoffJitter Jitter) Option {
	return func(s *Supervisor) error {
		s.BackoffJitter = backoffJitter
		return nil
	}
}

// WithTimeout is an option that sets the Timeout
func WithTimeout(timeout time.Duration) Option {
	return func(s *Supervisor) error {
		s.Timeout = timeout
		return nil
	}
}

// WithPassThroughPanics is an option that sets the PassThroughPanics
func WithPassThroughPanics(passThroughPanics bool) Option {
	return func(s *Supervisor) error {
		s.PassThroughPanics = passThroughPanics
		return nil
	}
}

// WithDontPropagateTermination is an option that sets the DontPropagateTermination
func WithDontPropagateTermination(passThroughPanics bool) Option {
	return func(s *Supervisor) error {
		s.PassThroughPanics = passThroughPanics
		return nil
	}
}

// WithContext is an option that allows you provide a context to use in service
func WithContext(ctx context.Context) Option {
	return func(s *Supervisor) error {
		s.ctx = context.Background()
		return nil
	}
}
