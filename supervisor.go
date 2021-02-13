package supervisor

// FIXMES in progress:
// 1. Ensure the supervisor actually gets to the terminated state for the
//     unstopped service report.
// 2. Save the unstopped service report in the supervisor.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

const (
	defaultVersion                = "v0.1.0"
	defaultTerminationGracePeriod = 5 * time.Second
	defaultTerminationWaitPeriod  = 0 * time.Second
)

const (
	notRunning = iota
	normal
	paused
	terminated
)

type supervisorID uint32
type serviceID uint32

// ErrSupervisorNotRunning is returned by some methods if the supervisor is
// not running, either because it has not been started or because it has
// been terminated.
var ErrSupervisorNotRunning = errors.New("supervisor not running")

/*
Supervisor is the core type of the module that represents a Supervisor.

Supervisors should be constructed either by New or NewSimple.

Once constructed, a Supervisor should be started in one of three ways:

 1. Calling .Serve(ctx).
 2. Calling .ServeBackground(ctx).
 3. Adding it to an existing Supervisor.

Calling Serve will cause the supervisor to run until the passed-in
context is cancelled. Often one of the last lines of the "main" func for a
program will be to call one of the Serve methods.

Calling ServeBackground will CORRECTLY start the supervisor running in a
new goroutine. It is risky to directly run

  go supervisor.Serve()

because that will briefly create a race condition as it starts up, if you
try to .Add() services immediately afterward.

*/
type Supervisor struct {
	Name    string
	Version string

	services             map[serviceID]serviceWithName
	cancellations        map[serviceID]context.CancelFunc
	servicesShuttingDown map[serviceID]serviceWithName
	lastFail             time.Time
	failures             float64
	restartQueue         []serviceID
	serviceCounter       serviceID
	control              chan supervisorMessage
	notifyServiceDone    chan serviceID
	resumeTimer          <-chan time.Time
	liveness             chan struct{}

	logger             *zap.Logger
	loggerRedirectUndo func()

	// despite the recommendation in the context package to avoid
	// holding this in a struct, I think due to the function of suture
	// and the way it works, I think it's OK in this case. This is the
	// exceptional case, basically.
	ctxMutex sync.Mutex
	ctx      context.Context
	// This function cancels this supervisor specifically.
	ctxCancel func()

	signals chan os.Signal

	terminationGracePeriod time.Duration
	terminationWaitPeriod  time.Duration

	getNow       func() time.Time
	getAfterChan func(time.Duration) <-chan time.Time

	m sync.Mutex

	// The unstopped service report is generated when we finish
	// stopping.
	unstoppedServiceReport UnstoppedServiceReport

	// malign leftovers
	id    supervisorID
	state uint8

	// Spec
	EventHook                EventHook
	FailureDecay             float64
	FailureThreshold         float64
	FailureBackoff           time.Duration
	BackoffJitter            Jitter
	Timeout                  time.Duration
	PassThroughPanics        bool
	DontPropagateTermination bool
}

