package supervisor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	Happy = iota
	Fail
	Panic
	Hang
	UseStopChan
	TerminateTree
	DoNotRestart
)

var everMultistarted = false

// Test that supervisors work perfectly when everything is hunky dory.
func TestTheHappyCase(t *testing.T) {
	// t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("A", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)
	if s.String() != "A" {
		t.Fatal("Can't get name from a supervisor")
	}
	service := NewService("B")

	s.Add(service)

	go s.Serve()

	<-service.started

	// If we stop the service, it just gets restarted
	service.take <- Fail
	<-service.started

	// And it is shut down when we stop the supervisor
	service.take <- UseStopChan
	cancel()
	<-service.stop
}

// Test that adding to a running supervisor does indeed start the service.
func TestAddingToRunningSupervisor(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewSimple("A1", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)

	s.ServeBackground()

	service := NewService("B1")
	s.Add(service)

	<-service.started

	services := s.Services()
	if !reflect.DeepEqual([]Service{service}, services) {
		t.Fatal("Can't get list of services as expected.")
	}
}

// Test what happens when services fail.
func TestFailures(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("A2", WithFailureThreshold(3.5), WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)

	go s.Serve()
	defer func() {
		// to avoid deadlocks during shutdown, we have to not try to send
		// things out on channels while we're shutting down (this undoes the
		// LogFailure overide about 25 lines down)
		s.EventHook = func(Event) {}
		cancel()
	}()
	s.sync()

	service1 := NewService("B2")
	service2 := NewService("C2")

	s.Add(service1)
	<-service1.started
	s.Add(service2)
	<-service2.started

	nowFeeder := NewNowFeeder()
	pastVal := time.Unix(1000000, 0)
	nowFeeder.appendTimes(pastVal)
	s.getNow = nowFeeder.getter

	resumeChan := make(chan time.Time)
	s.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}

	failNotify := make(chan bool)
	// use this to synchronize on here
	s.EventHook = func(e Event) {
		switch e.Type() {
		case EventTypeServiceTerminate:
			failNotify <- e.(EventServiceTerminate).Restarting
		case EventTypeServicePanic:
			failNotify <- e.(EventServicePanic).Restarting
		}
	}

	// All that setup was for this: Service1, please return now.
	service1.take <- Fail
	restarted := <-failNotify
	<-service1.started

	if !restarted || s.failures != 1 || s.lastFail != pastVal {
		t.Fatal("Did not fail in the expected manner")
	}
	// Getting past this means the service was restarted.
	service1.take <- Happy

	// Service2, your turn.
	service2.take <- Fail
	nowFeeder.appendTimes(pastVal)
	restarted = <-failNotify
	<-service2.started
	if !restarted || s.failures != 2 || s.lastFail != pastVal {
		t.Fatal("Did not fail in the expected manner")
	}
	// And you're back. (That is, the correct service was restarted.)
	service2.take <- Happy

	// Now, one failureDecay later, is everything working correctly?
	oneDecayLater := time.Unix(1000030, 0)
	nowFeeder.appendTimes(oneDecayLater)
	service2.take <- Fail
	restarted = <-failNotify
	<-service2.started
	// playing a bit fast and loose here with floating point, but...
	// we get 2 by taking the current failure value of 2, decaying it
	// by one interval, which cuts it in half to 1, then adding 1 again,
	// all of which "should" be precise
	if !restarted || s.failures != 2 || s.lastFail != oneDecayLater {
		t.Fatal("Did not decay properly", s.lastFail, oneDecayLater)
	}

	// For a change of pace, service1 would you be so kind as to panic?
	nowFeeder.appendTimes(oneDecayLater)
	service1.take <- Panic
	restarted = <-failNotify
	<-service1.started
	if !restarted || s.failures != 3 || s.lastFail != oneDecayLater {
		t.Fatal("Did not correctly recover from a panic")
	}

	nowFeeder.appendTimes(oneDecayLater)
	backingoff := make(chan bool)
	s.EventHook = func(e Event) {
		switch e.Type() {
		case EventTypeServiceTerminate:
			failNotify <- e.(EventServiceTerminate).Restarting
		case EventTypeBackoff:
			backingoff <- true
		case EventTypeResume:
			backingoff <- false
		}
	}

	// And with this failure, we trigger the backoff code.
	service1.take <- Fail
	backoff := <-backingoff
	restarted = <-failNotify

	if !backoff || restarted || s.failures != 4 {
		t.Fatal("Broke past the threshold but did not log correctly", s.failures, backoff, restarted)
	}
	if service1.existing != 0 {
		t.Fatal("service1 still exists according to itself?")
	}

	// service2 is still running, because we don't shut anything down in a
	// backoff, we just stop restarting.
	service2.take <- Happy

	var correct bool
	timer := time.NewTimer(time.Millisecond * 10)
	// verify the service has not been restarted
	// hard to get around race conditions here without simply using a timer...
	select {
	case service1.take <- Happy:
		correct = false
	case <-timer.C:
		correct = true
	}
	if !correct {
		t.Fatal("Restarted the service during the backoff interval")
	}

	// tell the supervisor the restart interval has passed
	resumeChan <- time.Time{}
	backoff = <-backingoff
	<-service1.started
	s.sync()
	if s.failures != 0 {
		t.Fatal("Did not reset failure count after coming back from timeout.")
	}

	nowFeeder.appendTimes(oneDecayLater)
	service1.take <- Fail
	restarted = <-failNotify
	<-service1.started
	if !restarted || backoff {
		t.Fatal("For some reason, got that we were backing off again.", restarted, backoff)
	}
}

