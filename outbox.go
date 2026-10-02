package environment

import (
	"fmt"
	"sync"

	"github.com/avanha/pmaas-common/queue"
)

// outbox hands closures from one actor to another without ever blocking the sender. The two actors
// here (the plugin goroutine and netDiscovery's mailbox goroutine) both use unbuffered mailboxes,
// so a direct blocking send in each direction can leave them waiting on each other forever. Post
// just appends to an unbounded queue; a dedicated goroutine does the potentially-blocking
// delivery, in order, so the only thing that can ever wait on the receiver is that goroutine.
type outbox struct {
	name  string
	queue *queue.RequestQueue[func()]
	wg    sync.WaitGroup
}

// newOutbox starts an outbox that delivers each posted closure with deliver, in posting order. A
// delivery error (typically the receiver having shut down) is logged and the closure dropped.
func newOutbox(name string, deliver func(func()) error) *outbox {
	deliveryCh := make(chan func())
	o := &outbox{name: name, queue: queue.NewRequestQueue[func()](deliveryCh)}

	o.wg.Go(o.queue.Run)
	o.wg.Go(func() {
		for f := range deliveryCh {
			if err := deliver(f); err != nil {
				fmt.Printf("netDiscovery: outbox %s: unable to deliver: %v\n", name, err)
			}
		}
	})

	return o
}

// Post queues f for delivery. Never blocks. Fails only once the outbox has been stopped.
func (o *outbox) Post(f func()) error {
	return o.queue.Enqueue(&f)
}

// Stop stops accepting new posts, then waits for everything already posted to be delivered.
func (o *outbox) Stop() {
	o.queue.Stop()
	o.wg.Wait()
}