/*

New is the full constructor function for a supervisor.

The name is a friendly human name for the supervisor, used in logging. Supervisor
does not care if this is unique, but it is good for your sanity if it is.

If not set, the following values are used:

 * EventHook:         A function is created that uses log.Print.
 * FailureDecay:      30 seconds
 * FailureThreshold:  5 failures
 * FailureBackoff:    15 seconds
 * Timeout:           10 seconds
 * BackoffJitter:     DefaultJitter

The EventHook function will be called when errors occur. Suture will log the
following:

 * When a service has failed, with a descriptive message about the
   current backoff status, and whether it was immediately restarted
 * When the supervisor has gone into its backoff mode, and when it
   exits it
 * When a service fails to stop

The failureRate, failureThreshold, and failureBackoff controls how failures
are handled, in order to avoid the supervisor failure case where the
program does nothing but restarting failed services. If you do not
care how failures behave, the default values should be fine for the
vast majority of services, but if you want the details:

The supervisor tracks the number of failures that have occurred, with an
exponential decay on the count. Every FailureDecay seconds, the number of
failures that have occurred is cut in half. (This is done smoothly with an
exponential function.) When a failure occurs, the number of failures
is incremented by one. When the number of failures passes the
FailureThreshold, the entire service waits for FailureBackoff seconds
before attempting any further restarts, at which point it resets its
failure count to zero.

Timeout is how long Suture will wait for a service to properly terminate.

The PassThroughPanics options can be set to let panics in services propagate
and crash the program, should this be desirable.

DontPropagateTermination indicates whether this supervisor tree will
propagate a ErrTerminateTree if a child process returns it. If false,
this supervisor will itself return an error that will terminate its
parent. If true, it will merely return ErrDoNotRestart. false by default.

*/
func New(name, version string, opts ...Option) (*Supervisor, error) {

	s := &Supervisor{
		Name:    name,
		Version: version,

		// services
		services: make(map[serviceID]serviceWithName),
		// cancellations
		cancellations: make(map[serviceID]context.CancelFunc),
		// servicesShuttingDown
		servicesShuttingDown: make(map[serviceID]serviceWithName),
		// lastFail, deliberately the zero time
		lastFail: time.Time{},
		// failures
		failures: 0,
		// restartQueue
		restartQueue: make([]serviceID, 0, 1),
		// serviceCounter
		serviceCounter: 0,
		// control
		control: make(chan supervisorMessage),
		// notifyServiceDone
		notifyServiceDone: make(chan serviceID),
		// resumeTimer
		resumeTimer: make(chan time.Time),

		// liveness
		liveness: make(chan struct{}),

		ctxMutex: sync.Mutex{},
		// ctx
		ctx: nil,
		// myCancel
		ctxCancel: nil,

		terminationGracePeriod: defaultTerminationGracePeriod,
		terminationWaitPeriod:  defaultTerminationWaitPeriod,

		signals: make(chan os.Signal, 5),

		// the tests can override these for testing threshold
		// behavior
		// getNow
		getNow: time.Now,
		// getAfterChan
		getAfterChan: time.After,

		// m
		m: sync.Mutex{},

		// unstoppedServiceReport
		unstoppedServiceReport: nil,

		// id
		id: nextSupervisorID(),
		// state
		state: notRunning,

		EventHook:                nil,
		FailureDecay:             30,
		FailureThreshold:         5,
		FailureBackoff:           time.Second * 15,
		BackoffJitter:            &DefaultJitter{},
		Timeout:                  time.Second * 10,
		PassThroughPanics:        false,
		DontPropagateTermination: false,
	}

	// Apply options
	for _, op := range opts {
		if err := op(s); err != nil {
			return nil, err
		}
	}

	// assign a default logger with development level
	if s.logger == nil {
		if err := WithDevelopmentLogger()(s); err != nil {
			return nil, err
		}
	}

	if s.EventHook == nil {
		s.EventHook = func(e Event) {}
	}

	return s, nil
}

func serviceName(service Service) (serviceName string) {
	stringer, canStringer := service.(fmt.Stringer)
	if canStringer {
		serviceName = stringer.String()
	} else {
		serviceName = fmt.Sprintf("%#v", service)
	}
	return
}

// NewSimple is a convenience function to create a service with just a name
// and the sensible defaults.
func NewSimple(name string, opts ...Option) (*Supervisor, error) {
	return New(name, defaultVersion, opts...)
}

// HasSupervisor is an interface that indicates the given struct contains a
// supervisor. If the struct is either already a *Supervisor, or it embeds
// a *Supervisor, this will already be implemented for you. Otherwise, a
// struct containing a supervisor will need to implement this in order to
// participate in the log function propagation and recursive
// UnstoppedService report.
//
// It is legal for GetSupervisor to return nil, in which case
// the supervisor-specific behaviors will simply be ignored.
type HasSupervisor interface {
	GetSupervisor() *Supervisor
}

func (s *Supervisor) GetSupervisor() *Supervisor {
	return s
}