func TestFullConstruction(t *testing.T) {
	// t.Parallel()

	s, err := New("Moo", "v1",
		WithEventHook(func(Event) {}),
		WithFailureDecay(1),
		WithFailureThreshold(2),
		WithFailureBackoff(3),
		WithTimeout(time.Second*29),
	)
	assert.Nil(t, err)
	assert.NotNil(t, s)

	if s.String() != "Moo" || s.FailureDecay != 1 || s.FailureThreshold != 2 || s.FailureBackoff != 3 || s.Timeout != time.Second*29 {
		t.Fatal("Full construction failed somehow")
	}
}

// This is mostly for coverage testing.
func TestDefaultLogging(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("A4", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)

	service := NewService("B4")
	s.Add(service)

	s.FailureThreshold = .5
	s.FailureBackoff = time.Millisecond * 25

	go s.Serve()
	s.sync()

	<-service.started

	resumeChan := make(chan time.Time)
	s.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}

	service.take <- UseStopChan
	service.take <- Fail
	<-service.stop
	resumeChan <- time.Time{}

	<-service.started

	service.take <- Happy

	s.EventHook(EventStopTimeout{s, "v1", s.Name, service, service.name})
	s.EventHook(EventServicePanic{
		SupervisorName:   s.Name,
		ServiceName:      service.name,
		CurrentFailures:  1,
		FailureThreshold: 1,
		Restarting:       true,
		PanicMsg:         "test error",
		Stacktrace:       "",
	})

	cancel()
}

func TestNestedSupervisors(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	super1, err := NewSimple("Top5", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, super1)

	super2, err := NewSimple("Nested5")
	assert.Nil(t, err)
	assert.NotNil(t, super2)

	service := NewService("Service5")

	super2.EventHook = func(e Event) {
		if e.Type() == EventTypeStopTimeout {
			panic("Failed to copy LogBadStop")
		}
	}

	super2.Add(service)

	// test the functions got copied from super1; if this panics, it didn't
	// get copied
	super2.EventHook(EventStopTimeout{
		super2, "v1", super2.Name,
		service, service.name,
	})

	go super1.Serve()
	super1.sync()

	<-service.started
	service.take <- Happy

	cancel()
}

