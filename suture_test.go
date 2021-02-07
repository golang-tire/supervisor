package supervisor

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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

	s := NewSimple("A")
	if s.String() != "A" {
		t.Fatal("Can't get name from a supervisor")
	}
	service := NewService("B")

	s.Add(service)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)

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

	s := NewSimple("A1")

	ctx, cancel := context.WithCancel(context.Background())
	s.ServeBackground(ctx)
	defer cancel()

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

	s := NewSimple("A2")
	s.spec.FailureThreshold = 3.5

	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	defer func() {
		// to avoid deadlocks during shutdown, we have to not try to send
		// things out on channels while we're shutting down (this undoes the
		// LogFailure overide about 25 lines down)
		s.spec.EventHook = func(Event) {}
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
	s.spec.EventHook = func(e Event) {
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
	s.spec.EventHook = func(e Event) {
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

func TestRunningAlreadyRunning(t *testing.T) {
	// t.Parallel()

	s := NewSimple("A3")
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	defer cancel()

	// ensure the supervisor has made it to its main loop
	s.sync()
	if !panics(s.Serve) {
		t.Fatal("Supervisor failed to prevent itself from double-running.")
	}
}

func TestFullConstruction(t *testing.T) {
	// t.Parallel()

	s := New("Moo", Spec{
		EventHook:        func(Event) {},
		FailureDecay:     1,
		FailureThreshold: 2,
		FailureBackoff:   3,
		Timeout:          time.Second * 29,
	})
	if s.String() != "Moo" || s.spec.FailureDecay != 1 || s.spec.FailureThreshold != 2 || s.spec.FailureBackoff != 3 || s.spec.Timeout != time.Second*29 {
		t.Fatal("Full construction failed somehow")
	}
}

// This is mostly for coverage testing.
func TestDefaultLogging(t *testing.T) {
	// t.Parallel()

	s := NewSimple("A4")

	service := NewService("B4")
	s.Add(service)

	s.spec.FailureThreshold = .5
	s.spec.FailureBackoff = time.Millisecond * 25
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
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

	s.spec.EventHook(EventStopTimeout{s, s.Name, service, service.name})
	s.spec.EventHook(EventServicePanic{
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

	super1 := NewSimple("Top5")
	super2 := NewSimple("Nested5")
	service := NewService("Service5")

	super2.spec.EventHook = func(e Event) {
		if e.Type() == EventTypeStopTimeout {
			panic("Failed to copy LogBadStop")
		}
	}

	super1.Add(super2)
	super2.Add(service)

	// test the functions got copied from super1; if this panics, it didn't
	// get copied
	super2.spec.EventHook(EventStopTimeout{
		super2, super2.Name,
		service, service.name,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go super1.Serve(ctx)
	super1.sync()

	<-service.started
	service.take <- Happy

	cancel()
}

func TestStoppingSupervisorStopsServices(t *testing.T) {
	// t.Parallel()

	s := NewSimple("Top6")
	service := NewService("Service 6")

	s.Add(service)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
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

	s := NewSimple("Top7")
	service := NewService("Service WillHang7")

	s.Add(service)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)

	<-service.started

	service.take <- UseStopChan
	service.take <- Hang

	resumeChan := make(chan time.Time)
	s.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}
	failNotify := make(chan struct{})
	s.spec.EventHook = func(e Event) {
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

	s := NewSimple("TopHungService")
	failNotify := make(chan struct{})
	resumeChan := make(chan time.Time)
	s.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}
	s.spec.EventHook = func(e Event) {
		if e.Type() == EventTypeStopTimeout {
			failNotify <- struct{}{}
		}
	}
	service := NewService("Service WillHang")

	sToken := s.Add(service)

	go s.Serve(context.Background())

	<-service.started
	service.take <- Hang

	_ = s.Remove(sToken)
	resumeChan <- time.Time{}

	<-failNotify
	service.release <- true
}

func TestRemoveService(t *testing.T) {
	// t.Parallel()

	s := NewSimple("Top")
	service := NewService("ServiceToRemove8")

	id := s.Add(service)

	go s.Serve(context.Background())

	<-service.started
	service.take <- UseStopChan

	err := s.Remove(id)
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

	s := NewSimple("Top")
	s.spec.Timeout = time.Millisecond
	service := NewService("ServiceName")

	id := s.Add(service)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)

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

func TestFailureToConstruct(t *testing.T) {
	// t.Parallel()

	var s *Supervisor

	panics(s.Serve)

	s = new(Supervisor)
	panics(s.Serve)
}

func TestFailingSupervisors(t *testing.T) {
	// t.Parallel()

	// This is a bit of a complicated test, so let me explain what
	// all this is doing:
	// 1. Set up a top-level supervisor with a hair-trigger backoff.
	// 2. Add a supervisor to that.
	// 3. To that supervisor, add a service.
	// 4. Panic the supervisor in the middle, sending the top-level into
	//    backoff.
	// 5. Kill the lower level service too.
	// 6. Verify that when the top-level service comes out of backoff,
	//    the service ends up restarted as expected.

	// Ultimately, we can't have more than a best-effort recovery here.
	// A panic'ed supervisor can't really be trusted to have consistent state,
	// and without *that*, we can't trust it to do anything sensible with
	// the children it may have been running. So unlike Erlang, we can't
	// can't really expect to be able to safely restart them or anything.
	// Really, the "correct" answer is that the Supervisor must never panic,
	// but in the event that it does, this verifies that it at least tries
	// to get on with life.

	// This also tests that if a Supervisor itself panics, and one of its
	// monitored services goes down in the meantime, that the monitored
	// service also gets correctly restarted when the supervisor does.

	s1 := NewSimple("Top9")
	s2 := NewSimple("Nested9")
	service := NewService("Service9")

	s1.Add(s2)
	s2.Add(service)

	// start the top-level supervisor...
	ctx, cancel := context.WithCancel(context.Background())
	go s1.Serve(ctx)
	defer cancel()
	// and sync on the service being started.
	<-service.started

	// Set the failure threshold such that even one failure triggers
	// backoff on the top-level supervisor.
	s1.spec.FailureThreshold = .5

	// This lets us control exactly when the top-level supervisor comes
	// back from its backoff, by forcing it to block on this channel
	// being sent something in order to come back.
	resumeChan := make(chan time.Time)
	s1.getAfterChan = func(d time.Duration) <-chan time.Time {
		return resumeChan
	}
	failNotify := make(chan string)
	// synchronize on the expected failure of the middle supervisor
	s1.spec.EventHook = func(e Event) {
		if e.Type() == EventTypeServicePanic {
			failNotify <- fmt.Sprintf("%s", e.(EventServicePanic).Service)
		}
	}

	// Now, the middle supervisor panics and dies.
	s2.panic()

	// Receive the notification from the hacked log message from the
	// top-level supervisor that the middle has failed.
	failing := <-failNotify
	// that's enough sync to guarantee this:
	if failing != "Nested9" || s1.state != paused {
		t.Fatal("Top-level supervisor did not go into backoff as expected")
	}

	// Tell the service to fail. Note the top-level supervisor has
	// still not restarted the middle supervisor.
	service.take <- Fail

	// We now permit the top-level supervisor to resume. It should
	// restart the middle supervisor, which should then restart the
	// child service...
	resumeChan <- time.Time{}

	// which we can pick up from here. If this successfully restarts,
	// then the whole chain must have worked.
	<-service.started
}

func TestNilSupervisorAdd(t *testing.T) {
	// t.Parallel()

	var s *Supervisor

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("did not panic as expected on nil add")
		}
	}()

	s.Add(s)
}

func TestPassNoContextToSupervisor(t *testing.T) {
	s := NewSimple("main")
	service := NewService("B")
	s.Add(service)

	go s.Serve(nil)
	<-service.started

	s.ctxCancel()
}

func TestNilSupervisorPanicsAsExpected(t *testing.T) {
	s := (*Supervisor)(nil)
	if !panicsWith(s.Serve, "with a nil *suture.Supervisor") {
		t.Fatal("nil supervisor doesn't panic as expected")
	}
}

// https://github.com/thejerf/suture/issues/11
//
// The purpose of this test is to verify that it does not cause data races,
// so there are no obvious assertions.
func TestIssue11(t *testing.T) {
	// t.Parallel()

	s := NewSimple("main")
	s.ServeBackground(context.Background())

	subsuper := NewSimple("sub")
	s.Add(subsuper)

	subsuper.Add(NewService("may cause data race"))
}

func TestRemoveAndWait(t *testing.T) {
	// t.Parallel()

	s := NewSimple("main")
	s.spec.Timeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	s.ServeBackground(ctx)

	service := NewService("A1")
	token := s.Add(service)
	<-service.started

	// Normal termination case; without the useStopChan flag on the
	// NewService, this will just terminate. So we can freely use a long
	// timeout, because it should not trigger.
	err := s.RemoveAndWait(token, time.Second)
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
	s = NewSimple("main")
	s.spec.Timeout = time.Millisecond
	ctx, cancel = context.WithCancel(context.Background())
	s.ServeBackground(ctx)
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

func TestSupervisorManagementIssue35(t *testing.T) {
	s := NewSimple("issue 35")

	for i := 1; i < 100; i++ {
		s2 := NewSimple("test")
		s.Add(s2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.ServeBackground(ctx)
	// should not have any panics
	cancel()
}

func TestCoverage(t *testing.T) {
	New("testing coverage", Spec{
		EventHook: func(Event) {},
	})
	NoJitter{}.Jitter(time.Millisecond)
}

func TestStopAfterRemoveAndWait(t *testing.T) {
	// t.Parallel()

	var badStopError error

	s := NewSimple("main")
	s.spec.Timeout = time.Second
	s.spec.EventHook = func(e Event) {
		if e.Type() == EventTypeStopTimeout {
			ev := e.(EventStopTimeout)
			badStopError = fmt.Errorf("%s: Service %s failed to terminate in a timely manner", ev.Supervisor, ev.Service)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.ServeBackground(ctx)

	service := NewService("A1")
	token := s.Add(service)

	<-service.started
	service.take <- UseStopChan

	err := s.RemoveAndWait(token, time.Second)
	if err != nil {
		t.Fatal("Happy case for RemoveAndWait failed: " + err.Error())
	}
	<-service.stop

	cancel()

	if badStopError != nil {
		t.Fatal("Unexpected timeout while stopping supervisor: " + badStopError.Error())
	}
}

// This tests that the entire supervisor tree is terminated when a service
// returns returns ErrTerminateTree directly.
func TestServiceAndTreeTermination(t *testing.T) {
	// t.Parallel()
	s1 := NewSimple("TestTreeTermination1")
	s2 := NewSimple("TestTreeTermination2")
	s1.Add(s2)

	service1 := NewService("TestTreeTerminationService1")
	service2 := NewService("TestTreeTerminationService2")
	service3 := NewService("TestTreeTerminationService2")
	s2.Add(service1)
	s2.Add(service2)
	s2.Add(service3)

	terminated := make(chan struct{})
	go func() {
		// we don't need the context because the service is going
		// to terminate the supervisor.
		s1.Serve(nil)
		terminated <- struct{}{}
	}()

	<-service1.started
	<-service2.started
	<-service3.started

	// OK, everything is up and running. Start by telling one service
	// to terminate itself, and verify it isn't restarted.
	service3.take <- DoNotRestart

	// I've got nothing other than just waiting for a suitable period
	// of time and hoping for the best here; it's hard to synchronize
	// on an event not happening...!
	time.Sleep(250 * time.Microsecond)
	service3.m.Lock()
	service3Running := service3.running
	service3.m.Unlock()

	if service3Running {
		t.Fatal("service3 was restarted")
	}

	service1.take <- TerminateTree
	<-terminated

	if service1.running || service2.running || service3.running {
		t.Fatal("Didn't shut services & tree down properly.")
	}
}

// Test that supervisors set to not propagate service failures upwards will
// not kill the whole tree.
func TestDoNotPropagate(t *testing.T) {
	s1 := NewSimple("TestDoNotPropagate")
	s2 := New("TestDoNotPropgate Subtree", Spec{DontPropagateTermination: true})

	s1.Add(s2)

	service1 := NewService("should keep running")
	service2 := NewService("should end up terminating")
	s1.Add(service1)
	s2.Add(service2)

	ctx, cancel := context.WithCancel(context.Background())
	go s1.Serve(ctx)
	defer cancel()

	<-service1.started
	<-service2.started

	fmt.Println("Service about to take")
	service2.take <- TerminateTree
	fmt.Println("Service took")
	time.Sleep(time.Millisecond)

	if service2.running {
		t.Fatal("service 2 should have terminated")
	}
	if s2.state != terminated {
		t.Fatal("child supervisor should be terminated")
	}
	if s1.state != normal {
		t.Fatal("parent supervisor should be running")
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

	s := NewSimple("main")
	ctx, cancel := context.WithCancel(context.Background())

	service := NewService("A1")
	supDone := make(chan struct{})
	addDone := make(chan struct{})

	go func() {
		s.Serve(ctx)
		close(supDone)
	}()

	cancel()
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
		case <-ctx.Done():
			s.existing--
			if useStopChan {
				s.stop <- true
			}
			return ctx.Err()
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