/*
Add adds a service to this supervisor.

If the supervisor is currently running, the service will be started
immediately. If the supervisor has not been started yet, the service
will be started when the supervisor is. If the supervisor was already stopped,
this is a no-op returning an empty service-token.

The returned ServiceID may be passed to the Remove method of the Supervisor
to terminate the service.

As a special behavior, if the service added is itself a supervisor, the
supervisor being added will copy the EventHook function from the Supervisor it
is being added to. This allows factoring out providing a Supervisor
from its logging. This unconditionally overwrites the child Supervisor's
logging functions.

*/
func (s *Supervisor) Add(service Service) ServiceToken {
	return s.addService("", service)
}

func (s *Supervisor) AddNamed(name string, service Service) ServiceToken {
	return s.addService(name, service)
}

func (s *Supervisor) addService(name string, service Service) ServiceToken {
	if s == nil {
		panic("can't add service to nil *suture.Supervisor")
	}

	if hasSupervisor, isHaveSupervisor := service.(HasSupervisor); isHaveSupervisor {
		supervisor := hasSupervisor.GetSupervisor()
		if supervisor != nil {
			supervisor.EventHook = s.EventHook
		}
	}

	s.m.Lock()
	if s.state == notRunning {
		id := s.serviceCounter
		s.serviceCounter++

		if name == "" {
			name = serviceName(service)
		}

		s.services[id] = serviceWithName{Service: service, name: name}
		s.restartQueue = append(s.restartQueue, id)

		s.m.Unlock()
		return ServiceToken{uint64(s.id)<<32 | uint64(id)}
	}
	s.m.Unlock()

	response := make(chan serviceID)
	if s.sendControl(addService{service: service, name: serviceName(service), response: response}) != nil {
		return ServiceToken{}
	}
	return ServiceToken{id: uint64(s.id)<<32 | uint64(<-response)}
}

// ServeBackground starts running a supervisor in its own goroutine. When
// this method returns, the supervisor is guaranteed to be in a running state.
func (s *Supervisor) ServeBackground(ctx context.Context) {
	go s.Serve(ctx)
	s.sync()
}

/*
Serve starts the supervisor. You should call this on the top-level supervisor,
but nothing else.
*/
func (s *Supervisor) Serve(ctx context.Context) error {

	// context documentation suggests that it is legal for functions to
	// take nil contexts, it's user's responsibility to never pass them in.
	if ctx == nil {
		ctx = context.Background()
	}

	if s == nil {
		panic("Can't serve with a nil *suture.Supervisor")
	}

	if s.id == 0 {
		panic("Can't call Serve on an incorrectly-constructed *suture.Supervisor")
	}

	// Take a separate cancellation function so this tree can be
	// indepedently cancelled.
	ctx, myCancel := context.WithCancel(ctx)
	s.ctxMutex.Lock()
	s.ctx = ctx
	s.ctxMutex.Unlock()
	s.ctxCancel = myCancel

	s.m.Lock()
	if s.state == normal || s.state == paused {
		s.m.Unlock()
		panic("Called .Serve() on a supervisor that is already Serve()ing")
	}

	s.state = normal
	s.m.Unlock()

	defer func() {
		s.m.Lock()
		s.state = terminated
		s.m.Unlock()
	}()

	// for all the services I currently know about, start them
	for _, id := range s.restartQueue {
		namedService, present := s.services[id]
		if present {
			s.runService(ctx, namedService, id)
		}
	}
	s.restartQueue = make([]serviceID, 0, 1)

	signal.Notify(s.signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGABRT)

	for {
		select {
		case sig := <-s.signals:
			s.logger.Warn("os termination signal", zap.String("signal", sig.String()))
			s.shutdown()
			return nil
		case <-ctx.Done():
			s.stopSupervisor()
			return ctx.Err()
		case m := <-s.control:
			switch msg := m.(type) {
			case serviceFailed:
				s.handleFailedService(ctx, msg.id, msg.panicMsg, msg.stacktrace, true)
			case serviceEnded:
				_, monitored := s.services[msg.id]
				if monitored {
					cancel := s.cancellations[msg.id]
					if isErr(msg.err, ErrDoNotRestart) || isErr(msg.err, context.Canceled) || isErr(msg.err, context.DeadlineExceeded) {
						delete(s.services, msg.id)
						delete(s.cancellations, msg.id)
						go cancel()
					} else if isErr(msg.err, ErrTerminateSupervisorTree) {
						s.stopSupervisor()
						if s.DontPropagateTermination {
							return ErrDoNotRestart
						} else {
							return msg.err
						}
					} else {
						s.handleFailedService(ctx, msg.id, msg.err, nil, false)
					}
				}
			case addService:
				id := s.serviceCounter
				s.serviceCounter++

				s.services[id] = serviceWithName{Service: msg.service, name: msg.name}
				s.runService(ctx, s.services[id], id)

				msg.response <- id
			case removeService:
				s.removeService(msg.id, msg.notification)
			case stopSupervisor:
				msg.done <- s.stopSupervisor()
				return nil
			case listServices:
				var services []Service
				for _, service := range s.services {
					services = append(services, service.Service)
				}
				msg.c <- services
			case syncSupervisor:
				// this does nothing on purpose; its sole purpose is to
				// introduce a sync point via the channel receive
			case panicSupervisor:
				// used only by tests
				panic("Panicking as requested!")
			}
		case serviceEnded := <-s.notifyServiceDone:
			delete(s.servicesShuttingDown, serviceEnded)
		case <-s.resumeTimer:
			// We're resuming normal operation after a pause due to
			// excessive thrashing
			// FIXME: Ought to permit some spacing of these functions, rather
			// than simply hammering through them
			s.m.Lock()
			s.state = normal
			s.m.Unlock()
			s.failures = 0
			s.EventHook(EventResume{Supervisor: s, SupervisorName: s.Name, Version: s.Version})
			s.logger.Info("supervisor exiting backoff state.", zap.String("Supervisor", s.Name), zap.String("version", s.Version))
			for _, id := range s.restartQueue {
				namedService, present := s.services[id]
				if present {
					s.runService(ctx, namedService, id)
				}
			}
			s.restartQueue = make([]serviceID, 0, 1)
		}
	}
}