func TestStoppingSupervisorStopsServices(t *testing.T) {
	// t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("Top6", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)

	service := NewService("Service 6")

	s.Add(service)

	go s.Serve()
	s.sync()

	<-service.started

	service.take <- UseStopChan

	cancel()
	<-service.stop

	if s.sendControl(syncSupervisor{}) != ErrSupervisorNotRunning {
		t.Fatal("supervisor is shut down, should be returning ErrSupervisorNotRunning for sendControl")
	}
	if s.Services() != nil {
		t.Fatal("Non-running supervisor is returning services list")
	}
}

// This tests that even if a service is hung, the supervisor will stop.
func TestStoppingStillWorksWithHungServices(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("Top7", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)

	service := NewService("Service WillHang7")

	s.Add(service)

	go s.Serve()

	<-service.started

	service.take <- UseStopChan
	service.take <- Hang

	resumeChan := make(chan time.Time)
	s.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}
	failNotify := make(chan struct{})
	s.EventHook = func(e Event) {
		if e.Type() == EventTypeStopTimeout {
			failNotify <- struct{}{}
		}
	}

	// stop the supervisor, then immediately call time on it
	go cancel()

	resumeChan <- time.Time{}
	<-failNotify
	service.release <- true
	<-service.stop
}

// This tests that even if a service is hung, the supervisor can still
// remove it.
func TestRemovingHungService(t *testing.T) {
	// t.Parallel()

	s, err := NewSimple("TopHungService")
	assert.Nil(t, err)
	assert.NotNil(t, s)

	failNotify := make(chan struct{})
	resumeChan := make(chan time.Time)
	s.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}
	s.EventHook = func(e Event) {
		if e.Type() == EventTypeStopTimeout {
			failNotify <- struct{}{}
		}
	}
	service := NewService("Service WillHang")

	sToken := s.Add(service)

	go s.Serve()

	<-service.started
	service.take <- Hang

	_ = s.Remove(sToken)
	resumeChan <- time.Time{}

	<-failNotify
	service.release <- true
}

func TestRemoveService(t *testing.T) {
	// t.Parallel()

	s, err := NewSimple("Top")
	assert.Nil(t, err)
	assert.NotNil(t, s)

	service := NewService("ServiceToRemove8")

	id := s.Add(service)

	go s.Serve()

	<-service.started
	service.take <- UseStopChan

	err = s.Remove(id)
	if err != nil {
		t.Fatal("Removing service somehow failed")
	}
	<-service.stop

	err = s.Remove(ServiceToken{id.id + (1 << 32)})
	if err != ErrWrongSupervisor {
		t.Fatal("Did not detect that the ServiceToken was wrong")
	}
	err = s.RemoveAndWait(ServiceToken{id.id + (1 << 32)}, time.Second)
	if err != ErrWrongSupervisor {
		t.Fatal("Did not detect that the ServiceToken was wrong")
	}
}

func TestServiceReport(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("Top", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)

	s.Timeout = time.Millisecond
	service := NewService("ServiceName")

	id := s.Add(service)

	go s.Serve()

	<-service.started
	service.take <- Hang

	expected := UnstoppedServiceReport{
		{[]*Supervisor{s}, service, "ServiceName", id},
	}

	cancel()

	report, err := s.UnstoppedServiceReport()
	if err != nil {
		t.Fatalf("error getting unstopped service report: %v", err)
	}
	if !reflect.DeepEqual(report, expected) {
		t.Fatalf("did not get expected stop service report %#v != %#v", report, expected)
	}
}

func TestPassNoContextToSupervisor(t *testing.T) {

	s, err := NewSimple("main")
	assert.Nil(t, err)
	assert.NotNil(t, s)
	service := NewService("B")
	s.Add(service)

	go s.Serve()
	<-service.started

	s.ctxCancel()
}

