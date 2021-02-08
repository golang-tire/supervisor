package main

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-tire/supervisor"
)

type SampleService struct {
	stop chan bool
}

func (s *SampleService) Serve(ctx context.Context) error {
	for {
		select {
		case <-time.After(time.Second * 2):
			fmt.Println("sample service tick")
		case <-s.stop:
			fmt.Println("Stopping the service with stop command")
			return nil
		case <-ctx.Done():
			fmt.Println("Stopping the service")
			s.stop <- true
			return nil
		}
	}
}

func (s *SampleService) Stop() error {
	s.stop <- true
	return nil
}

func main() {

	s, err := supervisor.New("SupV1", "v1")
	if err != nil {
		panic(err)
	}

	sampleSvc := &SampleService{stop: make(chan bool)}
	s.AddNamed("sample", sampleSvc)

	err = s.Serve(context.Background())
	if err != nil {
		panic(err)
	}
}