/*
Stop stops the supervisor
*/
func (s *Supervisor) Stop() error {
	s.stopSupervisor()
	return nil
}

// UnstoppedServiceReport will return a report of what services failed to
// stop when the supervisor was stopped. This call will return when the
// supervisor is done shutting down. It will hang on a supervisor that has
// not been stopped, because it will not be "done shutting down".
//
// Calling this on a supervisor will return a report for the whole
// supervisor tree under it.
//
// WARNING: Technically, any use of the returned data structure is a
// TOCTOU violation:
// https://en.wikipedia.org/wiki/Time-of-check_to_time-of-use
// Since the data structure was generated and returned to you, any of these
// services may have stopped since then.
//
// However, this can still be useful information at program teardown
// time. For instance, logging that a service failed to stop as expected is
// still useful, as even if it shuts down later, it was still later than
// you expected.
//
// But if you cast the Service objects back to their underlying objects and
// start trying to manipulate them ("shut down harder!"), be sure to
// account for the possibility they are in fact shut down before you get
// them.
//
// If there are no services to report, the UnstoppedServiceReport will be
// nil. A zero-length constructed slice is never returned.
func (s *Supervisor) UnstoppedServiceReport() (UnstoppedServiceReport, error) {
	// the only thing that ever happens to this channel is getting
	// closed when the supervisor terminates.
	_, _ = <-s.liveness

	// FIXME: Recurse on the supervisors
	return s.unstoppedServiceReport, nil
}

