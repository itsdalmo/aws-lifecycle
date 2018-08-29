package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/sirupsen/logrus"
)

// AutoscalingClient for testing purposes.
//go:generate mockgen -destination=mocks/mock_autoscaling_client.go -package=mocks github.com/telia-oss/aws-lifecycle AutoscalingClient
type AutoscalingClient autoscalingiface.AutoScalingAPI

// Daemon is responsible for handling lifecycle actions.
type Daemon struct {
	handler    string
	instanceID string
	topicArn   string

	autoscalingClient AutoscalingClient
	queue             *Queue
	log               *logrus.Entry
	wg                sync.WaitGroup
}

// NewDaemon creates a new daemon for handling lifecycle events
func NewDaemon(handler string, instanceID, topicArn string, autoscaling AutoscalingClient, queue *Queue, log *logrus.Entry) *Daemon {
	return &Daemon{
		handler:           handler,
		instanceID:        instanceID,
		topicArn:          topicArn,
		autoscalingClient: autoscaling,
		queue:             queue,
		log:               log,
	}
}

// Start the daemon
func (d *Daemon) Start(ctx context.Context) error {
	queueName := fmt.Sprintf("aws-lifecycle-%s", d.instanceID)

	// Create the SQS queue
	d.log.WithField("queueName", queueName).Info("creating queue")
	if err := d.queue.Create(queueName, d.topicArn); err != nil {
		return fmt.Errorf("failed to create queue: %s", err)
	}
	defer func() {
		d.log.WithField("queueName", queueName).Info("deleting queue")
		if err := d.queue.Delete(); err != nil {
			d.log.WithError(err).Error("failed to delete queue")
		}
	}()

	// Subscribe to the STS topic
	d.log.WithField("topicArn", d.topicArn).Info("subscribing to topic")
	if err := d.queue.Subscribe(d.topicArn); err != nil {
		return fmt.Errorf("failed to subscribe to the sns topic: %s", err)
	}
	defer func() {
		d.log.WithField("topicArn", d.topicArn).Info("unsubscribing from topic")
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
	d.log.Debug("started processor...")
	d.wg.Add(1)

	// Wait until all processes have completed before returning
	d.log.Info("Daemon is running!")
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
				if err := d.queue.deleteMessage(ctx, m.ReceiptHandle); err != nil {
					d.log.WithField("messageId", m.MessageID).WithError(err).Warn("failed to delete message")
				}
			}
		}
	}
}

// Process the SQS messages
func (d *Daemon) Process(ctx context.Context, messages <-chan *Message) {
	defer d.wg.Done()

	for m := range messages {
		log := d.log.WithField("messageId", m.MessageID)
		if m.InstanceID != d.instanceID {
			log.Debugf("ignoring notice: target instance id does not match: %s", m.InstanceID)
			continue
		}
		if m.Transition != "autoscaling:EC2_INSTANCE_TERMINATING" {
			log.Debugf("ignoring notice: not a termination notice: %s", m.Transition)
			continue
		}

		log.Info("received termination notice")

		// New context to stop heartbeating
		heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
		log.Info("starting heartbeat")
		go d.heartbeat(heartbeatCtx, m, log)

		log.Info("executing handler")
		if err := d.executeHandler(ctx, log); err != nil {
			log.WithError(err).Error("failed to execute handler")
		} else {
			log.Info("handler completed successfully")
		}
		cancelHeartbeat()
	}
	d.log.Debug("stopping processor...")
}

func (d *Daemon) executeHandler(ctx context.Context, log *logrus.Entry) error {
	cmd := exec.Command(d.handler)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	finished := make(chan error)
	go func() {
		defer close(finished)
		finished <- cmd.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				if err := cmd.Process.Signal(os.Interrupt); err != nil {
					log.WithError(err).Error("failed to interrupt execution of handler")
				}
			}
			return nil
		case err := <-finished:
			return err
		}
	}
}

func (d *Daemon) heartbeat(ctx context.Context, m *Message, log *logrus.Entry) {
	for {
		select {
		case <-ctx.Done():
			log.Debug("stopping heartbeat")
			return
		case <-time.NewTicker(10 * time.Second).C:
			log.Debug("sending heartbeat")
			_, err := d.autoscalingClient.RecordLifecycleActionHeartbeat(&autoscaling.RecordLifecycleActionHeartbeatInput{
				AutoScalingGroupName: aws.String(m.GroupName),
				LifecycleHookName:    aws.String(m.HookName),
				InstanceId:           aws.String(m.InstanceID),
				LifecycleActionToken: aws.String(m.ActionToken),
			})
			if err != nil {
				log.WithError(err).Error("failed to send heartbeat")
			}
		}
	}
}
