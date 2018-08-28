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

	queue *Queue
	log   *logrus.Entry
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
	d.log.Info("started listener...")

	// Handle new messages
	d.Handle(ctx, messages)
	return nil
}

// Listen for new messages
func (d *Daemon) Listen(ctx context.Context, messages chan<- *Message) {
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
				d.log.Warnf("failed to get messages: %s", err)
			}
			for _, m := range out {
				messages <- m
			}
		}
	}
}

// Handle new messages
func (d *Daemon) Handle(ctx context.Context, messages <-chan *Message) {
	for m := range messages {
		d.log.Debugf("got message: %s", m.ReceiptHandle)
		d.log.Debugf("message content: '%s'", m.Body)
		d.log.Debug("deleting message")
		if err := d.queue.deleteMessage(m.ReceiptHandle); err != nil {
			d.log.Warnf("failed to delete message: %s", err)
		}
	}
	d.log.Debug("stopping handler...")
}