func (s *Supervisor) handleFailedService(ctx context.Context, id serviceID, err interface{}, stacktrace []byte, panic bool) {
	now := s.getNow()

	if s.lastFail.IsZero() {
		s.lastFail = now
		s.failures = 1.0
	} else {
		sinceLastFail := now.Sub(s.lastFail).Seconds()
		intervals := sinceLastFail / s.FailureDecay
		s.failures = s.failures*math.Pow(.5, intervals) + 1
	}

	if s.failures > s.FailureThreshold {
		s.m.Lock()
		s.state = paused
		s.m.Unlock()
		s.EventHook(EventBackoff{Supervisor: s, Version: s.Version, SupervisorName: s.Name})
		s.logger.Info("supervisor entering the backoff state.", zap.String("supervisor", s.Name), zap.String("version", s.Version))
		s.resumeTimer = s.getAfterChan(
			s.BackoffJitter.Jitter(s.FailureBackoff))
	}

	s.lastFail = now

	failedService, monitored := s.services[id]

	// It is possible for a service to be no longer monitored
	// by the time we get here. In that case, just ignore it.
	if monitored {
		s.m.Lock()
		curState := s.state
		s.m.Unlock()
		if curState == normal {
			s.runService(ctx, failedService, id)
		} else {
			s.restartQueue = append(s.restartQueue, id)
		}
		if panic {
			s.EventHook(EventServicePanic{
				Supervisor:       s,
				SupervisorName:   s.Name,
				Service:          failedService.Service,
				ServiceName:      failedService.name,
				CurrentFailures:  s.failures,
				FailureThreshold: s.FailureThreshold,
				Restarting:       curState == normal,
				PanicMsg:         err.(string),
				Stacktrace:       string(stacktrace),
			})
			s.logger.Error("service failed with panic",
				zap.String("supervisor", s.Name),
				zap.String("version", s.Version),
				zap.String("service", failedService.name),
				zap.Float64("currentFailures", s.failures),
				zap.Float64("failureThreshold", s.FailureThreshold),
				zap.Bool("restarting", curState == normal),
				zap.Error(err.(error)),
			)
		} else {
			e := EventServiceTerminate{
				Supervisor:       s,
				SupervisorName:   s.Name,
				Service:          failedService.Service,
				ServiceName:      failedService.name,
				CurrentFailures:  s.failures,
				FailureThreshold: s.FailureThreshold,
				Restarting:       curState == normal,
			}
			if err != nil {
				e.Err = err
			}
			s.EventHook(e)

			s.logger.Error("service failed and terminated",
				zap.String("supervisor", s.Name),
				zap.String("version", s.Version),
				zap.String("service", failedService.name),
				zap.Float64("currentFailures", s.failures),
				zap.Float64("failureThreshold", s.FailureThreshold),
				zap.Bool("restarting", curState == normal),
				zap.Error(err.(error)),
			)
		}
	}
}

func (s *Supervisor) runService(ctx context.Context, svc serviceWithName, id serviceID) {

	s.logger.Info("run service", zap.String("service", svc.name))
	childCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	blockingCancellation := func() {
		cancel()
		<-done
	}
	s.cancellations[id] = blockingCancellation
	go func() {
		if !s.PassThroughPanics {
			defer func() {
				if r := recover(); r != nil {
					buf := make([]byte, 65535)
					written := runtime.Stack(buf, false)
					buf = buf[:written]
					s.fail(id, r.(string), buf)
				}
			}()
		}

		err := svc.Service.Serve(childCtx)
		cancel()
		close(done)

		s.serviceEnded(id, err)
	}()
}

func (s *Supervisor) removeService(id serviceID, notificationChan chan struct{}) {
	namedService, present := s.services[id]
	if present {
		cancel := s.cancellations[id]
		delete(s.services, id)
		delete(s.cancellations, id)

		s.servicesShuttingDown[id] = namedService
		go func() {
			successChan := make(chan struct{})
			go func() {
				cancel()
				close(successChan)
				if notificationChan != nil {
					notificationChan <- struct{}{}
				}
			}()

			select {
			case <-successChan:
				// Life is good!
			case <-s.getAfterChan(s.Timeout):
				s.EventHook(EventStopTimeout{
					Supervisor:     s,
					Version:        s.Version,
					SupervisorName: s.Name,
					Service:        namedService.Service,
					ServiceName:    namedService.name,
				})
				s.logger.Error("Service failed to terminate in a timely manner",
					zap.String("supervisor", s.Name),
					zap.String("version", s.Version),
					zap.String("service", namedService.name),
				)
			}
			s.notifyServiceDone <- id
		}()
	} else {
		if notificationChan != nil {
			notificationChan <- struct{}{}
		}
	}
}

