package lifecycle

import (
	"context"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
)

// Daemon is responsible for handling lifecycle actions.
type Daemon struct {
	instanceID string
	topicArn   string

	queue *Queue
	log   *logrus.Entry
	wg    sync.WaitGroup
}

// NewDaemon creates a new daemon for handling lifecycle events
func NewDaemon(instanceID, topicArn string, queue *Queue, log *logrus.Entry) *Daemon {
	return &Daemon{
		instanceID: instanceID,
		topicArn:   topicArn,
		queue:      queue,
		log:        log,
	}
}

// Start the daemon
func (d *Daemon) Start(ctx context.Context) error {
	queueName := fmt.Sprintf("aws-lifecycle-%s", d.instanceID)

	// Create the SQS queue
	d.log.Infof("creating queue: %s", queueName)
	if err := d.queue.Create(queueName, d.topicArn); err != nil {
		return fmt.Errorf("failed to create queue: %s", err)
	}
	defer func() {
		d.log.Infof("deleting queue: %s", queueName)
		if err := d.queue.Delete(); err != nil {
			d.log.WithError(err).Error("failed to delete queue")
		}
	}()

	// Subscribe to the STS topic
	d.log.Infof("subscribing to topic: %s", d.topicArn)
	if err := d.queue.Subscribe(d.topicArn); err != nil {
		return fmt.Errorf("failed to subscribe to the sns topic: %s", err)
	}
	defer func() {
		d.log.Infof("unsubscribing from topic: %s", d.topicArn)
		if err := d.queue.Unsubscribe(); err != nil {
			d.log.WithError(err).Error("failed to unsubscribe from sns topic")
		}
	}()

	// Listen for new messages
	messages := make(chan *Message)

	go d.Listen(ctx, messages)
	d.log.Debug("started listener...")
	d.wg.Add(1)

	go d.Process(ctx, messages)
	d.log.Debug("started handler...")
	d.wg.Add(1)

	// Wait until all processes have completed before returning
	d.wg.Wait()
	return nil
}

// Listen polls SQS for new messages
func (d *Daemon) Listen(ctx context.Context, messages chan<- *Message) {
	defer d.wg.Done()
	defer close(messages)

	for {
		select {
		case <-ctx.Done():
			d.log.Debug("stopping listener...")
			return
		default:
			d.log.Debug("requesting new messages")
			out, err := d.queue.getMessage(ctx)
			if err != nil {
				d.log.WithError(err).Warn("failed to get message")
			}
			for _, m := range out {
				messages <- m
				if err := d.queue.deleteMessage(m.ReceiptHandle); err != nil {
					d.log.WithError(err).Warn("failed to delete message")
				}
			}
		}
	}
}

// Process the SQS messages
func (d *Daemon) Process(ctx context.Context, messages <-chan *Message) {
	defer d.wg.Done()

	for m := range messages {
		if m.InstanceID != d.instanceID || m.Transition != "autoscaling:EC2_INSTANCE_TERMINATING" {
			d.log.Infof("skipping notice: %s with target: %s", m.Transition, m.InstanceID)
		}
		d.log.Debug("DO SOME STUFF!")
	}
	d.log.Debug("stopping handler...")
}
