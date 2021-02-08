package supervisor

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	JobLimit = 2
)

type IncrementorJob struct {
	current int
	next    chan int
}

func (i *IncrementorJob) Stop() error {
	return nil
}

func (i *IncrementorJob) Serve(ctx context.Context) error {
	for {
		select {
		case i.next <- i.current + 1:
			i.current++
			if i.current >= JobLimit {
				fmt.Println("Stopping the service")
				return ErrDoNotRestart
			}
		}
	}
}

func TestCompleteJob(t *testing.T) {

	supervisor, err := NewSimple("Supervisor")
	assert.Nil(t, err)
	assert.NotNil(t, supervisor)

	service := &IncrementorJob{0, make(chan int)}
	supervisor.Add(service)

	supervisor.ServeBackground(context.Background())

	fmt.Println("Got:", <-service.next)
	fmt.Println("Got:", <-service.next)

	// Output:
	// Got: 1
	// Got: 2
	// Stopping the service
}