func (s *Supervisor) shutdown() {
	s.logger.Info("stop supervisor", zap.Duration("terminationWaitPeriod", s.terminationWaitPeriod))
	time.Sleep(s.terminationWaitPeriod)
	defer func() {
		<-time.After(s.terminationGracePeriod)
	}()
	_ = s.stopSupervisor()
}

func (s *Supervisor) stopSupervisor() UnstoppedServiceReport {
	notifyDone := make(chan serviceID, len(s.services))

	for id, namedService := range s.services {
		s.logger.Info("stop service process stared", zap.String("service", namedService.name))
		cancel := s.cancellations[id]
		delete(s.cancellations, id)
		s.servicesShuttingDown[id] = namedService
		go func(sID serviceID) {
			_ = s.services[sID].Service.Stop()
			delete(s.services, sID)
			cancel()
			notifyDone <- sID
		}(id)
	}

	timeout := s.getAfterChan(s.Timeout)

SHUTTING_DOWN_SERVICES:
	for len(s.servicesShuttingDown) > 0 {
		select {
		case id := <-notifyDone:
			delete(s.servicesShuttingDown, id)
		case serviceID := <-s.notifyServiceDone:
			delete(s.servicesShuttingDown, serviceID)
		case <-timeout:
			for _, namedService := range s.servicesShuttingDown {
				s.EventHook(EventStopTimeout{
					Supervisor:     s,
					Version:        s.Version,
					SupervisorName: s.Name,
					Service:        namedService.Service,
					ServiceName:    namedService.name,
				})
				s.logger.Error("Service failed to terminate in a timely manner",
					zap.String("supervisor", s.Name),
					zap.String("version", s.Version),
					zap.String("service", namedService.name),
				)
			}

			// failed remove statements will log the errors.
			break SHUTTING_DOWN_SERVICES
		}
	}

	// If nothing else has cancelled our context, we should now.
	s.ctxCancel()

	// Indicate that we're done shutting down
	defer close(s.liveness)

	if len(s.servicesShuttingDown) == 0 {
		return nil
	} else {
		report := UnstoppedServiceReport{}
		for serviceID, serviceWithName := range s.servicesShuttingDown {
			report = append(report, UnstoppedService{
				SupervisorPath: []*Supervisor{s},
				Service:        serviceWithName.Service,
				Name:           serviceWithName.name,
				ServiceToken:   ServiceToken{uint64(s.id)<<32 | uint64(serviceID)},
			})
		}
		s.m.Lock()
		s.unstoppedServiceReport = report
		s.m.Unlock()
		return report
	}
}

// String implements the fmt.Stringer interface.
func (s *Supervisor) String() string {
	return s.Name
}

// sendControl abstracts checking for the supervisor to still be running
// when we send a message. This prevents blocking when sending to a
// cancelled supervisor.
func (s *Supervisor) sendControl(sm supervisorMessage) error {
	var doneChan <-chan struct{}
	s.ctxMutex.Lock()
	if s.ctx == nil {
		s.ctxMutex.Unlock()
		return ErrSupervisorNotStarted
	}
	doneChan = s.ctx.Done()
	s.ctxMutex.Unlock()

	select {
	case s.control <- sm:
		return nil
	case <-doneChan:
		return ErrSupervisorNotRunning
	}
}

/*
Remove will remove the given service from the Supervisor, and attempt to Stop() it.
The ServiceID token comes from the Add() call. This returns without waiting
for the service to stop.
*/
func (s *Supervisor) Remove(id ServiceToken) error {
	sID := supervisorID(id.id >> 32)
	if sID != s.id {
		return ErrWrongSupervisor
	}
	err := s.sendControl(removeService{serviceID(id.id & 0xffffffff), nil})
	if err == ErrSupervisorNotRunning {
		// No meaningful error handling if the supervisor is stopped.
		return nil
	}
	return err
}