func TestRemoveAndWait(t *testing.T) {
	// t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("main", WithContext(ctx))
	assert.Nil(t, err)
	assert.NotNil(t, s)
	s.Timeout = time.Second

	s.ServeBackground()

	service := NewService("A1")
	token := s.Add(service)
	<-service.started

	// Normal termination case; without the useStopChan flag on the
	// NewService, this will just terminate. So we can freely use a long
	// timeout, because it should not trigger.
	err = s.RemoveAndWait(token, time.Second)
	if err != nil {
		t.Fatal("Happy case for RemoveAndWait failed: " + err.Error())
	}
	// Removing already-removed service does unblock the channel
	err = s.RemoveAndWait(token, time.Second)
	if err != nil {
		t.Fatal("Removing already-removed service failed: " + err.Error())
	}

	service = NewService("A2")
	token = s.Add(service)
	<-service.started
	service.take <- Hang

	// Abnormal case; the service is hung until we release it
	err = s.RemoveAndWait(token, time.Millisecond)
	if err == nil {
		t.Fatal("RemoveAndWait unexpectedly returning that everything is fine")
	}
	if err != ErrTimeout {
		// laziness; one of the unhappy results is err == nil, which will
		// panic here, but, hey, that's a failing test, right?
		t.Fatal("Unexpected result for RemoveAndWait on frozen service: " +
			err.Error())
	}

	// Abnormal case: The service is hung and we get the supervisor
	// stopping instead.
	service = NewService("A3")
	token = s.Add(service)
	<-service.started
	cancel()
	err = s.RemoveAndWait(token, 10*time.Millisecond)

	if err != ErrSupervisorNotRunning {
		t.Fatal("Unexpected result for RemoveAndWait on a stopped service: " + err.Error())
	}

	// Abnormal case: The service takes long to terminate, which takes more than the timeout of the spec, but
	// if the service eventually terminates, this does not hang RemoveAndWait.
	s, err = NewSimple("main")
	assert.Nil(t, err)
	assert.NotNil(t, s)

	s.Timeout = time.Millisecond
	ctx, cancel = context.WithCancel(context.Background())
	s.ServeBackground()
	defer cancel()
	service = NewService("A1")
	token = s.Add(service)
	<-service.started
	service.take <- Hang

	go func() {
		time.Sleep(10 * time.Millisecond)
		service.release <- true
	}()

	err = s.RemoveAndWait(token, 0)
	if err != nil {
		t.Fatal("Unexpected result of RemoveAndWait: " + err.Error())
	}
}

func TestCoverage(t *testing.T) {
	New("testing coverage", "", WithEventHook(func(Event) {}))
	NoJitter{}.Jitter(time.Millisecond)
}

func TestStopAfterRemoveAndWait(t *testing.T) {
	// t.Parallel()

	var badStopError error

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewSimple("main",
		WithContext(ctx),
		WithTimeout(time.Second),
		WithEventHook(func(e Event) {
			if e.Type() == EventTypeStopTimeout {
				ev := e.(EventStopTimeout)
				badStopError = fmt.Errorf("%s: Service %s failed to terminate in a timely manner", ev.Supervisor, ev.Service)
			}
		}),
	)

	s.ServeBackground()

	service := NewService("A1")
	token := s.Add(service)

	<-service.started
	service.take <- UseStopChan

	err = s.RemoveAndWait(token, time.Second)
	if err != nil {
		t.Fatal("Happy case for RemoveAndWait failed: " + err.Error())
	}
	<-service.stop

	cancel()

	if badStopError != nil {
		t.Fatal("Unexpected timeout while stopping supervisor: " + badStopError.Error())
	}
}

// http://golangtutorials.blogspot.com/2011/10/gotest-unit-testing-and-benchmarking-go.html
// claims test function are run in the same order as the source file...
// I'm not sure if this is part of the contract, though. Especially in the
// face of "t.Parallel()"...
//
// This is also why all the tests must go in this file; this test needs to
// run last, and the only way I know to even hopefully guarantee that is to
// have them all in one file.
func TestEverMultistarted(t *testing.T) {
	if everMultistarted {
		t.Fatal("Seem to have multistarted a service at some point, bummer.")
	}
}

