package lifecycle

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
)

// Daemon is responsible for handling lifecycle actions.
type Daemon struct {
	instanceID string
	topicArn   string

	log *logrus.Entry
}

// NewDaemon creates a new daemon for handling lifecycle events
func NewDaemon(instanceID, topicArn string, log *logrus.Entry) *Daemon {
	return &Daemon{
		instanceID: instanceID,
		topicArn:   topicArn,
		log:        log,
	}
}

// Start the daemon
func (d *Daemon) Start(ctx context.Context, queue *Queue) error {
	queueName := fmt.Sprintf("aws-lifecycle-%s", d.instanceID)

	// Create the SQS queue
	d.log.Infof("creating queue: %s", queueName)
	if err := queue.Create(queueName, d.topicArn); err != nil {
		return fmt.Errorf("failed to create queue: %s", err)
	}
	defer func() {
		d.log.Infof("deleting queue: %s", queueName)
		if err := queue.Delete(); err != nil {
			d.log.Errorf("failed to delete queue: %s", err)
		}
	}()

	// Subscribe to the STS topic
	d.log.Infof("subscribing to topic: %s", d.topicArn)
	if err := queue.Subscribe(d.topicArn); err != nil {
		return fmt.Errorf("failed to subscribe to the sns topic: %s", err)
	}
	defer func() {
		d.log.Infof("unsubscribing from topic: %s", d.topicArn)
		if err := queue.Unsubscribe(); err != nil {
			d.log.Errorf("failed to unsubscribe from sns topic: %s", err)
		}
	}()

	// Listen for new messages
	messages := make(chan *Message)
	go d.Listen(ctx, queue, messages)
	d.log.Info("started listener...")

	// Handle new messages
	d.Handle(queue, messages)
	return nil
}

// Listen for new messages
func (d *Daemon) Listen(ctx context.Context, queue *Queue, messages chan<- *Message) {
	defer close(messages)

Loop:
	for {
		select {
		case <-ctx.Done():
			d.log.Debug("stopping listener...")
			break Loop
		default:
			d.log.Debug("requesting new messages")
			out, err := queue.getMessage(ctx)
			if err != nil {
				d.log.Warnf("failed to get messages: %s", err)
			}
			for _, m := range out {
				messages <- m
			}
		}
	}
}

// Handle new messages
func (d *Daemon) Handle(queue *Queue, messages <-chan *Message) {
	for m := range messages {
		d.log.Debugf("got message: %s", m.ReceiptHandle)
		d.log.Debugf("message content: '%s'", m.Body)
		d.log.Debug("deleting message")
		if err := queue.deleteMessage(m.ReceiptHandle); err != nil {
			d.log.Warnf("failed to delete message: %s", err)
		}
	}
	d.log.Debug("stopping handler...")
}