/*
RemoveAndWait will remove the given service from the Supervisor and attempt
to Stop() it. It will wait up to the given timeout value for the service to
terminate. A timeout value of 0 means to wait forever.

If a nil error is returned from this function, then the service was
terminated normally. If either the supervisor terminates or the timeout
passes, ErrTimeout is returned. (If this isn't even the right supervisor
ErrWrongSupervisor is returned.)
*/
func (s *Supervisor) RemoveAndWait(id ServiceToken, timeout time.Duration) error {
	sID := supervisorID(id.id >> 32)
	if sID != s.id {
		return ErrWrongSupervisor
	}

	var timeoutC <-chan time.Time

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	notificationC := make(chan struct{})

	sentControlErr := s.sendControl(removeService{id: serviceID(id.id & 0xffffffff), notification: notificationC})

	if sentControlErr != nil {
		return sentControlErr
	}

	select {
	case <-notificationC:
		// normal case; the service is terminated.
		return nil

	// This occurs if the entire supervisor ends without the service
	// having terminated, and includes the timeout the supervisor
	// itself waited before closing the liveness channel.
	case <-s.ctx.Done():
		return ErrTimeout

	// The local timeout.
	case <-timeoutC:
		return ErrTimeout
	}
}

// Logger returns the supervisor logger. will be nil if New failed
func (s *Supervisor) Logger() *zap.Logger {
	return s.logger
}

/*

Services returns a []Service containing a snapshot of the services this
Supervisor is managing.

*/
func (s *Supervisor) Services() []Service {
	ls := listServices{make(chan []Service)}

	if s.sendControl(ls) == nil {
		return <-ls.c
	}
	return nil
}

var currentSupervisorIDL sync.Mutex
var currentSupervisorID uint32

func nextSupervisorID() supervisorID {
	currentSupervisorIDL.Lock()
	defer currentSupervisorIDL.Unlock()
	currentSupervisorID++
	return supervisorID(currentSupervisorID)
}

// ServiceToken is an opaque identifier that can be used to terminate a service that
// has been Add()ed to a Supervisor.
type ServiceToken struct {
	id uint64
}

// An UnstoppedService is the component member of an
// UnstoppedServiceReport.
//
// The SupervisorPath is the path down the supervisor tree to the given
// service.
type UnstoppedService struct {
	SupervisorPath []*Supervisor
	Service        Service
	Name           string
	ServiceToken   ServiceToken
}

// An UnstoppedServiceReport will be returned by StopWithReport, reporting
// which services failed to stop.
type UnstoppedServiceReport []UnstoppedService

type serviceWithName struct {
	Service Service
	name    string
}

// Jitter returns the sum of the input duration and a random jitter.  It is
// compatible with the jitter functions in github.com/lthibault/jitterbug.
type Jitter interface {
	Jitter(time.Duration) time.Duration
}

// NoJitter does not apply any jitter to the input duration
type NoJitter struct{}

// Jitter leaves the input duration d unchanged.
func (NoJitter) Jitter(d time.Duration) time.Duration { return d }

// DefaultJitter is the jitter function that is applied when spec.BackoffJitter
// is set to nil.
type DefaultJitter struct {
	rand *rand.Rand
}

// Jitter will jitter the backoff time by uniformly distributing it into
// the range [FailureBackoff, 1.5 * FailureBackoff).
func (dj *DefaultJitter) Jitter(d time.Duration) time.Duration {
	// this is only called by the core supervisor loop, so it is
	// single-thread safe.
	if dj.rand == nil {
		dj.rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	jitter := dj.rand.Float64() / 2
	return d + time.Duration(float64(d)*jitter)
}

// ErrWrongSupervisor is returned by the (*Supervisor).Remove method
// if you pass a ServiceToken from the wrong Supervisor.
var ErrWrongSupervisor = errors.New("wrong supervisor for this service token, no service removed")

// ErrTimeout is returned when an attempt to RemoveAndWait for a service to
// stop has timed out.
var ErrTimeout = errors.New("waiting for service to stop has timed out")

// ErrSupervisorNotTerminated is returned when asking for a stopped service
// report before the supervisor has been terminated.
var ErrSupervisorNotTerminated = errors.New("supervisor not terminated")

// ErrSupervisorNotStarted is returned if you try to send control messages
// to a supervisor that has not started yet. See note on Supervisor struct
// about the legal ways to start a supervisor.
var ErrSupervisorNotStarted = errors.New("supervisor not started yet")