func TestAddAfterStopping(t *testing.T) {
	// t.Parallel()

	s, err := NewSimple("main")
	assert.Nil(t, err)
	assert.NotNil(t, s)

	service := NewService("A1")
	supDone := make(chan struct{})
	addDone := make(chan struct{})

	go func() {
		s.Serve()
		close(supDone)
	}()

	<-supDone

	go func() {
		s.Add(service)
		close(addDone)
	}()

	select {
	case <-time.After(5 * time.Second):
		t.Fatal("Timed out waiting for Add to return")
	case <-addDone:
	}
}

// A test service that can be induced to fail, panic, or hang on demand.
func NewService(name string) *FailableService {
	return &FailableService{name, make(chan bool), make(chan int),
		make(chan bool), make(chan bool, 1), 0, sync.Mutex{}, false}
}

type FailableService struct {
	name     string
	started  chan bool
	take     chan int
	release  chan bool
	stop     chan bool
	existing int

	m       sync.Mutex
	running bool
}

func (s *FailableService) Serve(ctx context.Context) error {
	if s.existing != 0 {
		everMultistarted = true
		panic("Multi-started the same service! " + s.name)
	}
	s.existing++

	s.m.Lock()
	s.running = true
	s.m.Unlock()

	defer func() {
		s.m.Lock()
		s.running = false
		s.m.Unlock()
	}()

	s.started <- true

	useStopChan := false

	for {
		select {
		case val := <-s.take:
			switch val {
			case Happy:
				// Do nothing on purpose. Life is good!
			case Fail:
				s.existing--
				if useStopChan {
					s.stop <- true
				}
				return nil
			case Panic:
				s.existing--
				panic("Panic!")
			case Hang:
				// or more specifically, "hang until I release you"
				<-s.release
			case UseStopChan:
				useStopChan = true
			case TerminateTree:
				return ErrTerminateSupervisorTree
			case DoNotRestart:
				return ErrDoNotRestart
			}
		}
	}
}

func (s *FailableService) String() string {
	return s.name
}

type OldService struct {
	done     chan struct{}
	doReturn chan struct{}
	stopping chan struct{}
	sync     chan struct{}
}

func (os *OldService) Serve() {
	for {
		select {
		case <-os.done:
			return
		case <-os.doReturn:
			return
		case <-os.sync:
			// deliberately do nothing
		}
	}
}

func (os *OldService) Stop() {
	close(os.done)
	os.stopping <- struct{}{}
}

type NowFeeder struct {
	values []time.Time
	getter func() time.Time
	m      sync.Mutex
}

// This is used to test serviceName; it's a service without a Stringer.
type BarelyService struct{}

func (bs *BarelyService) Serve(context context.Context) error {
	return nil
}

func NewNowFeeder() (nf *NowFeeder) {
	nf = new(NowFeeder)
	nf.getter = func() time.Time {
		nf.m.Lock()
		defer nf.m.Unlock()
		if len(nf.values) > 0 {
			ret := nf.values[0]
			nf.values = nf.values[1:]
			return ret
		}
		panic("Ran out of values for NowFeeder")
	}
	return
}

func (nf *NowFeeder) appendTimes(t ...time.Time) {
	nf.m.Lock()
	defer nf.m.Unlock()
	nf.values = append(nf.values, t...)
}

func panics(doesItPanic func(ctx context.Context) error) (panics bool) {
	defer func() {
		if r := recover(); r != nil {
			panics = true
		}
	}()

	doesItPanic(context.Background())

	return
}

func panicsWith(doesItPanic func(context.Context) error, s string) (panics bool) {
	defer func() {
		if r := recover(); r != nil {
			rStr := fmt.Sprintf("%v", r)
			if !strings.Contains(rStr, s) {
				fmt.Println("unexpected:", rStr)
			} else {
				panics = true
			}
		}
	}()

	doesItPanic(context.Background())

	return
}
